package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"agency-two/internal/agency/config"
	"agency-two/internal/agency/eventlog"
	"agency-two/internal/agency/provider"
)

func testDBPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "agency.db")
}

func TestMigrationCreatesSchemaV1(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	wantTables := []string{
		"schema_migrations", "config_sources", "config_revisions",
		"projects", "repositories", "project_repositories", "hosts",
		"agent_providers", "provider_models", "agent_profiles", "agent_profile_revisions",
		"agent_profile_current_revisions", "project_profile_defaults",
		"workspaces", "project_root_workspaces", "managed_worktrees", "active_worktrees", "closing_worktrees", "removed_worktrees",
		"tmux_servers", "tmux_targets", "agent_sessions", "session_tmux_targets",
		"agent_runs", "active_runner_bindings", "run_terminal_outcomes", "runner_output_chunks", "run_input_events",
		"status_subjects", "status_snapshots", "latest_status_snapshots",
		"safety_policies", "safety_check_runs", "safety_check_findings",
		"close_attempts", "close_blockers", "events",
		"notification_channels", "notification_deliveries", "idempotency_keys",
	}
	for _, table := range wantTables {
		var name string
		err := store.DB.QueryRowContext(ctx, "select name from sqlite_master where type='table' and name=?", table).Scan(&name)
		if err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}

	var version string
	if err := store.DB.QueryRowContext(ctx, "select version from schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "1" {
		t.Fatalf("schema version = %q, want 1", version)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
}

func TestOpenCreatesPrivateStateDirAndFile(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "agency.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("dir mode = %s, want drwx------", dirInfo.Mode().Perm())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %s, want -rw-------", info.Mode().Perm())
	}
}

func TestOpenRejectsWideStateDir(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, filepath.Join(dir, "agency.db"))
	if err == nil {
		store.Close()
		t.Fatal("Open succeeded for wide state dir")
	}
	if !strings.Contains(err.Error(), "wider than 0700") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenRejectsWideStateFile(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, path)
	if err == nil {
		store.Close()
		t.Fatal("Open succeeded for wide state file")
	}
	if !strings.Contains(err.Error(), "wider than 0600") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenReassertsWALForExistingSchemaV1(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(mode) != "delete" {
		t.Fatalf("journal_mode reset = %q, want delete", mode)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.DB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestSeededProviderCatalogMatchesProviderAdapter(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, cap := range provider.Catalog() {
		var displayName, command, rawConfig string
		if err := store.DB.QueryRowContext(ctx, `select display_name, command_path, provider_config_json from agent_providers where provider_key=?`, cap.ProviderKey).Scan(&displayName, &command, &rawConfig); err != nil {
			t.Fatal(err)
		}
		if displayName != cap.DisplayName || command != cap.Command {
			t.Fatalf("%s provider row = %q/%q", cap.ProviderKey, displayName, command)
		}
		var controls struct {
			PermissionModes  []string `json:"permissionModes"`
			SandboxModes     []string `json:"sandboxModes"`
			ApprovalPolicies []string `json:"approvalPolicies"`
		}
		if err := json.Unmarshal([]byte(rawConfig), &controls); err != nil {
			t.Fatal(err)
		}
		if strings.Join(controls.PermissionModes, ",") != strings.Join(cap.PermissionModes, ",") ||
			strings.Join(controls.SandboxModes, ",") != strings.Join(cap.SandboxModes, ",") ||
			strings.Join(controls.ApprovalPolicies, ",") != strings.Join(cap.ApprovalPolicies, ",") {
			t.Fatalf("%s provider controls = %+v, want %+v", cap.ProviderKey, controls, cap)
		}
		for _, model := range cap.Models {
			var rawSpec string
			if err := store.DB.QueryRowContext(ctx, `select model_spec_json from provider_models m join agent_providers p on p.provider_id=m.provider_id where p.provider_key=? and m.model_key=?`, cap.ProviderKey, model).Scan(&rawSpec); err != nil {
				t.Fatalf("%s/%s missing: %v", cap.ProviderKey, model, err)
			}
			var spec struct {
				Efforts []string `json:"efforts"`
			}
			if err := json.Unmarshal([]byte(rawSpec), &spec); err != nil {
				t.Fatal(err)
			}
			if strings.Join(spec.Efforts, ",") != strings.Join(cap.Efforts, ",") {
				t.Fatalf("%s/%s efforts = %+v, want %+v", cap.ProviderKey, model, spec.Efforts, cap.Efforts)
			}
		}
	}
}

func TestEffectiveConfigRevisionPersistsTypedSnapshot(t *testing.T) {
	ctx := context.Background()
	stateDB := testDBPath(t)
	store, err := Open(ctx, stateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	root := filepath.Join(t.TempDir(), "agency-two")
	project, err := store.EnsureProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHostAccess(ctx, "devbox", "ssh:devbox"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectDefaultHost(ctx, project.Key, "devbox"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectDefaultWorktreeMode(ctx, project.Key, "always"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEffectiveConfigRevision(ctx, stateDB, config.Default()); err != nil {
		t.Fatal(err)
	}

	var sources, revisions int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from config_sources where source_key='cli' and path='agency config set'`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from config_revisions`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || revisions != 1 {
		t.Fatalf("sources=%d revisions=%d, want 1/1", sources, revisions)
	}

	effective, err := store.LatestEffectiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projects := effective["projects"].(map[string]any)
	projectConfig := projects[project.Key].(map[string]any)
	if projectConfig["defaultHost"] != "devbox" || projectConfig["worktreeMode"] != "Always" || projectConfig["root"] != root {
		t.Fatalf("project effective config = %+v", projectConfig)
	}
	hosts := effective["hosts"].(map[string]any)
	access := hosts["devbox"].(map[string]any)["access"].(map[string]any)
	if access["mode"] != "Ssh" || access["hostAlias"] != "devbox" {
		t.Fatalf("host effective config = %+v", access)
	}
	defaults := effective["defaults"].(map[string]any)
	if defaults["project"] != project.Key || defaults["host"] != "devbox" || defaults["worktreeMode"] != "Always" {
		t.Fatalf("defaults effective config = %+v", defaults)
	}
}

func TestEventsCarryCorrelationId(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	// BindRunner emits RunnerHeartbeatAccepted on the run subject.
	var correlation string
	if err := store.DB.QueryRowContext(ctx, `select correlation_json from events where event_type='RunnerHeartbeatAccepted'`).Scan(&correlation); err != nil {
		t.Fatal(err)
	}
	if correlation == "" || correlation == "{}" {
		t.Fatalf("event correlation is empty: %q", correlation)
	}
	if !strings.Contains(correlation, run.RunID) {
		t.Fatalf("event correlation %q does not carry run id %q", correlation, run.RunID)
	}
}

func TestRunStoppedLinksCausationToStopRequested(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.StopRun(ctx, run.Session); err != nil {
		t.Fatal(err)
	}
	var requestedID, causation string
	if err := store.DB.QueryRowContext(ctx, `select event_id from events where event_type='StopRequested'`).Scan(&requestedID); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select coalesce(causation_event_id, '') from events where event_type='RunStopped'`).Scan(&causation); err != nil {
		t.Fatal(err)
	}
	if causation == "" || causation != requestedID {
		t.Fatalf("RunStopped causation = %q, want StopRequested id %q", causation, requestedID)
	}
}

func TestPruneRemovesPastRetentionRowsOnly(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// One completed idempotency key backdated past retention, one recent.
	for _, key := range []string{"old-key", "recent-key"} {
		if _, _, err := store.BeginIdempotency(ctx, key, "op", "hash-"+key); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteIdempotency(ctx, key, "op", "hash-"+key, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02T15:04:05.000Z")
	if _, err := store.DB.ExecContext(ctx, `update idempotency_keys set completed_at=? where replay_key='old-key'`, old); err != nil {
		t.Fatal(err)
	}

	result, err := store.Prune(ctx, config.Retention{IdempotencyRetentionDays: 7, EventRetentionDays: 90, OutputRetentionDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	if result.IdempotencyKeys != 1 {
		t.Fatalf("pruned idempotency keys = %d, want 1", result.IdempotencyKeys)
	}
	var remaining int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from idempotency_keys where replay_key='recent-key'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("recent idempotency key was pruned")
	}
	var gone int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from idempotency_keys where replay_key='old-key'`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if gone != 0 {
		t.Fatalf("old idempotency key survived retention")
	}
}

func TestPruneAgesOutTerminalRunEventsOnly(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Two runs in one project (workspace_key 'project_root' is globally unique,
	// so seedStorageRun cannot be called twice).
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(ctx, project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	mkRun := func(title string) CreatedRun {
		run, err := store.CreateRunIntent(ctx, LaunchIntent{
			ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
			Title: title, WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	terminal := mkRun("terminal")
	live := mkRun("live")

	// Emit events on both runs; make only the first terminal.
	if err := store.RecordStopRequested(ctx, terminal.RunID, terminal.Session); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(ctx, terminal.RunID, "UserStopped", `{"outcome":"UserStopped"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStopRequested(ctx, live.RunID, live.Session); err != nil {
		t.Fatal(err)
	}

	// Backdate every event well past the retention horizon.
	old := time.Now().UTC().AddDate(0, 0, -200).Format("2006-01-02T15:04:05.000Z")
	if _, err := store.DB.ExecContext(ctx, `update events set occurred_at=?`, old); err != nil {
		t.Fatal(err)
	}
	countRun := func(runID string) int {
		var n int
		if err := store.DB.QueryRowContext(ctx, `select count(*) from events e
			join status_subjects s on s.status_subject_id=e.status_subject_id
			where s.subject_hash='Run:'||?`, runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	terminalBefore := countRun(terminal.RunID)
	liveBefore := countRun(live.RunID)
	if terminalBefore == 0 || liveBefore == 0 {
		t.Fatalf("expected events on both runs, got terminal=%d live=%d", terminalBefore, liveBefore)
	}

	// Causation parents are held back one round (a surviving child still names
	// them), so convergence to zero can take multiple sweeps — the documented
	// self-healing behavior. Prune repeatedly until stable.
	totalPruned := 0
	for i := 0; i < 5; i++ {
		result, err := store.Prune(ctx, config.Retention{IdempotencyRetentionDays: 7, EventRetentionDays: 90, OutputRetentionDays: 14, StatusSnapshotWindow: 50})
		if err != nil {
			t.Fatal(err)
		}
		totalPruned += result.Events
		if countRun(terminal.RunID) == 0 {
			break
		}
	}
	if got := countRun(terminal.RunID); got != 0 {
		t.Fatalf("terminal-run events did not age out after repeated prunes: %d", got)
	}
	if totalPruned != terminalBefore {
		t.Fatalf("total pruned events = %d, want %d", totalPruned, terminalBefore)
	}
	if got := countRun(live.RunID); got != liveBefore {
		t.Fatalf("live-run events were pruned: before=%d after=%d", liveBefore, got)
	}
}

func TestPruneTrimsStatusSnapshotsToWindow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)

	// CreateRunIntent already created the run's status subject; reuse it and add
	// eight historical snapshots plus a latest pointer, via raw inserts so the
	// window count is exact.
	var subjectID string
	if err := store.DB.QueryRowContext(ctx, `select status_subject_id from status_subjects where subject_hash=?`, "Run:"+run.RunID).Scan(&subjectID); err != nil {
		t.Fatal(err)
	}
	var newestID string
	for i := 0; i < 8; i++ {
		id := NewID()
		newestID = id
		captured := time.Now().UTC().Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05.000Z")
		if _, err := store.DB.ExecContext(ctx, `insert into status_snapshots(status_snapshot_id, status_subject_id, captured_at, source, snapshot_hash, snapshot_json)
			values(?, ?, ?, 'Event', ?, ?)`, id, subjectID, captured, "hash-"+id, `{"runStatus":"Live"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB.ExecContext(ctx, `insert into latest_status_snapshots(status_subject_id, status_snapshot_id, updated_at) values(?, ?, ?)`,
		subjectID, newestID, Now()); err != nil {
		t.Fatal(err)
	}

	result, err := store.Prune(ctx, config.Retention{IdempotencyRetentionDays: 7, EventRetentionDays: 90, OutputRetentionDays: 14, StatusSnapshotWindow: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusSnapshots != 5 {
		t.Fatalf("pruned snapshots = %d, want 5 (8 - window 3)", result.StatusSnapshots)
	}
	var remaining int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from status_snapshots where status_subject_id=?`, subjectID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 3 {
		t.Fatalf("remaining snapshots = %d, want 3", remaining)
	}
	// The latest pointer must still resolve to a retained row.
	var latestKept int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from latest_status_snapshots l join status_snapshots s on s.status_snapshot_id=l.status_snapshot_id where l.status_subject_id=?`, subjectID).Scan(&latestKept); err != nil {
		t.Fatal(err)
	}
	if latestKept != 1 {
		t.Fatalf("latest snapshot pointer dangling after prune")
	}
}

func TestSyncNotificationChannelsUpsertsAndDisables(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Configure a Desktop channel in addition to the seeded terminal one.
	if err := store.SyncNotificationChannels(ctx, map[string]config.Notification{
		"terminal": {Type: "Terminal", Events: []string{"RunFailed"}},
		"desktop":  {Type: "Desktop", Events: []string{"RunNeedsApproval"}},
	}); err != nil {
		t.Fatal(err)
	}
	enabled := func() map[string]string {
		rows, err := store.DB.QueryContext(ctx, `select notification_channel_key, channel_type from notification_channels where disabled_at is null`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var k, ty string
			if err := rows.Scan(&k, &ty); err != nil {
				t.Fatal(err)
			}
			got[k] = ty
		}
		return got
	}
	got := enabled()
	if got["desktop"] != "Desktop" || got["terminal"] != "Terminal" {
		t.Fatalf("after sync, enabled channels = %+v, want terminal+desktop", got)
	}

	// Dropping desktop from config soft-disables it; terminal survives.
	if err := store.SyncNotificationChannels(ctx, map[string]config.Notification{
		"terminal": {Type: "Terminal", Events: []string{"RunFailed"}},
	}); err != nil {
		t.Fatal(err)
	}
	got = enabled()
	if _, ok := got["desktop"]; ok {
		t.Fatalf("desktop channel should be disabled after removal from config: %+v", got)
	}
	if got["terminal"] != "Terminal" {
		t.Fatalf("terminal channel should remain enabled: %+v", got)
	}
	// The disabled desktop row is retained (soft-disable), not deleted.
	var total int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from notification_channels where notification_channel_key='desktop'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("desktop channel row count = %d, want 1 (soft-disabled, not deleted)", total)
	}
}

func TestBeginWorktreeRemovalRechecksLiveSessions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	project, err := store.EnsureProject(ctx, filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := store.AddManagedWorkspace(ctx, project.ID, "wt_race", filepath.Join(project.ManagedWorktreeRoot, "wt_race"), "agency/wt_race", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(ctx, project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunIntent(ctx, LaunchIntent{
		ProjectID: project.ID, WorkspaceID: workspaceID, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Race run", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	wt, err := store.ActiveManagedWorktree(ctx, "wt_race")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWorktreeRemoval(ctx, wt); err == nil || err.Error() != "SessionStillRunning" {
		t.Fatalf("BeginWorktreeRemoval error = %v, want SessionStillRunning", err)
	}
	var active int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from active_worktrees`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active worktrees = %d, want 1", active)
	}
}

func TestExportWritesReadableDatabase(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	dir := filepath.Dir(path)
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureProject(ctx, filepath.Join(dir, "repo")); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(dir, "export.db")
	if err := store.Export(ctx, exportPath); err != nil {
		t.Fatal(err)
	}
	if err := store.Export(ctx, exportPath); err == nil {
		t.Fatal("second export overwrote existing file")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	exported, err := Open(ctx, exportPath)
	if err != nil {
		t.Fatal(err)
	}
	defer exported.Close()
	projects, err := exported.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Key != "repo" {
		t.Fatalf("exported projects = %+v", projects)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.DB.ExecContext(ctx, `insert into project_repositories(project_repository_id, project_id, repository_id, role, created_at)
		values(?, 'missing', 'missing', 'Primary', ?)`, NewID(), Now())
	if err == nil {
		t.Fatal("bad child insert succeeded with foreign_keys enabled")
	}
}

func TestUnknownSchemaVersionFailsStartup(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `create table schema_migrations(version text primary key, applied_at timestamp not null);
		insert into schema_migrations(version, applied_at) values('999', '2026-07-01T00:00:00.000Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open succeeded with unknown schema version")
	}
}

func TestEnsureProjectCreatesRootWorkspace(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	root := filepath.Join(t.TempDir(), "agency-two")
	project, err := store.EnsureProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if project.Key != "agency_two" {
		t.Fatalf("project key = %q, want agency_two", project.Key)
	}
	if project.ProjectRootWorkspace == "" {
		t.Fatal("project root workspace was not recorded")
	}
	again, err := store.EnsureProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != project.ID {
		t.Fatalf("EnsureProject created duplicate project: %s then %s", project.ID, again.ID)
	}
}

func TestRecordWorkspaceSafetyCheckPersistsFindings(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkID, err := store.RecordWorkspaceSafetyCheck(ctx, project.ProjectRootWorkspace, "WorktreeClose", []SafetyFinding{{
		Blocker: "WorktreeDirty", Severity: "Blocker", Message: "Tracked changes block worktree close.",
		Location: map[string]any{"workspace": "project_root"},
		Evidence: map[string]any{"gitSummary": "Dirty"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var kind, blocker, severity, message, location, evidence string
	if err := store.DB.QueryRowContext(ctx, `select r.check_kind, f.blocker, f.severity, f.message, f.location_json, f.evidence_json
		from safety_check_runs r
		join safety_check_findings f on f.safety_check_run_id=r.safety_check_run_id
		where r.safety_check_run_id=?`, checkID).Scan(&kind, &blocker, &severity, &message, &location, &evidence); err != nil {
		t.Fatal(err)
	}
	if kind != "WorktreeClose" || blocker != "WorktreeDirty" || severity != "Blocker" || message == "" {
		t.Fatalf("safety row = kind=%q blocker=%q severity=%q message=%q", kind, blocker, severity, message)
	}
	if !json.Valid([]byte(location)) || !json.Valid([]byte(evidence)) {
		t.Fatalf("invalid json location=%s evidence=%s", location, evidence)
	}
}

func TestInputEventStoresBase64ContentRef(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(ctx, project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(ctx, LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Input test", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddInputEvent(ctx, run.RunID, []byte("hello\x00runner\n")); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := store.DB.QueryRowContext(ctx, `select input_ref_json from run_input_events where run_id=?`, run.RunID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Kind        string `json:"kind"`
		BytesBase64 string `json:"bytesBase64"`
	}
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(ref.BytesBase64)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Kind != "Inline" || string(decoded) != "hello\x00runner\n" {
		t.Fatalf("input ref = %+v decoded=%q", ref, string(decoded))
	}
}

func TestConcurrentInputEventsAllocateOrderedSequences(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	run := seedStorageRun(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	seqs := make([]int, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store, err := Open(ctx, path)
			if err != nil {
				errs[i] = err
				return
			}
			defer store.Close()
			seqs[i], errs[i] = store.AddInputEvent(ctx, run.RunID, []byte("hello\n"))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Ints(seqs)
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("seqs = %+v", seqs)
		}
	}
}

func TestListSessionEventsReturnsLifecycleTimeline(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	seq, err := store.AddInputEvent(ctx, run.RunID, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInputAccepted(ctx, run.RunID, seq); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(ctx, run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(ctx, run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListSessionEvents(ctx, run.Session)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	subjects := map[string]string{}
	seqs := map[string]int{}
	sessionSubject := map[string]any{}
	for _, event := range events {
		counts[event.EventType]++
		subjects[event.EventType] = event.Subject["kind"].(string)
		seqs[event.EventType] = event.EventSeq
		if event.EventType == string(eventlog.SessionCreated) {
			sessionSubject = event.Subject
		}
		if event.Subject["sessionId"] != nil || event.Subject["runId"] != nil {
			t.Fatalf("event subject leaked private ids: %+v", event.Subject)
		}
		if event.Label == "" {
			t.Fatalf("missing label for %+v", event)
		}
	}
	for _, want := range []eventlog.EventType{
		eventlog.SessionCreated,
		eventlog.RunRequested,
		eventlog.InputAccepted,
		eventlog.ProviderProcessExited,
	} {
		if counts[string(want)] != 1 {
			t.Fatalf("%s count = %d, events = %+v", want, counts[string(want)], events)
		}
	}
	if subjects[string(eventlog.SessionCreated)] != "Session" || subjects[string(eventlog.ProviderProcessExited)] != "Run" {
		t.Fatalf("subjects = %+v", subjects)
	}
	if sessionSubject["session"] != run.Session {
		t.Fatalf("session subject = %+v", sessionSubject)
	}
	if seqs[string(eventlog.SessionCreated)] != 1 ||
		seqs[string(eventlog.RunRequested)] != 1 ||
		seqs[string(eventlog.InputAccepted)] != 2 ||
		seqs[string(eventlog.ProviderProcessExited)] != 3 {
		t.Fatalf("event seqs = %+v", seqs)
	}
}

func TestTerminalOutcomeRejectsInvalidPayload(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	for _, tc := range []struct {
		name    string
		outcome string
		body    string
		want    string
	}{
		{"unknown", "Bogus", `{"outcome":"Bogus"}`, "unknown terminal outcome"},
		{"json", "ProviderExited", `{`, "unexpected end of JSON input"},
		{"mismatch", "ProviderExited", `{"outcome":"UserStopped"}`, "TerminalOutcomePayloadMismatch"},
	} {
		err := store.RecordTerminalOutcome(ctx, run.RunID, tc.outcome, tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s error = %v, want %s", tc.name, err, tc.want)
		}
	}
	var outcomes int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from run_terminal_outcomes`).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != 0 {
		t.Fatalf("terminal outcomes = %d, want 0", outcomes)
	}
}

func TestTerminalOutcomeRejectsConflictingOutcome(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.RecordTerminalOutcome(ctx, run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(ctx, run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	err = store.RecordTerminalOutcome(ctx, run.RunID, "UserKilled", `{"outcome":"UserKilled","signal":"SIGKILL"}`)
	if err == nil || err.Error() != "TerminalOutcomeConflict" {
		t.Fatalf("conflicting terminal outcome error = %v, want TerminalOutcomeConflict", err)
	}
	var outcomes, events int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from run_terminal_outcomes`).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from events where event_type='ProviderProcessExited'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if outcomes != 1 || events != 1 {
		t.Fatalf("outcomes=%d events=%d, want 1/1", outcomes, events)
	}
}

func TestBindRunnerRejectsTerminalRun(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.RecordTerminalOutcome(ctx, run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	err = store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock"))
	if err == nil || err.Error() != "RunAlreadyTerminal" {
		t.Fatalf("BindRunner error = %v, want RunAlreadyTerminal", err)
	}
}

func TestStartFailedRetiresTmuxTarget(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.MarkStartFailed(ctx, run.RunID, "runner control socket did not appear"); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Failed" {
		t.Fatalf("sessions = %+v", sessions)
	}
	var activeTargets, retiredTargets int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from session_tmux_targets where detached_at is null`).Scan(&activeTargets); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from tmux_targets where retired_at is not null`).Scan(&retiredTargets); err != nil {
		t.Fatal(err)
	}
	if activeTargets != 0 || retiredTargets != 1 {
		t.Fatalf("activeTargets=%d retiredTargets=%d, want 0/1", activeTargets, retiredTargets)
	}
	detail, err := store.Session(ctx, run.Session)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TmuxTargetKey != "" || detail.RunnerSocket != "" {
		t.Fatalf("detail target/socket = %q/%q, want empty", detail.TmuxTargetKey, detail.RunnerSocket)
	}
}

func TestTmuxServerRestartOrphansActiveRuns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTmuxServerIdentity(ctx, "default", `{"pid":1,"startedAt":1}`); err != nil {
		t.Fatal(err)
	}
	orphaned, err := store.MarkTmuxServerRestarted(ctx, "default", `{"pid":2,"startedAt":2}`, "tmux server default identity changed")
	if err != nil {
		t.Fatal(err)
	}
	if orphaned != 1 {
		t.Fatalf("orphaned = %d, want 1", orphaned)
	}
	identity, ok, err := store.TmuxServerIdentity(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || identity != `{"pid":2,"startedAt":2}` {
		t.Fatalf("identity = %q ok=%v", identity, ok)
	}
	bindings, err := store.ListActiveRunnerBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %+v, want none", bindings)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Failed" {
		t.Fatalf("sessions = %+v", sessions)
	}
	var outcome, activeTargets, retiredTargets, restartEvents int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from run_terminal_outcomes where run_id=? and outcome='Orphaned'`, run.RunID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from session_tmux_targets where detached_at is null`).Scan(&activeTargets); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from tmux_targets where retired_at is not null`).Scan(&retiredTargets); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from events where event_type='TmuxServerRestarted'`).Scan(&restartEvents); err != nil {
		t.Fatal(err)
	}
	if outcome != 1 || activeTargets != 0 || retiredTargets != 1 || restartEvents != 1 {
		t.Fatalf("outcome=%d activeTargets=%d retiredTargets=%d restartEvents=%d, want 1/0/1/1", outcome, activeTargets, retiredTargets, restartEvents)
	}
	orphaned, err = store.MarkTmuxServerRestarted(ctx, "default", `{"pid":2,"startedAt":2}`, "tmux server default identity changed")
	if err != nil {
		t.Fatal(err)
	}
	if orphaned != 0 {
		t.Fatalf("second orphaned = %d, want 0", orphaned)
	}
}

func TestRunnerQuarantineRemovesControlBinding(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunnerQuarantined(ctx, run.RunID, 2, "future", "runner protocol version is not supported"); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListActiveRunnerBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %+v, want none", bindings)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "RepairRequired" {
		t.Fatalf("sessions = %+v", sessions)
	}
	detail, err := store.Session(ctx, run.Session)
	if err != nil {
		t.Fatal(err)
	}
	if detail.RunnerSocket != "" {
		t.Fatalf("RunnerSocket = %q, want empty", detail.RunnerSocket)
	}
}

func TestDeleteUnpublishedManagedWorkspaceDeletesOnlyReservations(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := store.AddManagedWorkspace(ctx, project.ID, "reserved", filepath.Join(project.ManagedWorktreeRoot, "reserved"), "agency/reserved", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUnpublishedManagedWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	var workspaces int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from workspaces where workspace_id=?`, workspaceID).Scan(&workspaces); err != nil {
		t.Fatal(err)
	}
	if workspaces != 0 {
		t.Fatalf("reserved workspace rows = %d, want 0", workspaces)
	}
	workspaceID, err = store.AddManagedWorkspace(ctx, project.ID, "published", filepath.Join(project.ManagedWorktreeRoot, "published"), "agency/published", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	err = store.DeleteUnpublishedManagedWorkspace(ctx, workspaceID)
	if err == nil || err.Error() != "ManagedWorktreePublished" {
		t.Fatalf("DeleteUnpublishedManagedWorkspace error = %v, want ManagedWorktreePublished", err)
	}
}

func TestSelectedEventsSurfaceAsPendingNotifications(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.StopRun(ctx, run.Session); err != nil {
		t.Fatal(err)
	}
	// Delivery is decoupled from the event write; RunStopped must surface as a
	// pending terminal notification for the worker, not a synchronous delivery.
	pending, err := store.PendingNotifications(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range pending {
		if n.ChannelKey == "terminal" && n.EventType == "RunStopped" {
			if n.AttemptSeq != 1 || n.SessionKey != run.Session {
				t.Fatalf("pending notification = %+v", n)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("RunStopped not pending for terminal channel: %+v", pending)
	}
	// After the worker records delivery, it is no longer pending.
	var channelID string
	if err := store.DB.QueryRowContext(ctx, `select notification_channel_id from notification_channels where notification_channel_key='terminal'`).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := store.DB.QueryRowContext(ctx, `select event_id from events where event_type='RunStopped'`).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotificationDelivered(ctx, channelID, eventID, 1); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingNotifications(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range pending {
		if n.EventType == "RunStopped" {
			t.Fatalf("RunStopped still pending after delivery: %+v", n)
		}
	}
}

func TestOutputChunksAreIdempotentAndBase64Backed(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	next, err := store.NextOutputChunkSeq(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if next != 1 {
		t.Fatalf("next seq = %d, want 1", next)
	}
	if err := store.AppendOutputChunk(ctx, run.RunID, 1, "Stdout", []byte("hello\x00runner")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendOutputChunk(ctx, run.RunID, 1, "Stdout", []byte("duplicate")); err != nil {
		t.Fatal(err)
	}
	next, err = store.NextOutputChunkSeq(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Fatalf("next seq = %d, want 2", next)
	}
	chunks, err := store.ListOutputChunks(ctx, run.RunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].ChunkSeq != 1 || chunks[0].Stream != "Stdout" || string(chunks[0].Bytes) != "hello\x00runner" {
		t.Fatalf("chunks = %+v", chunks)
	}
	var raw string
	if err := store.DB.QueryRowContext(ctx, `select content_ref_json from runner_output_chunks where run_id=?`, run.RunID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ref struct {
		BytesBase64 string `json:"bytesBase64"`
	}
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(ref.BytesBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "hello\x00runner" {
		t.Fatalf("decoded = %q", string(decoded))
	}
}

func TestOutputChunksRejectInvalidStream(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	for _, stream := range []string{"stdout", "Bogus", ""} {
		err := store.AppendOutputChunk(ctx, run.RunID, 1, stream, []byte("recent\n"))
		if err == nil || err.Error() != "InvalidOutputStream" {
			t.Fatalf("stream %q error = %v, want InvalidOutputStream", stream, err)
		}
	}
}

func TestStopRunRequiresLiveBinding(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.StopRun(ctx, run.Session); err == nil || err.Error() != "RunNotLive" {
		t.Fatalf("StopRun error = %v, want RunNotLive", err)
	}
}

func TestCloseSessionBlocksActivePromptStatuses(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRunStatusSnapshot(ctx, tx, run.RunID, "Event", `{"runStatus":"NeedsInput"}`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "NeedsInput" || sessions[0].Close["summary"] != "StopRequired" || sessions[0].Close["closable"] != false {
		t.Fatalf("sessions = %+v", sessions)
	}
	if err := store.CloseSession(ctx, run.Session); err == nil || err.Error() != "SessionStillRunning" {
		t.Fatalf("CloseSession error = %v, want SessionStillRunning", err)
	}
	var blockers int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from close_blockers where blocker='SessionStillRunning'`).Scan(&blockers); err != nil {
		t.Fatal(err)
	}
	if blockers != 1 {
		t.Fatalf("SessionStillRunning blockers = %d, want 1", blockers)
	}
}

func TestStatusSnapshotsAreWrittenOnlyWhenHashChanges(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRunStatusSnapshot(ctx, tx, run.RunID, "Event", `{"runStatus":"NeedsInput"}`); err != nil {
		t.Fatal(err)
	}
	if err := insertRunStatusSnapshot(ctx, tx, run.RunID, "Event", `{"runStatus":"NeedsInput"}`); err != nil {
		t.Fatal(err)
	}
	if err := insertRunStatusSnapshot(ctx, tx, run.RunID, "Event", `{"runStatus":"NeedsApproval"}`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var snapshots int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from status_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 2 {
		t.Fatalf("snapshots = %d, want 2", snapshots)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "NeedsApproval" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestStatusSnapshotRejectsInvalidShape(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, tc := range []struct {
		name   string
		source string
		body   string
		want   string
	}{
		{"source", "Manual", `{"runStatus":"Live"}`, "InvalidStatusSnapshotSource"},
		{"json", "Event", `{`, "unexpected end of JSON input"},
		{"status", "Event", `{"runStatus":"Bogus"}`, "InvalidRunStatus"},
	} {
		err := insertRunStatusSnapshot(ctx, tx, run.RunID, tc.source, tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s error = %v, want %s", tc.name, err, tc.want)
		}
	}
}

func TestPromptHintsRecordStatusEventsAndNotifications(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPromptHint(ctx, run.RunID, "NeedsApproval", "approval required"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPromptHint(ctx, run.RunID, "NeedsApproval", "approval required again"); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "NeedsApproval" {
		t.Fatalf("sessions = %+v", sessions)
	}
	var events int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from events where event_type='RunNeedsApproval'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events=%d, want 1", events)
	}
	pending, err := store.PendingNotifications(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	approvalPending := false
	for _, n := range pending {
		if n.ChannelKey == "terminal" && n.EventType == "RunNeedsApproval" {
			approvalPending = true
		}
	}
	if !approvalPending {
		t.Fatalf("RunNeedsApproval not pending for terminal channel: %+v", pending)
	}
	seq, err := store.AddInputEvent(ctx, run.RunID, []byte("yes\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInputAccepted(ctx, run.RunID, seq); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Live" {
		t.Fatalf("sessions after input = %+v", sessions)
	}
}

func TestClearPromptHintUnsticksResolvedPrompt(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	// A prompt is detected, pinning the run to NeedsInput.
	if err := store.RecordPromptHint(ctx, run.RunID, "NeedsInput", "type your message"); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "NeedsInput" {
		t.Fatalf("sessions = %+v, want NeedsInput", sessions)
	}
	// The prompt resolves outside agency (direct pane interaction / timeout); the
	// pane no longer shows it. Clearing must revert to the derived Live status,
	// not stick on NeedsInput forever.
	if err := store.ClearPromptHint(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Live" {
		t.Fatalf("sessions after clear = %+v, want Live", sessions)
	}
}

func TestClearPromptHintLeavesNonPromptSnapshotUntouched(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	// A RepairRequired snapshot must survive a clear: ClearPromptHint only
	// un-pins prompt states, never a reconciliation verdict.
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRunStatusSnapshot(ctx, tx, run.RunID, "Reconciliation", `{"runStatus":"RepairRequired"}`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearPromptHint(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "RepairRequired" {
		t.Fatalf("sessions after clear = %+v, want RepairRequired preserved", sessions)
	}
}

func TestLiveStatusUsesQuietAndRecentOutput(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := seedStorageRun(t, store)
	if err := store.BindRunner(ctx, run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	// A live binding with no recent output and an old bind time projects to
	// Quiet. Heartbeat liveness (LostRunner) is a supervisor monotonic-clock
	// projection, not a store decision.
	old := time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000Z")
	if _, err := store.DB.ExecContext(ctx, `update active_runner_bindings set bound_at=? where run_id=?`, old, run.RunID); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Quiet" {
		t.Fatalf("quiet sessions = %+v", sessions)
	}
	// Recent output flips the projection to Live.
	if err := store.AppendOutputChunk(ctx, run.RunID, 1, "Stdout", []byte("recent\n")); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Live" {
		t.Fatalf("live sessions = %+v", sessions)
	}
	// A stale wall-clock heartbeat must NOT make the store report LostRunner;
	// that determination belongs to the supervisor's monotonic clock.
	stale := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	if _, err := store.DB.ExecContext(ctx, `update active_runner_bindings set last_heartbeat_at=? where run_id=?`, stale, run.RunID); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus == "LostRunner" {
		t.Fatalf("store must not decide LostRunner from wall-clock: %+v", sessions)
	}
}

func TestAdditionalClaudeRunUsesContinue(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.ProfileRevision(ctx, "claude_default")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateRunIntent(ctx, LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Claude resume", WorkingDirectory: project.RootPath, Argv: []string{"claude", "--model", "sonnet"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateAdditionalRunFromPrevious(ctx, first.Session)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--continue", "--model", "sonnet"}
	if !reflect.DeepEqual(second.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", second.Argv, want)
	}
}

func TestRunCreationRejectsClosingManagedWorkspace(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project.ManagedWorktreeRoot, "closing")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := store.AddManagedWorkspace(ctx, project.ID, "closing", path, "agency/closing", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(ctx, project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateRunIntent(ctx, LaunchIntent{
		ProjectID: project.ID, WorkspaceID: workspaceID, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Closing run", WorkingDirectory: path, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(ctx, first.RunID, "UserStopped", `{"outcome":"UserStopped"}`); err != nil {
		t.Fatal(err)
	}
	wt, err := store.ActiveManagedWorktree(ctx, "closing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWorktreeRemoval(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunIntent(ctx, LaunchIntent{
		ProjectID: project.ID, WorkspaceID: workspaceID, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Blocked run", WorkingDirectory: path, Argv: []string{"codex"}, Env: map[string]any{},
	}); err == nil || err.Error() != "WorkspaceClosing" {
		t.Fatalf("CreateRunIntent error = %v, want WorkspaceClosing", err)
	}
	if _, err := store.CreateAdditionalRunFromPrevious(ctx, first.Session); err == nil || err.Error() != "WorkspaceClosing" {
		t.Fatalf("CreateAdditionalRunFromPrevious error = %v, want WorkspaceClosing", err)
	}
}

func seedStorageRun(t *testing.T, store *Store) CreatedRun {
	t.Helper()
	ctx := context.Background()
	project, err := store.EnsureProject(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(ctx, project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(ctx, LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Storage run", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}
