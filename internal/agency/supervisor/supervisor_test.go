package supervisor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agency-two/internal/agency/runner"
	"agency-two/internal/agency/runnerproto"
	"agency-two/internal/agency/storage"
)

func TestSupervisorHealthAndListSessions(t *testing.T) {
	cfg := testConfig(t)
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, provider, model, effort, policy, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if provider != "codex" || model == "" || effort == "" || policy == "" {
		t.Fatalf("bad seeded profile: %s %s %s %s", provider, model, effort, policy)
	}
	if _, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Supervisor test", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	health, err := HealthCheck(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != "Live" || health.Sessions != 1 || health.DBPath != cfg.StateDB {
		t.Fatalf("health = %+v", health)
	}
	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Title != "Supervisor test" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestSupervisorSessionOutputAPI(t *testing.T) {
	cfg := testConfig(t)
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Output API", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendOutputChunk(context.Background(), run.RunID, 1, "Stdout", []byte("api output\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	chunks, err := SessionOutput(context.Background(), cfg.SocketPath, run.Session, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || string(chunks[0].Bytes) != "api output\n" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestSupervisorSingletonLock(t *testing.T) {
	cfg := testConfig(t)
	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	err := Serve(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "SupervisorAlreadyRunning") {
		t.Fatalf("Serve second supervisor error = %v, want SupervisorAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), "pid=") {
		t.Fatalf("Serve second supervisor error = %v, want holding pid", err)
	}
}

func TestSupervisorLockMetadataIsTruncated(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.LockPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.LockPath, []byte("stale metadata that should be removed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cancel := startTestSupervisor(t, cfg)
	defer cancel()
	raw, err := os.ReadFile(cfg.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stale") {
		t.Fatalf("lock metadata was not truncated:\n%s", raw)
	}
	if !strings.Contains(string(raw), "pid=") {
		t.Fatalf("lock metadata missing pid:\n%s", raw)
	}
}

func TestSupervisorRemovesStaleSocket(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cancel := startTestSupervisor(t, cfg)
	defer cancel()
	if _, err := HealthCheck(context.Background(), cfg.SocketPath); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorAdoptsCompatibleRunnerOnStartup(t *testing.T) {
	cfg := testConfig(t)
	runID := seedBoundRun(t, cfg, filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock"))
	stopRunner := serveRunnerPreamble(t, filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock"), runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion,
		RunnerBinaryVersion:   "test-runner",
		RunID:                 runID,
	})
	defer stopRunner()

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	health, err := HealthCheck(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if health.AdoptedRunners != 1 || health.QuarantinedRunners != 0 || health.OrphanedRunners != 0 || health.LastReconcileAt == "" {
		t.Fatalf("health = %+v", health)
	}
	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "LostTmuxTarget" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestSupervisorQuarantinesIncompatibleRunnerOnStartup(t *testing.T) {
	cfg := testConfig(t)
	socket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock")
	runID := seedBoundRun(t, cfg, socket)
	stopRunner := serveRunnerPreamble(t, socket, runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion + 1,
		RunnerBinaryVersion:   "future-runner",
		RunID:                 runID,
	})
	defer stopRunner()

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	health, err := HealthCheck(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if health.AdoptedRunners != 0 || health.QuarantinedRunners != 1 || health.OrphanedRunners != 0 {
		t.Fatalf("health = %+v", health)
	}
	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "RepairRequired" {
		t.Fatalf("sessions = %+v", sessions)
	}
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var bindings int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from active_runner_bindings`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("active bindings = %d, want 0", bindings)
	}
	if _, err := SendInput(context.Background(), cfg.SocketPath, sessions[0].Session, []byte("hello\n"), "quarantine-input"); err == nil {
		t.Fatal("SendInput succeeded for quarantined runner")
	}
}

func TestSupervisorMarksOldUnreachableRunnerOrphanedOnStartup(t *testing.T) {
	cfg := testConfig(t)
	runID := seedBoundRun(t, cfg, filepath.Join(filepath.Dir(cfg.SocketPath), "missing-runner.sock"))
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(context.Background(), `update active_runner_bindings set last_heartbeat_at='2000-01-01T00:00:00.000Z' where run_id=?`, runID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	health, err := HealthCheck(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if health.OrphanedRunners != 1 || health.AdoptedRunners != 0 || health.QuarantinedRunners != 0 {
		t.Fatalf("health = %+v", health)
	}
	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Failed" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestSupervisorOrphansBindingsWhenTmuxServerIsMissing(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("AGENCY_TMUX_SOCKET", filepath.Join(t.TempDir(), "missing-tmux.sock"))
	runID := seedBoundRun(t, cfg, filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock"))
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTmuxServerIdentity(context.Background(), "default", `{"pid":1,"startedAt":1}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	health, err := HealthCheck(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if health.OrphanedRunners != 1 || health.AdoptedRunners != 0 || health.QuarantinedRunners != 0 {
		t.Fatalf("health = %+v", health)
	}
	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RunStatus != "Failed" {
		t.Fatalf("sessions = %+v", sessions)
	}
	store, err = storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var outcome, bindings, restartEvents int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from run_terminal_outcomes where run_id=? and outcome='Orphaned'`, runID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from active_runner_bindings`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from events where event_type='TmuxServerRestarted'`).Scan(&restartEvents); err != nil {
		t.Fatal(err)
	}
	if outcome != 1 || bindings != 0 || restartEvents != 1 {
		t.Fatalf("outcome=%d bindings=%d restartEvents=%d, want 1/0/1", outcome, bindings, restartEvents)
	}
}

func TestSupervisorReconcilesUnpublishedManagedWorktrees(t *testing.T) {
	cfg := testConfig(t)
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publishPath := filepath.Join(project.ManagedWorktreeRoot, "publishable")
	publishID, err := store.AddManagedWorkspace(context.Background(), project.ID, "publishable", publishPath, "agency/publishable", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(publishPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publishPath, ".agency-worktree"), []byte(publishID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(project.ManagedWorktreeRoot, "missing")
	missingID, err := store.AddManagedWorkspace(context.Background(), project.ID, "missing", missingPath, "agency/missing", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	store, err = storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var active, missingRows int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from active_worktrees aw join managed_worktrees mw on mw.managed_worktree_id=aw.managed_worktree_id where mw.workspace_id=?`, publishID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from workspaces where workspace_id=?`, missingID).Scan(&missingRows); err != nil {
		t.Fatal(err)
	}
	if active != 1 || missingRows != 0 {
		t.Fatalf("active=%d missingRows=%d, want 1/0", active, missingRows)
	}
}

func TestWorktreeStatusShowsFailedClosingWorktree(t *testing.T) {
	cfg := testConfig(t)
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project.ManagedWorktreeRoot, "failed-close")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := store.AddManagedWorkspace(context.Background(), project.ID, "failed-close", path, "agency/failed-close", "HEAD", "base", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(context.Background(), workspaceID); err != nil {
		t.Fatal(err)
	}
	wt, err := store.ActiveManagedWorktree(context.Background(), "failed-close")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := store.BeginWorktreeRemoval(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FailWorktreeRemoval(context.Background(), attemptID, "forced failure"); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: cfg, store: store}
	summary, err := server.worktreeStatus(context.Background(), "failed-close")
	if err != nil {
		t.Fatal(err)
	}
	if summary.WorkspaceKey != "failed-close" || summary.Close["summary"] != "RepairRequired" || summary.Close["closable"] != false {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Git["presence"] != "NotAWorktree" {
		t.Fatalf("git = %+v", summary.Git)
	}
	list, err := server.listWorktrees(context.Background(), project.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].WorkspaceKey != "failed-close" || list[0].Close["summary"] != "RepairRequired" {
		t.Fatalf("list = %+v", list)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorRejectsAttachToClosedSession(t *testing.T) {
	cfg := testConfig(t)
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Closed attach", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRunner(context.Background(), run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.StopRun(context.Background(), run.Session); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseSession(context.Background(), run.Session); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store}
	_, err = server.attachCommand(context.Background(), run.Session)
	if err == nil || err.Error() != "SessionClosed" {
		t.Fatalf("attach error = %v, want SessionClosed", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorPersistsAdoptedRunnerOutput(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	stopRunner := serveRunnerWithOutput(t, runnerSocket, runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion,
		RunnerBinaryVersion:   "test-runner",
		RunID:                 runID,
	}, []byte("supervised-output\n"))
	defer stopRunner()

	cancelSupervisor := startTestSupervisor(t, cfg)
	defer cancelSupervisor()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		store, err := storage.Open(context.Background(), cfg.StateDB)
		if err != nil {
			t.Fatal(err)
		}
		chunks, err := store.ListOutputChunks(context.Background(), runID, 1)
		if closeErr := store.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			if bytes.Contains(chunk.Bytes, []byte("supervised-output")) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor did not persist runner output")
}

func TestSupervisorPersistsRunnerStreamHeartbeats(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner-heartbeat.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	stopRunner := serveRunnerWithPeriodicHeartbeat(t, runnerSocket, runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion,
		RunnerBinaryVersion:   "test-runner",
		RunID:                 runID,
	})
	defer stopRunner()

	cancelSupervisor := startTestSupervisor(t, cfg)
	defer cancelSupervisor()

	old := "2000-01-01T00:00:00.000Z"
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(context.Background(), `update active_runner_bindings set last_heartbeat_at=? where run_id=?`, old, runID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		store, err := storage.Open(context.Background(), cfg.StateDB)
		if err != nil {
			t.Fatal(err)
		}
		var heartbeat string
		err = store.DB.QueryRowContext(context.Background(), `select last_heartbeat_at from active_runner_bindings where run_id=?`, runID).Scan(&heartbeat)
		if closeErr := store.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if heartbeat != old {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor did not persist stream heartbeat")
}

func TestSupervisorRecordsPromptHintFromRunnerOutput(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner-prompt.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	stopRunner := serveRunnerWithOutput(t, runnerSocket, runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion,
		RunnerBinaryVersion:   "test-runner",
		RunID:                 runID,
	}, []byte("approval required before running command\n"))
	defer stopRunner()

	cancelSupervisor := startTestSupervisor(t, cfg)
	defer cancelSupervisor()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		store, err := storage.Open(context.Background(), cfg.StateDB)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot string
		err = store.DB.QueryRowContext(context.Background(), `select s.snapshot_json
			from latest_status_snapshots latest
			join status_snapshots s on s.status_snapshot_id=latest.status_snapshot_id
			join status_subjects subject on subject.status_subject_id=latest.status_subject_id
			where subject.subject_hash='Run:' || ?`, runID).Scan(&snapshot)
		if err == nil && strings.Contains(snapshot, `"NeedsApproval"`) {
			var events, deliveries int
			if err := store.DB.QueryRowContext(context.Background(), `select count(*) from events where event_type='RunNeedsApproval'`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if err := store.DB.QueryRowContext(context.Background(), `select count(*)
				from notification_deliveries d
				join events e on e.event_id=d.event_id
				where e.event_type='RunNeedsApproval' and d.delivered_at is not null`).Scan(&deliveries); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if events != 1 || deliveries != 1 {
				t.Fatalf("events=%d deliveries=%d, want 1/1", events, deliveries)
			}
			return
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.DB.QueryContext(context.Background(), `select snapshot_json from status_snapshots`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshots []string
	for rows.Next() {
		var snapshot string
		if err := rows.Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	t.Fatalf("prompt snapshot was not recorded: %+v", snapshots)
}

func TestSupervisorResubscribesRunnerOutputFromNextChunk(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner-reconnect.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	stopRunner := serveRunnerWithDisconnectingOutput(t, runnerSocket, runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion,
		RunnerBinaryVersion:   "test-runner",
		RunID:                 runID,
	}, [][]byte{[]byte("first\n"), []byte("second\n")})
	defer stopRunner()

	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, outputSubscriptions: map[string]context.CancelFunc{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		server.close()
	}()
	server.startOutputSubscription(ctx, runID, runnerSocket)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		chunks, err := store.ListOutputChunks(context.Background(), runID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) == 2 && chunks[0].ChunkSeq == 1 && chunks[1].ChunkSeq == 2 &&
			string(chunks[0].Bytes) == "first\n" && string(chunks[1].Bytes) == "second\n" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	chunks, err := store.ListOutputChunks(context.Background(), runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("chunks after reconnect = %+v", chunks)
}

func TestSupervisorSendsInputThroughAdoptedRunner(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	ctx, cancelRunner := context.WithCancel(context.Background())
	defer cancelRunner()
	errs := make(chan error, 1)
	go func() {
		errs <- runner.Serve(ctx, runner.Config{
			RunID:         runID,
			SocketPath:    runnerSocket,
			SpoolPath:     filepath.Join(filepath.Dir(cfg.SocketPath), "runner-input.out"),
			BinaryVersion: "test-runner",
			Command:       []string{"/bin/cat"},
			Stdout:        &bytes.Buffer{},
			Stderr:        &bytes.Buffer{},
		})
	}()
	waitForPath(t, runnerSocket, errs)

	cancelSupervisor := startTestSupervisor(t, cfg)
	defer cancelSupervisor()
	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	result, err := SendInput(context.Background(), cfg.SocketPath, sessions[0].Session, []byte("hello runner\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Run != "run_1" || result.InputSeq != 1 {
		t.Fatalf("send result = %+v", result)
	}
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	var delivery string
	if err := store.DB.QueryRowContext(context.Background(), `select delivery_json from run_input_events where run_id=? and input_seq=1`, runID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivery, `"Accepted"`) {
		t.Fatalf("delivery_json = %s", delivery)
	}
	cancelRunner()
	select {
	case err := <-errs:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop")
	}
}

func TestSupervisorMarksInputFailedWhenRunnerIsGone(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "missing-runner.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	sessions, err := ListSessions(context.Background(), cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	if _, err := SendInput(context.Background(), cfg.SocketPath, sessions[0].Session, []byte("hello\n"), ""); err == nil {
		t.Fatal("SendInput returned nil error for missing runner")
	}
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var delivery string
	if err := store.DB.QueryRowContext(context.Background(), `select delivery_json from run_input_events where run_id=? and input_seq=1`, runID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivery, `"Failed"`) {
		t.Fatalf("delivery_json = %s", delivery)
	}
}

func TestSupervisorStopRecordsRunnerTermination(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner-stop-exit.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	session := sessionForRun(t, cfg, runID)
	stopRunner := serveRunnerWithTerminalOutcome(t, runnerSocket, runnerproto.Preamble{
		Magic:                 runnerproto.Magic,
		RunnerProtocolVersion: runnerproto.ProtocolVersion,
		RunnerBinaryVersion:   "test-runner",
		RunID:                 runID,
	}, runnerproto.ProviderExited(7))
	defer stopRunner()

	cancel := startTestSupervisor(t, cfg)
	defer cancel()

	if _, err := StopRun(context.Background(), cfg.SocketPath, session, "stop-actual-outcome"); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var outcome, payload string
	if err := store.DB.QueryRowContext(context.Background(), `select outcome, termination_json from run_terminal_outcomes where run_id=?`, runID).Scan(&outcome, &payload); err != nil {
		t.Fatal(err)
	}
	if outcome != "ProviderExited" || !strings.Contains(payload, `"exitCode":7`) {
		t.Fatalf("terminal outcome = %s %s, want ProviderExited exitCode 7", outcome, payload)
	}
}

func TestSupervisorRecordsRunnerExit(t *testing.T) {
	cfg := testConfig(t)
	runnerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "runner.sock")
	runID := seedBoundRun(t, cfg, runnerSocket)
	ctx, cancelRunner := context.WithCancel(context.Background())
	defer cancelRunner()
	errs := make(chan error, 1)
	go func() {
		errs <- runner.Serve(ctx, runner.Config{
			RunID:         runID,
			SocketPath:    runnerSocket,
			SpoolPath:     filepath.Join(filepath.Dir(cfg.SocketPath), "runner-exit.out"),
			BinaryVersion: "test-runner",
			Command:       []string{"/bin/sh", "-c", "sleep 0.2; exit 7"},
			Stdout:        &bytes.Buffer{},
			Stderr:        &bytes.Buffer{},
		})
	}()
	waitForPath(t, runnerSocket, errs)

	cancelSupervisor := startTestSupervisor(t, cfg)
	defer cancelSupervisor()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := ListSessions(context.Background(), cfg.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) == 1 && sessions[0].RunStatus == "Exited" {
			store, err := storage.Open(context.Background(), cfg.StateDB)
			if err != nil {
				t.Fatal(err)
			}
			var outcome string
			if err := store.DB.QueryRowContext(context.Background(), `select outcome from run_terminal_outcomes where run_id=?`, runID).Scan(&outcome); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if outcome != "ProviderExited" {
				t.Fatalf("outcome = %s", outcome)
			}
			select {
			case err := <-errs:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("runner did not exit")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor did not record runner exit")
}

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{
		StateDB:    filepath.Join(stateDir, "agency.db"),
		SocketPath: filepath.Join(dir, "supervisor.sock"),
		LockPath:   filepath.Join(stateDir, "agency.db.lock"),
	}
}

func waitForPath(t *testing.T, path string, errs <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-errs:
			t.Fatalf("process exited before %s appeared: %v", path, err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}

func seedBoundRun(t *testing.T, cfg Config, runnerSocket string) string {
	t.Helper()
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.EnsureProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Reconcile test", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRunner(context.Background(), created.RunID, created.TmuxTargetID, runnerSocket); err != nil {
		t.Fatal(err)
	}
	return created.RunID
}

func sessionForRun(t *testing.T, cfg Config, runID string) string {
	t.Helper()
	store, err := storage.Open(context.Background(), cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var session string
	if err := store.DB.QueryRowContext(context.Background(), `select s.session_key from agent_sessions s join agent_runs r on r.session_id=s.session_id where r.run_id=?`, runID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	return session
}

func serveRunnerPreamble(t *testing.T, socketPath string, preamble runnerproto.Preamble) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = runnerproto.WritePreamble(conn, preamble)
				_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				msg, err := runnerproto.ReadMessage(conn)
				if err == nil {
					if hello, ok := msg.(runnerproto.Hello); ok && hello.Version == runnerproto.ProtocolVersion && preamble.RunnerProtocolVersion == runnerproto.ProtocolVersion {
						_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
					}
				}
				<-done
			}()
		}
	}()
	return func() {
		close(done)
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

func serveRunnerWithTerminalOutcome(t *testing.T, socketPath string, preamble runnerproto.Preamble, termination runnerproto.Termination) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = runnerproto.WritePreamble(conn, preamble)
				for {
					select {
					case <-done:
						return
					default:
					}
					_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					msg, err := runnerproto.ReadMessage(conn)
					if err != nil {
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							continue
						}
						return
					}
					switch m := msg.(type) {
					case runnerproto.Hello:
						if m.Version == runnerproto.ProtocolVersion {
							_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
						}
					case runnerproto.Subscribe:
						_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
					case runnerproto.Stop, runnerproto.Kill:
						_ = runnerproto.WriteMessage(conn, runnerproto.NewExit(termination))
						return
					}
				}
			}()
		}
	}()
	return func() {
		close(done)
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

func serveRunnerWithOutput(t *testing.T, socketPath string, preamble runnerproto.Preamble, output []byte) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = runnerproto.WritePreamble(conn, preamble)
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				msg, err := runnerproto.ReadMessage(conn)
				if err != nil {
					return
				}
				switch m := msg.(type) {
				case runnerproto.Hello:
					if m.Version == runnerproto.ProtocolVersion {
						_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
					}
				case runnerproto.Subscribe:
					_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
					if m.Output {
						_ = runnerproto.WriteMessage(conn, runnerproto.NewOutputChunk(m.FromChunkSeq, "Stdout", output))
					}
				}
			}()
		}
	}()
	return func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

func serveRunnerWithDisconnectingOutput(t *testing.T, socketPath string, preamble runnerproto.Preamble, outputs [][]byte) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	outputsCh := make(chan []byte, len(outputs))
	for _, output := range outputs {
		outputsCh <- output
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = runnerproto.WritePreamble(conn, preamble)
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				msg, err := runnerproto.ReadMessage(conn)
				if err != nil {
					return
				}
				subscribe, ok := msg.(runnerproto.Subscribe)
				if !ok || !subscribe.Output {
					return
				}
				_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
				select {
				case output := <-outputsCh:
					_ = runnerproto.WriteMessage(conn, runnerproto.NewOutputChunk(subscribe.FromChunkSeq, "Stdout", output))
				default:
				}
			}()
		}
	}()
	return func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

func serveRunnerWithPeriodicHeartbeat(t *testing.T, socketPath string, preamble runnerproto.Preamble) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = runnerproto.WritePreamble(conn, preamble)
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				msg, err := runnerproto.ReadMessage(conn)
				if err != nil {
					return
				}
				switch m := msg.(type) {
				case runnerproto.Hello:
					if m.Version == runnerproto.ProtocolVersion {
						_ = runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID))
					}
				case runnerproto.Subscribe:
					if !m.Output {
						return
					}
					ticker := time.NewTicker(50 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-done:
							return
						case <-ticker.C:
							if err := runnerproto.WriteMessage(conn, runnerproto.NewHeartbeat(preamble.RunID)); err != nil {
								return
							}
						}
					}
				}
			}()
		}
	}()
	return func() {
		close(done)
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

func startTestSupervisor(t *testing.T, cfg Config) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, cfg)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SocketPath); err == nil {
			return func() {
				cancel()
				select {
				case err := <-errs:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Fatalf("supervisor exit: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("supervisor did not stop")
				}
			}
		}
		select {
		case err := <-errs:
			t.Fatalf("supervisor exited before socket appeared: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor socket did not appear")
	return cancel
}
