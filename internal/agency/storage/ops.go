package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agency-two/internal/agency/config"
	"agency-two/internal/agency/eventlog"
	"agency-two/internal/agency/provider"
)

// defaultQuietThreshold bounds output silence before a live runner is labeled
// Quiet. It is the fallback when the configuration service has not supplied
// [timing].quiet_threshold_ms; the effective value lives on Store.quietThreshold.
// Heartbeat liveness (LostRunner) is intentionally not evaluated here: it is a
// monotonic-clock projection owned by the supervisor (see supervisor.runnerLost)
// so a restart or suspend/resume never expires a live runner from a persisted
// wall-clock delta.
const defaultQuietThreshold = 30 * time.Second

// idempotencyRecoveryWindow bounds how long an InFlight replay key is respected
// before it is treated as abandoned by a crashed holder and reclaimed.
const idempotencyRecoveryWindow = 5 * time.Minute

type Project struct {
	ID                   string `json:"projectId"`
	HostID               string `json:"hostId"`
	HostKey              string `json:"hostKey"`
	Key                  string `json:"key"`
	DisplayName          string `json:"displayName"`
	RootPath             string `json:"rootPath"`
	DefaultBaseRef       string `json:"defaultBaseRef"`
	DefaultWorktreeMode  string `json:"defaultWorktreeMode"`
	ManagedWorktreeRoot  string `json:"managedWorktreeRoot"`
	ProjectRootWorkspace string `json:"projectRootWorkspace"`
}

type Model struct {
	Provider         string   `json:"provider"`
	Key              string   `json:"key"`
	Name             string   `json:"name"`
	Efforts          []string `json:"efforts"`
	PermissionModes  []string `json:"permissionModes"`
	SandboxModes     []string `json:"sandboxModes"`
	ApprovalPolicies []string `json:"approvalPolicies"`
	Availability     string   `json:"availability"`
	Notes            string   `json:"notes,omitempty"`
}

type Profile struct {
	ID             string `json:"profileId,omitempty"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	PermissionMode string `json:"permissionMode,omitempty"`
	SandboxMode    string `json:"sandboxMode,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
}

type HostAccess struct {
	Mode             string `json:"mode"`
	HostAlias        string `json:"hostAlias,omitempty"`
	SocketForwarding bool   `json:"socketForwarding,omitempty"`
}

type Host struct {
	Key         string     `json:"host"`
	DisplayName string     `json:"displayName"`
	Access      HostAccess `json:"access"`
}

type LaunchIntent struct {
	ProjectID         string
	WorkspaceID       string
	ProfileRevisionID string
	HostID            string
	Title             string
	WorkingDirectory  string
	Argv              []string
	Env               map[string]any
	TmuxTargetSpec    map[string]any
}

type CreatedRun struct {
	SessionID    string   `json:"sessionId"`
	SessionKey   string   `json:"sessionKey"`
	Session      string   `json:"session"`
	RunID        string   `json:"runId"`
	RunSeq       int      `json:"runSeq"`
	Run          string   `json:"run"`
	TmuxTarget   string   `json:"tmuxTarget"`
	TmuxTargetID string   `json:"tmuxTargetId"`
	Argv         []string `json:"argv"`
	WorkingDir   string   `json:"workingDir"`
	Env          []string `json:"-"`
}

type SessionSummary struct {
	Session   string         `json:"session"`
	Title     string         `json:"title"`
	Provider  string         `json:"provider"`
	Project   string         `json:"project"`
	Workspace string         `json:"workspace"`
	RunStatus string         `json:"runStatus"`
	Git       map[string]any `json:"git"`
	Model     string         `json:"model"`
	Effort    string         `json:"effort"`
	Close     map[string]any `json:"close"`
	LastEvent string         `json:"lastEventAt"`
	// WorkspaceKey is the workspace's stable key (for example project_root or a
	// worktree name). It is serialized because the CLI renders the WORKSPACE
	// column from it (via content.WorkspaceLabel); the supervisor also uses it
	// internally to distinguish the project root from managed worktrees.
	WorkspaceKey string `json:"workspaceKey"`
	Path         string `json:"-"`
	RunID        string `json:"-"`
	WorkspaceID  string `json:"-"`
	// HasActiveBinding is true when an active runner binding exists for the
	// latest run. The supervisor uses it to decide LostRunner on its monotonic
	// clock; it is not part of the public payload.
	HasActiveBinding bool `json:"-"`
}

type SessionDetail struct {
	Summary         SessionSummary
	ID              string
	RunID           string
	Run             string
	Argv            []string
	TmuxTargetKey   string
	TmuxSessionName string
	RunnerSocket    string
}

type ActiveRunnerBinding struct {
	RunID           string
	TmuxServerKey   string
	EndpointPath    string
	ProtocolVersion int
	BinaryVersion   string
	LastHeartbeat   time.Time
}

type OutputChunk struct {
	RunID      string `json:"-"`
	ChunkSeq   int64  `json:"chunkSeq"`
	Stream     string `json:"stream"`
	Bytes      []byte `json:"bytes"`
	CapturedAt string `json:"capturedAt"`
}

type LaunchEnvSnapshot struct {
	Session string
	Run     string
	Env     map[string]any
}

type Event struct {
	EventID     string         `json:"-"`
	EventSeq    int            `json:"eventSeq"`
	OccurredAt  string         `json:"occurredAt"`
	EventType   string         `json:"eventType"`
	Label       string         `json:"-"`
	Subject     map[string]any `json:"subject"`
	Actor       map[string]any `json:"actor"`
	Payload     map[string]any `json:"payload"`
	Correlation map[string]any `json:"correlation"`
}

type ManagedWorktree struct {
	ManagedWorktreeID string
	WorkspaceID       string
	WorkspaceKey      string
	Path              string
	Branch            string
	BaseRef           string
	BaseSHA           string
	MarkerFilePath    string
	MarkerFileHash    string
	ProjectID         string
	ProjectKey        string
	ProjectRoot       string
	ManagedRoot       string
	RepositoryID      string
	GitCommonDir      string
}

type ClosingWorktree struct {
	ManagedWorktree
	AttemptID string
}

type SafetyFinding struct {
	Blocker  string
	Severity string
	Location map[string]any
	Message  string
	Evidence map[string]any
}

func (s *Store) BeginIdempotency(ctx context.Context, replayKey, operationKey, requestHash string) (string, bool, error) {
	if replayKey == "" {
		return "", false, errors.New("replay key is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var storedOperation, storedHash, state, createdAt string
	var result sql.NullString
	err = tx.QueryRowContext(ctx, `select operation_key, request_hash, state, created_at, result_ref_json from idempotency_keys where replay_key=?`, replayKey).Scan(&storedOperation, &storedHash, &state, &createdAt, &result)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `insert into idempotency_keys(idempotency_key_id, replay_key, operation_key, request_hash, state, created_at)
			values(?, ?, ?, ?, 'InFlight', ?)`, NewID(), replayKey, operationKey, requestHash, Now()); err != nil {
			return "", false, err
		}
		return "", false, tx.Commit()
	}
	if err != nil {
		return "", false, err
	}
	if storedOperation != operationKey || storedHash != requestHash {
		return "", false, errors.New("ReplayKeyConflict")
	}
	if state != "Completed" {
		// The key is InFlight. Live same-key operations are serialized by the
		// supervisor's conflict-key locks and complete quickly, so an InFlight
		// row older than the recovery window belongs to a crashed holder: take it
		// over (reset to a fresh InFlight) and let the caller re-run to
		// completion, rather than dead-ending replay forever.
		if created, perr := time.Parse("2006-01-02T15:04:05.000Z", createdAt); perr == nil && time.Since(created) >= idempotencyRecoveryWindow {
			if _, err := tx.ExecContext(ctx, `update idempotency_keys set state='InFlight', created_at=?, result_ref_json=null, completed_at=null where replay_key=?`, Now(), replayKey); err != nil {
				return "", false, err
			}
			return "", false, tx.Commit()
		}
		return "", false, errors.New("ReplayInFlight")
	}
	if !result.Valid {
		return "", false, errors.New("ReplayResultMissing")
	}
	return result.String, true, tx.Commit()
}

func (s *Store) CompleteIdempotency(ctx context.Context, replayKey, operationKey, requestHash, result string) error {
	res, err := s.DB.ExecContext(ctx, `update idempotency_keys set result_ref_json=?, state='Completed', completed_at=?
		where replay_key=? and operation_key=? and request_hash=? and state='InFlight'`, result, Now(), replayKey, operationKey, requestHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("IdempotencyKeyNotInFlight")
	}
	return nil
}

func (s *Store) AbortIdempotency(ctx context.Context, replayKey, operationKey, requestHash string) error {
	_, err := s.DB.ExecContext(ctx, `delete from idempotency_keys where replay_key=? and operation_key=? and request_hash=? and state='InFlight'`, replayKey, operationKey, requestHash)
	return err
}

func (s *Store) RecordWorkspaceSafetyCheck(ctx context.Context, workspaceID, checkKind string, findings []SafetyFinding) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var policyID string
	if err := tx.QueryRowContext(ctx, `select safety_policy_id from safety_policies where safety_policy_key='strict' and archived_at is null`).Scan(&policyID); err != nil {
		return "", err
	}
	subjectID, err := ensureWorkspaceSubject(ctx, tx, workspaceID)
	if err != nil {
		return "", err
	}
	checkID := NewID()
	now := Now()
	if _, err := tx.ExecContext(ctx, `insert into safety_check_runs(safety_check_run_id, safety_policy_id, status_subject_id, check_kind, started_at, completed_at)
		values(?, ?, ?, ?, ?, ?)`, checkID, policyID, subjectID, checkKind, now, now); err != nil {
		return "", err
	}
	for _, finding := range findings {
		location, err := json.Marshal(finding.Location)
		if err != nil {
			return "", err
		}
		evidence, err := json.Marshal(finding.Evidence)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `insert into safety_check_findings(safety_check_finding_id, safety_check_run_id, blocker, severity, location_json, message, evidence_json, created_at)
			values(?, ?, ?, ?, ?, ?, ?, ?)`, NewID(), checkID, finding.Blocker, finding.Severity, string(location), finding.Message, string(evidence), now); err != nil {
			return "", err
		}
	}
	return checkID, tx.Commit()
}

func (s *Store) EnsureProject(ctx context.Context, rootPath string) (Project, error) {
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return Project{}, err
	}
	key := keyFromName(filepath.Base(rootPath))
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()

	if project, ok, err := readProjectByKey(ctx, tx, key); err != nil || ok {
		return project, err
	}

	var hostID string
	if err := tx.QueryRowContext(ctx, `select host_id from hosts where host_key='local' and archived_at is null`).Scan(&hostID); err != nil {
		return Project{}, err
	}
	var profileID string
	if err := tx.QueryRowContext(ctx, `select profile_id from agent_profiles where profile_key='codex_default' and archived_at is null`).Scan(&profileID); err != nil {
		return Project{}, err
	}
	projectID := NewID()
	repositoryID := NewID()
	workspaceID := NewID()
	managedRoot := filepath.Join(filepath.Dir(rootPath), ".agency-worktrees", key)
	gitCommon := filepath.Join(rootPath, ".git")
	if _, err := tx.ExecContext(ctx, `insert into projects(project_id, project_key, display_name, root_path, default_base_ref, default_worktree_mode, managed_worktree_root_path, created_at)
			values(?, ?, ?, ?, 'HEAD', 'Prompt', ?, ?)`, projectID, key, filepath.Base(rootPath), rootPath, managedRoot, now); err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into repositories(repository_id, repository_key, git_common_dir_path, default_branch_ref, created_at)
		values(?, ?, ?, 'refs/heads/main', ?)`, repositoryID, key+"_primary", gitCommon, now); err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into project_repositories(project_repository_id, project_id, repository_id, role, created_at)
		values(?, ?, ?, 'Primary', ?)`, NewID(), projectID, repositoryID, now); err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into project_profile_defaults(project_profile_default_id, project_id, profile_id, created_at)
		values(?, ?, ?, ?)`, NewID(), projectID, profileID, now); err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into workspaces(workspace_id, workspace_key, project_id, repository_id, host_id, path, created_at)
		values(?, 'project_root', ?, ?, ?, ?, ?)`, workspaceID, projectID, repositoryID, hostID, rootPath, now); err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into project_root_workspaces(project_root_workspace_id, workspace_id, created_at)
		values(?, ?, ?)`, NewID(), workspaceID, now); err != nil {
		return Project{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	return Project{ID: projectID, HostID: hostID, HostKey: "local", Key: key, DisplayName: filepath.Base(rootPath), RootPath: rootPath, DefaultBaseRef: "HEAD", DefaultWorktreeMode: "Prompt", ManagedWorktreeRoot: managedRoot, ProjectRootWorkspace: workspaceID}, nil
}

func (s *Store) ProjectForPath(ctx context.Context, path string) (Project, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Project{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `select p.project_id, p.project_key, p.display_name, p.root_path, p.default_base_ref, p.default_worktree_mode, p.managed_worktree_root_path, w.workspace_id, w.host_id, h.host_key
			from projects p
			join workspaces w on w.project_id=p.project_id
			join project_root_workspaces r on r.workspace_id=w.workspace_id
			join hosts h on h.host_id=w.host_id
			where p.archived_at is null order by length(p.root_path) desc`)
	if err != nil {
		return Project{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Key, &p.DisplayName, &p.RootPath, &p.DefaultBaseRef, &p.DefaultWorktreeMode, &p.ManagedWorktreeRoot, &p.ProjectRootWorkspace, &p.HostID, &p.HostKey); err != nil {
			return Project{}, err
		}
		if path == p.RootPath || strings.HasPrefix(path, p.RootPath+string(filepath.Separator)) {
			return p, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Project{}, err
	}
	return Project{}, errors.New("ProjectNotFound")
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.DB.QueryContext(ctx, `select p.project_id, p.project_key, p.display_name, p.root_path, p.default_base_ref, p.default_worktree_mode, p.managed_worktree_root_path, w.host_id, h.host_key
		from projects p
		join workspaces w on w.project_id=p.project_id
		join project_root_workspaces r on r.workspace_id=w.workspace_id
		join hosts h on h.host_id=w.host_id
		where p.archived_at is null order by p.project_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Key, &p.DisplayName, &p.RootPath, &p.DefaultBaseRef, &p.DefaultWorktreeMode, &p.ManagedWorktreeRoot, &p.HostID, &p.HostKey); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListModels(ctx context.Context, provider string) ([]Model, error) {
	query := `select p.provider_key, m.model_key, m.provider_model_name, m.model_spec_json, p.provider_config_json
		from provider_models m join agent_providers p on p.provider_id=m.provider_id where m.retired_at is null`
	args := []any{}
	if provider != "" {
		query += ` and p.provider_key=?`
		args = append(args, provider)
	}
	query += ` order by p.provider_key, m.model_key`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		var modelSpec, providerConfig string
		if err := rows.Scan(&m.Provider, &m.Key, &m.Name, &modelSpec, &providerConfig); err != nil {
			return nil, err
		}
		var spec struct {
			Efforts      []string `json:"efforts"`
			Availability string   `json:"availability"`
			Notes        string   `json:"notes"`
		}
		if err := json.Unmarshal([]byte(modelSpec), &spec); err != nil {
			return nil, err
		}
		var controls struct {
			PermissionModes  []string `json:"permissionModes"`
			SandboxModes     []string `json:"sandboxModes"`
			ApprovalPolicies []string `json:"approvalPolicies"`
		}
		if err := json.Unmarshal([]byte(providerConfig), &controls); err != nil {
			return nil, err
		}
		m.Efforts = nonNilStrings(spec.Efforts)
		m.PermissionModes = nonNilStrings(controls.PermissionModes)
		m.SandboxModes = nonNilStrings(controls.SandboxModes)
		m.ApprovalPolicies = nonNilStrings(controls.ApprovalPolicies)
		m.Availability = spec.Availability
		m.Notes = spec.Notes
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.DB.QueryContext(ctx, `select a.profile_key, a.display_name, p.provider_key, coalesce(m.model_key, ''), coalesce(r.effort_key, ''), r.permission_policy_json
			from agent_profiles a
		join agent_profile_current_revisions c on c.profile_id=a.profile_id
		join agent_profile_revisions r on r.profile_revision_id=c.profile_revision_id
		join agent_providers p on p.provider_id=r.provider_id
		left join provider_models m on m.provider_model_id=r.provider_model_id
		where a.archived_at is null order by a.profile_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		var rawPolicy string
		if err := rows.Scan(&p.Key, &p.Name, &p.Provider, &p.Model, &p.Effort, &rawPolicy); err != nil {
			return nil, err
		}
		var policy map[string]string
		if err := json.Unmarshal([]byte(rawPolicy), &policy); err != nil {
			return nil, err
		}
		p.PermissionMode = policy["permissionMode"]
		p.SandboxMode = policy["sandboxMode"]
		p.ApprovalPolicy = policy["approvalPolicy"]
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpsertProfile(ctx context.Context, key, providerKey, model, effort, policyJSON string) error {
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var providerID string
	if err := tx.QueryRowContext(ctx, `select provider_id from agent_providers where provider_key=? and disabled_at is null`, providerKey).Scan(&providerID); err != nil {
		return err
	}
	var modelArg any
	if model != "" {
		var modelID string
		if err := tx.QueryRowContext(ctx, `select provider_model_id from provider_models where provider_id=? and model_key=? and retired_at is null`, providerID, model).Scan(&modelID); err != nil {
			return err
		}
		modelArg = modelID
	}
	var profileID string
	err = tx.QueryRowContext(ctx, `select profile_id from agent_profiles where profile_key=?`, key).Scan(&profileID)
	if errors.Is(err, sql.ErrNoRows) {
		profileID = NewID()
		if _, err := tx.ExecContext(ctx, `insert into agent_profiles(profile_id, profile_key, display_name, created_at) values(?, ?, ?, ?)`, profileID, key, key, now); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var next int
	if err := tx.QueryRowContext(ctx, `select coalesce(max(revision_seq), 0)+1 from agent_profile_revisions where profile_id=?`, profileID).Scan(&next); err != nil {
		return err
	}
	revisionID := NewID()
	if policyJSON == "" {
		policyJSON = "{}"
	}
	var effortArg any
	if effort != "" {
		effortArg = effort
	}
	if _, err := tx.ExecContext(ctx, `insert into agent_profile_revisions(profile_revision_id, profile_id, revision_seq, provider_id, provider_model_id, effort_key, permission_policy_json, runtime_limits_json, extra_args_json, created_at)
		values(?, ?, ?, ?, ?, ?, ?, '{}', '{}', ?)`, revisionID, profileID, next, providerID, modelArg, effortArg, policyJSON, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from agent_profile_current_revisions where profile_id=?`, profileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into agent_profile_current_revisions(profile_id, profile_revision_id, selected_at) values(?, ?, ?)`, profileID, revisionID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetProjectDefaultProfile(ctx context.Context, projectKey, profileKey string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID, profileID string
	if err := tx.QueryRowContext(ctx, `select project_id from projects where project_key=? and archived_at is null`, projectKey).Scan(&projectID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `select profile_id from agent_profiles where profile_key=? and archived_at is null`, profileKey).Scan(&profileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from project_profile_defaults where project_id=?`, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into project_profile_defaults(project_profile_default_id, project_id, profile_id, created_at) values(?, ?, ?, ?)`, NewID(), projectID, profileID, Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProjectDefaultProfile(ctx context.Context, projectKey string) (string, error) {
	var profile string
	err := s.DB.QueryRowContext(ctx, `select a.profile_key
		from projects p
		join project_profile_defaults d on d.project_id=p.project_id
		join agent_profiles a on a.profile_id=d.profile_id
		where p.project_key=? and p.archived_at is null and a.archived_at is null`, projectKey).Scan(&profile)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("ProjectDefaultProfileNotFound")
	}
	return profile, err
}

func (s *Store) SetProjectDefaultBaseRef(ctx context.Context, projectKey, baseRef string) error {
	if strings.TrimSpace(baseRef) == "" || strings.ContainsAny(baseRef, "\x00\n\r") {
		return errors.New("InvalidBaseRef")
	}
	res, err := s.DB.ExecContext(ctx, `update projects set default_base_ref=? where project_key=? and archived_at is null`, baseRef, projectKey)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("ProjectNotFound")
	}
	return nil
}

func (s *Store) SetProjectDefaultWorktreeMode(ctx context.Context, projectKey, mode string) error {
	mode, err := ParseWorktreeMode(mode)
	if err != nil {
		return err
	}
	res, err := s.DB.ExecContext(ctx, `update projects set default_worktree_mode=? where project_key=? and archived_at is null`, mode, projectKey)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("ProjectNotFound")
	}
	return nil
}

func (s *Store) ProjectDefaultWorktreeMode(ctx context.Context, projectKey string) (string, error) {
	var mode string
	err := s.DB.QueryRowContext(ctx, `select default_worktree_mode from projects where project_key=? and archived_at is null`, projectKey).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("ProjectNotFound")
	}
	return mode, err
}

func (s *Store) SetProjectDefaultHost(ctx context.Context, projectKey, hostKey string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hostID string
	if err := tx.QueryRowContext(ctx, `select host_id from hosts where host_key=? and archived_at is null`, hostKey).Scan(&hostID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("HostNotFound")
		}
		return err
	}
	res, err := tx.ExecContext(ctx, `update workspaces set host_id=?
		where workspace_id=(
			select w.workspace_id
			from projects p
			join workspaces w on w.project_id=p.project_id
			join project_root_workspaces r on r.workspace_id=w.workspace_id
			where p.project_key=? and p.archived_at is null
		)`, hostID, projectKey)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("ProjectNotFound")
	}
	return tx.Commit()
}

func (s *Store) ProjectDefaultHost(ctx context.Context, projectKey string) (string, error) {
	var host string
	err := s.DB.QueryRowContext(ctx, `select h.host_key
		from projects p
		join workspaces w on w.project_id=p.project_id
		join project_root_workspaces r on r.workspace_id=w.workspace_id
		join hosts h on h.host_id=w.host_id
		where p.project_key=? and p.archived_at is null and h.archived_at is null`, projectKey).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("ProjectNotFound")
	}
	return host, err
}

func (s *Store) ProjectDefaultBaseRef(ctx context.Context, projectKey string) (string, error) {
	var baseRef string
	err := s.DB.QueryRowContext(ctx, `select default_base_ref from projects where project_key=? and archived_at is null`, projectKey).Scan(&baseRef)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("ProjectNotFound")
	}
	return baseRef, err
}

func (s *Store) LaunchEnvSnapshots(ctx context.Context) ([]LaunchEnvSnapshot, error) {
	rows, err := s.DB.QueryContext(ctx, `select s.session_key, r.run_seq, r.launch_env_json
		from agent_runs r
		join agent_sessions s on s.session_id=r.session_id
		order by s.created_at, r.run_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LaunchEnvSnapshot
	for rows.Next() {
		var session, raw string
		var seq int
		if err := rows.Scan(&session, &seq, &raw); err != nil {
			return nil, err
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return nil, err
		}
		if env == nil {
			env = map[string]any{}
		}
		out = append(out, LaunchEnvSnapshot{Session: session, Run: fmtRun(seq), Env: env})
	}
	return out, rows.Err()
}

func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.DB.QueryContext(ctx, `select host_key, display_name, access_spec_json from hosts where archived_at is null order by host_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		host, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, host)
	}
	return out, rows.Err()
}

func (s *Store) Host(ctx context.Context, key string) (Host, error) {
	row := s.DB.QueryRowContext(ctx, `select host_key, display_name, access_spec_json from hosts where host_key=? and archived_at is null`, key)
	host, err := scanHost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Host{}, errors.New("HostNotFound")
	}
	return host, err
}

func (s *Store) RecordEffectiveConfigRevision(ctx context.Context, stateDB string, cfg config.Config) error {
	effective, err := s.EffectiveConfig(ctx, stateDB, cfg)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(effective)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sourceID := ""
	err = tx.QueryRowContext(ctx, `select config_source_id from config_sources where source_key='cli'`).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		sourceID = NewID()
		if _, err := tx.ExecContext(ctx, `insert into config_sources(config_source_id, source_key, path, priority, created_at)
			values(?, 'cli', 'agency config set', 100, ?)`, sourceID, now); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into config_revisions(config_revision_id, config_source_id, content_hash, effective_config_json, loaded_at)
		values(?, ?, ?, ?, ?)`, NewID(), sourceID, hex.EncodeToString(sum[:]), string(raw), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LatestEffectiveConfig(ctx context.Context) (map[string]any, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `select r.effective_config_json
		from config_revisions r
		join config_sources s on s.config_source_id=r.config_source_id
		where s.source_key='cli' and s.disabled_at is null
		order by r.loaded_at desc, r.config_revision_id desc limit 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("ConfigRevisionNotFound")
	}
	if err != nil {
		return nil, err
	}
	var effective map[string]any
	if err := json.Unmarshal([]byte(raw), &effective); err != nil {
		return nil, err
	}
	return effective, nil
}

// EffectiveConfig blends the runtime-tunable configuration parsed from TOML
// (timing, retention, security, ui, defaults) with the catalog projected from
// storage (hosts, providers, profiles, projects). It is the durable, auditable
// record persisted as a config revision.
func (s *Store) EffectiveConfig(ctx context.Context, stateDB string, cfg config.Config) (map[string]any, error) {
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := s.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := s.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	models, err := s.ListModels(ctx, "")
	if err != nil {
		return nil, err
	}

	defaults := map[string]any{
		"host":         cfg.Defaults.Host,
		"profile":      cfg.Defaults.Profile,
		"baseRef":      cfg.Defaults.BaseRef,
		"worktreeMode": cfg.Defaults.WorktreeMode,
	}
	if cfg.Defaults.Project != "" {
		defaults["project"] = cfg.Defaults.Project
	}
	projectsJSON := map[string]any{}
	for _, project := range projects {
		profile, err := s.ProjectDefaultProfile(ctx, project.Key)
		if err != nil {
			return nil, err
		}
		if defaults["project"] == nil {
			defaults["project"] = project.Key
			defaults["host"] = project.HostKey
			defaults["profile"] = profile
			defaults["baseRef"] = project.DefaultBaseRef
			defaults["worktreeMode"] = project.DefaultWorktreeMode
		}
		projectsJSON[project.Key] = map[string]any{
			"displayName":         project.DisplayName,
			"root":                project.RootPath,
			"defaultHost":         project.HostKey,
			"defaultProfile":      profile,
			"baseRef":             project.DefaultBaseRef,
			"worktreeMode":        project.DefaultWorktreeMode,
			"managedWorktreeRoot": project.ManagedWorktreeRoot,
		}
	}

	hostsJSON := map[string]any{}
	for _, host := range hosts {
		hostsJSON[host.Key] = map[string]any{"displayName": host.DisplayName, "access": host.Access}
	}

	profilesJSON := map[string]any{}
	for _, profile := range profiles {
		record := map[string]any{
			"displayName": profile.Name,
			"provider":    profile.Provider,
			"model":       profile.Model,
			"effort":      profile.Effort,
		}
		if profile.PermissionMode != "" {
			record["permissionMode"] = profile.PermissionMode
		}
		if profile.SandboxMode != "" {
			record["sandboxMode"] = profile.SandboxMode
		}
		if profile.ApprovalPolicy != "" {
			record["approvalPolicy"] = profile.ApprovalPolicy
		}
		profilesJSON[profile.Key] = record
	}

	providersJSON := map[string]any{}
	for _, model := range models {
		providerJSON, _ := providersJSON[model.Provider].(map[string]any)
		if providerJSON == nil {
			providerJSON = map[string]any{
				"models":           []any{},
				"permissionModes":  model.PermissionModes,
				"sandboxModes":     model.SandboxModes,
				"approvalPolicies": model.ApprovalPolicies,
			}
			providersJSON[model.Provider] = providerJSON
		}
		providerJSON["models"] = append(providerJSON["models"].([]any), map[string]any{
			"key":          model.Key,
			"name":         model.Name,
			"efforts":      model.Efforts,
			"availability": model.Availability,
			"notes":        model.Notes,
		})
	}

	logDir := cfg.Paths.LogDir
	if logDir == "" {
		logDir = filepath.Join(filepath.Dir(stateDB), "logs")
	}
	notificationsJSON := map[string]any{}
	for key, channel := range cfg.Notifications {
		notificationsJSON[key] = map[string]any{"type": channel.Type, "events": channel.Events}
	}
	return map[string]any{
		"version": 1,
		"paths": map[string]any{
			"stateDb": stateDB,
			"logDir":  logDir,
		},
		"defaults": defaults,
		"ui": map[string]any{
			"theme":      cfg.UI.Theme,
			"refreshMs":  cfg.UI.RefreshMs,
			"showClosed": cfg.UI.ShowClosed,
		},
		"timing": map[string]any{
			"heartbeatIntervalMs":   cfg.Timing.HeartbeatIntervalMs,
			"heartbeatTtlMs":        cfg.Timing.HeartbeatTTLMs,
			"quietThresholdMs":      cfg.Timing.QuietThresholdMs,
			"gracefulStopTimeoutMs": cfg.Timing.GracefulStopTimeoutMs,
			"reconcileIntervalMs":   cfg.Timing.ReconcileIntervalMs,
			"suspendResumeReset":    cfg.Timing.SuspendResumeReset,
		},
		"retention": map[string]any{
			"outputRetentionDays":      cfg.Retention.OutputRetentionDays,
			"eventRetentionDays":       cfg.Retention.EventRetentionDays,
			"statusSnapshotWindow":     cfg.Retention.StatusSnapshotWindow,
			"idempotencyRetentionDays": cfg.Retention.IdempotencyRetentionDays,
			"busyTimeoutMs":            cfg.Retention.BusyTimeoutMs,
			"journalSizeLimitBytes":    cfg.Retention.JournalSizeLimitBytes,
		},
		"security": map[string]any{
			"socketPeerCredentialCheck": cfg.Security.SocketPeerCredentialCheck,
			"tcpTunnelRequiresToken":    cfg.Security.TCPTunnelRequiresToken,
			"tcpTunnelLoopbackOnly":     cfg.Security.TCPTunnelLoopbackOnly,
		},
		"hosts":         hostsJSON,
		"providers":     providersJSON,
		"profiles":      profilesJSON,
		"projects":      projectsJSON,
		"notifications": map[string]any{"channels": notificationsJSON},
	}, nil
}

func (s *Store) UpsertHostAccess(ctx context.Context, key, value string) (Host, error) {
	if !ValidHostKey(key) {
		return Host{}, errors.New("InvalidHostKey")
	}
	access, err := ParseHostAccess(value)
	if err != nil {
		return Host{}, err
	}
	raw, err := json.Marshal(access)
	if err != nil {
		return Host{}, err
	}
	now := Now()
	displayName := strings.ReplaceAll(key, "_", " ")
	_, err = s.DB.ExecContext(ctx, `insert into hosts(host_id, host_key, display_name, access_spec_json, created_at)
		values(?, ?, ?, ?, ?)
		on conflict(host_key) do update set display_name=excluded.display_name, access_spec_json=excluded.access_spec_json, archived_at=null`, NewID(), key, displayName, string(raw), now)
	if err != nil {
		return Host{}, err
	}
	return Host{Key: key, DisplayName: displayName, Access: access}, nil
}

func (s *Store) DefaultProfileRevision(ctx context.Context, projectID, provider string) (string, string, string, string, string, error) {
	row := s.DB.QueryRowContext(ctx, `select r.profile_revision_id, p.provider_key, coalesce(m.model_key, ''), coalesce(r.effort_key, ''), r.permission_policy_json
		from projects pr
		join project_profile_defaults d on d.project_id=pr.project_id
		join agent_profile_current_revisions c on c.profile_id=d.profile_id
		join agent_profile_revisions r on r.profile_revision_id=c.profile_revision_id
		join agent_providers p on p.provider_id=r.provider_id
		left join provider_models m on m.provider_model_id=r.provider_model_id
		where pr.project_id=?`, projectID)
	var rev, gotProvider, model, effort, policy string
	if err := row.Scan(&rev, &gotProvider, &model, &effort, &policy); err != nil {
		return "", "", "", "", "", err
	}
	if provider != "" && provider != gotProvider {
		row = s.DB.QueryRowContext(ctx, `select r.profile_revision_id, p.provider_key, coalesce(m.model_key, ''), coalesce(r.effort_key, ''), r.permission_policy_json
			from agent_profiles a
			join agent_profile_current_revisions c on c.profile_id=a.profile_id
			join agent_profile_revisions r on r.profile_revision_id=c.profile_revision_id
			join agent_providers p on p.provider_id=r.provider_id
			left join provider_models m on m.provider_model_id=r.provider_model_id
			where a.profile_key=?`, provider+"_default")
		if err := row.Scan(&rev, &gotProvider, &model, &effort, &policy); err != nil {
			return "", "", "", "", "", err
		}
	}
	return rev, gotProvider, model, effort, policy, nil
}

func (s *Store) ProfileRevision(ctx context.Context, profile string) (string, string, string, string, string, error) {
	row := s.DB.QueryRowContext(ctx, `select r.profile_revision_id, p.provider_key, coalesce(m.model_key, ''), coalesce(r.effort_key, ''), r.permission_policy_json
		from agent_profiles a
		join agent_profile_current_revisions c on c.profile_id=a.profile_id
		join agent_profile_revisions r on r.profile_revision_id=c.profile_revision_id
		join agent_providers p on p.provider_id=r.provider_id
		left join provider_models m on m.provider_model_id=r.provider_model_id
		where a.profile_key=? and a.archived_at is null`, profile)
	var rev, provider, model, effort, policy string
	if err := row.Scan(&rev, &provider, &model, &effort, &policy); err != nil {
		return "", "", "", "", "", err
	}
	return rev, provider, model, effort, policy, nil
}

func (s *Store) CreateRunIntent(ctx context.Context, input LaunchIntent) (CreatedRun, error) {
	sessionID := NewID()
	runID := NewID()
	tmuxID := NewID()
	sessionKey := Handle("ses_", sessionID)
	tmuxKey := Handle("tmux_", tmuxID)
	if input.TmuxTargetSpec == nil {
		input.TmuxTargetSpec = map[string]any{}
	}
	if _, ok := input.TmuxTargetSpec["session"]; !ok {
		input.TmuxTargetSpec["session"] = "agency-" + sessionKey
	}
	argv, err := json.Marshal(input.Argv)
	if err != nil {
		return CreatedRun{}, err
	}
	env, err := json.Marshal(input.Env)
	if err != nil {
		return CreatedRun{}, err
	}
	target, err := json.Marshal(input.TmuxTargetSpec)
	if err != nil {
		return CreatedRun{}, err
	}
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CreatedRun{}, err
	}
	defer tx.Rollback()
	if err := ensureWorkspaceRunnable(ctx, tx, input.WorkspaceID); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into tmux_targets(tmux_target_id, tmux_target_key, host_id, tmux_server_id, target_spec_json, created_at)
		values(?, ?, ?, (select tmux_server_id from tmux_servers where tmux_server_key='default'), ?, ?)`, tmuxID, tmuxKey, input.HostID, string(target), now); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into agent_sessions(session_id, session_key, project_id, workspace_id, profile_revision_id, host_id, title, created_at)
		values(?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, sessionKey, input.ProjectID, input.WorkspaceID, input.ProfileRevisionID, input.HostID, input.Title, now); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into session_tmux_targets(session_tmux_target_id, session_id, tmux_target_id, attached_at)
		values(?, ?, ?, ?)`, NewID(), sessionID, tmuxID, now); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into agent_runs(run_id, session_id, run_seq, launch_argv_json, launch_env_json, working_directory, requested_at)
		values(?, ?, 1, ?, ?, ?, ?)`, runID, sessionID, string(argv), string(env), input.WorkingDirectory, now); err != nil {
		return CreatedRun{}, err
	}
	if err := insertSessionEvent(ctx, tx, sessionID, string(eventlog.SessionCreated), `{"session":"`+sessionKey+`"}`); err != nil {
		return CreatedRun{}, err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunRequested), `{"run":"run_1","session":"`+sessionKey+`"}`); err != nil {
		return CreatedRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreatedRun{}, err
	}
	return CreatedRun{
		SessionID: sessionID, SessionKey: sessionKey, Session: sessionKey,
		RunID: runID, RunSeq: 1, Run: "run_1", TmuxTarget: tmuxKey, TmuxTargetID: tmuxID,
		Argv: input.Argv, WorkingDir: input.WorkingDirectory,
	}, nil
}

func (s *Store) AddManagedWorkspace(ctx context.Context, projectID, workspaceKey, path, branch, baseRef, baseSHA, initialHeadSHA string) (string, error) {
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var repositoryID, hostID string
	if err := tx.QueryRowContext(ctx, `select repository_id from project_repositories where project_id=? and role='Primary'`, projectID).Scan(&repositoryID); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `select w.host_id
		from workspaces w
		join project_root_workspaces r on r.workspace_id=w.workspace_id
		where w.project_id=?`, projectID).Scan(&hostID); err != nil {
		return "", err
	}
	workspaceID := NewID()
	managedID := NewID()
	markerPath := filepath.Join(path, ".agency-worktree")
	if _, err := tx.ExecContext(ctx, `insert into workspaces(workspace_id, workspace_key, project_id, repository_id, host_id, path, created_at)
		values(?, ?, ?, ?, ?, ?, ?)`, workspaceID, workspaceKey, projectID, repositoryID, hostID, path, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `insert into managed_worktrees(managed_worktree_id, workspace_id, branch_name, base_ref, base_sha, initial_head_sha, marker_file_path, marker_file_hash, created_at)
		values(?, ?, ?, ?, ?, ?, ?, ?, ?)`, managedID, workspaceID, branch, baseRef, baseSHA, initialHeadSHA, markerPath, workspaceID, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return workspaceID, nil
}

func (s *Store) PublishManagedWorkspace(ctx context.Context, workspaceID string) error {
	var managedID string
	if err := s.DB.QueryRowContext(ctx, `select managed_worktree_id from managed_worktrees where workspace_id=?`, workspaceID).Scan(&managedID); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `insert into active_worktrees(active_worktree_id, managed_worktree_id, published_at)
		values(?, ?, ?)`, NewID(), managedID, Now())
	return err
}

// RecordWorktreeReconciled appends a WorktreeReconciled audit event for a
// managed worktree that changed state during a reconciliation pass. It is an
// append-only audit record on the workspace subject; it never mutates canonical
// worktree state.
func (s *Store) RecordWorktreeReconciled(ctx context.Context, workspaceID, action string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertWorkspaceEvent(ctx, tx, workspaceID, string(eventlog.WorktreeReconciled), `{"action":`+quoteJSON(action)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteUnpublishedManagedWorkspace(ctx context.Context, workspaceID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var managedID string
	if err := tx.QueryRowContext(ctx, `select managed_worktree_id from managed_worktrees where workspace_id=?`, workspaceID).Scan(&managedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("ManagedWorktreeNotFound")
		}
		return err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `select count(*) from active_worktrees where managed_worktree_id=?`, managedID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return errors.New("ManagedWorktreePublished")
	}
	res, err := tx.ExecContext(ctx, `delete from managed_worktrees where managed_worktree_id=?`, managedID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("ManagedWorktreeNotFound")
	}
	res, err = tx.ExecContext(ctx, `delete from workspaces where workspace_id=?`, workspaceID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("WorkspaceNotFound")
	}
	return tx.Commit()
}

func (s *Store) ListActiveManagedWorktrees(ctx context.Context, projectID string) ([]ManagedWorktree, error) {
	rows, err := s.DB.QueryContext(ctx, managedWorktreeSelect()+` where w.project_id=? order by w.workspace_key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedWorktree
	for rows.Next() {
		wt, err := scanManagedWorktree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wt)
	}
	return out, rows.Err()
}

func (s *Store) ListUnpublishedManagedWorktrees(ctx context.Context) ([]ManagedWorktree, error) {
	rows, err := s.DB.QueryContext(ctx, `select mw.managed_worktree_id, w.workspace_id, w.workspace_key, w.path, mw.branch_name, mw.base_ref, mw.base_sha,
		mw.marker_file_path, mw.marker_file_hash, p.project_id, p.project_key, p.root_path, p.managed_worktree_root_path, r.repository_id, r.git_common_dir_path
		from managed_worktrees mw
		join workspaces w on w.workspace_id=mw.workspace_id
		join projects p on p.project_id=w.project_id
		join repositories r on r.repository_id=w.repository_id
		left join active_worktrees aw on aw.managed_worktree_id=mw.managed_worktree_id
		where aw.active_worktree_id is null and w.closed_at is null
		order by w.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedWorktree
	for rows.Next() {
		wt, err := scanManagedWorktree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wt)
	}
	return out, rows.Err()
}

func (s *Store) ActiveManagedWorktree(ctx context.Context, workspaceKey string) (ManagedWorktree, error) {
	row := s.DB.QueryRowContext(ctx, managedWorktreeSelect()+` where w.workspace_key=?`, workspaceKey)
	wt, err := scanManagedWorktree(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedWorktree{}, errors.New("ManagedWorktreeNotFound")
	}
	return wt, err
}

func (s *Store) ClosingManagedWorktree(ctx context.Context, workspaceKey string) (ClosingWorktree, error) {
	row := s.DB.QueryRowContext(ctx, `select cw.close_attempt_id, mw.managed_worktree_id, w.workspace_id, w.workspace_key, w.path, mw.branch_name, mw.base_ref, mw.base_sha,
		mw.marker_file_path, mw.marker_file_hash, p.project_id, p.project_key, p.root_path, p.managed_worktree_root_path, r.repository_id, r.git_common_dir_path
		from closing_worktrees cw
		join managed_worktrees mw on mw.managed_worktree_id=cw.managed_worktree_id
		join workspaces w on w.workspace_id=mw.workspace_id
		join projects p on p.project_id=w.project_id
		join repositories r on r.repository_id=w.repository_id
		where w.workspace_key=? and w.closed_at is null`, workspaceKey)
	var closing ClosingWorktree
	err := row.Scan(&closing.AttemptID, &closing.ManagedWorktreeID, &closing.WorkspaceID, &closing.WorkspaceKey, &closing.Path, &closing.Branch, &closing.BaseRef, &closing.BaseSHA, &closing.MarkerFilePath, &closing.MarkerFileHash, &closing.ProjectID, &closing.ProjectKey, &closing.ProjectRoot, &closing.ManagedRoot, &closing.RepositoryID, &closing.GitCommonDir)
	if errors.Is(err, sql.ErrNoRows) {
		return ClosingWorktree{}, errors.New("ManagedWorktreeNotFound")
	}
	return closing, err
}

func (s *Store) ListClosingWorktrees(ctx context.Context) ([]ClosingWorktree, error) {
	rows, err := s.DB.QueryContext(ctx, `select cw.close_attempt_id, mw.managed_worktree_id, w.workspace_id, w.workspace_key, w.path, mw.branch_name, mw.base_ref, mw.base_sha,
		mw.marker_file_path, mw.marker_file_hash, p.project_id, p.project_key, p.root_path, p.managed_worktree_root_path, r.repository_id, r.git_common_dir_path
		from closing_worktrees cw
		join managed_worktrees mw on mw.managed_worktree_id=cw.managed_worktree_id
		join workspaces w on w.workspace_id=mw.workspace_id
		join projects p on p.project_id=w.project_id
		join repositories r on r.repository_id=w.repository_id
		order by cw.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClosingWorktree
	for rows.Next() {
		var closing ClosingWorktree
		if err := rows.Scan(&closing.AttemptID, &closing.ManagedWorktreeID, &closing.WorkspaceID, &closing.WorkspaceKey, &closing.Path, &closing.Branch, &closing.BaseRef, &closing.BaseSHA, &closing.MarkerFilePath, &closing.MarkerFileHash, &closing.ProjectID, &closing.ProjectKey, &closing.ProjectRoot, &closing.ManagedRoot, &closing.RepositoryID, &closing.GitCommonDir); err != nil {
			return nil, err
		}
		out = append(out, closing)
	}
	return out, rows.Err()
}

func (s *Store) ListClosingManagedWorktrees(ctx context.Context, projectID string) ([]ClosingWorktree, error) {
	rows, err := s.DB.QueryContext(ctx, `select cw.close_attempt_id, mw.managed_worktree_id, w.workspace_id, w.workspace_key, w.path, mw.branch_name, mw.base_ref, mw.base_sha,
		mw.marker_file_path, mw.marker_file_hash, p.project_id, p.project_key, p.root_path, p.managed_worktree_root_path, r.repository_id, r.git_common_dir_path
		from closing_worktrees cw
		join managed_worktrees mw on mw.managed_worktree_id=cw.managed_worktree_id
		join workspaces w on w.workspace_id=mw.workspace_id
		join projects p on p.project_id=w.project_id
		join repositories r on r.repository_id=w.repository_id
		where w.project_id=? and w.closed_at is null
		order by w.workspace_key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClosingWorktree
	for rows.Next() {
		var closing ClosingWorktree
		if err := rows.Scan(&closing.AttemptID, &closing.ManagedWorktreeID, &closing.WorkspaceID, &closing.WorkspaceKey, &closing.Path, &closing.Branch, &closing.BaseRef, &closing.BaseSHA, &closing.MarkerFilePath, &closing.MarkerFileHash, &closing.ProjectID, &closing.ProjectKey, &closing.ProjectRoot, &closing.ManagedRoot, &closing.RepositoryID, &closing.GitCommonDir); err != nil {
			return nil, err
		}
		out = append(out, closing)
	}
	return out, rows.Err()
}

func (s *Store) LiveSessionDetailsForWorkspace(ctx context.Context, workspaceID string) ([]SessionDetail, error) {
	rows, err := s.DB.QueryContext(ctx, `select s.session_key
		from agent_sessions s
		join agent_runs r on r.session_id=s.session_id and r.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)
		left join run_terminal_outcomes o on o.run_id=r.run_id
		where s.workspace_id=? and s.closed_at is null and o.run_terminal_outcome_id is null`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var handles []string
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			return nil, err
		}
		handles = append(handles, handle)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	details := make([]SessionDetail, 0, len(handles))
	for _, handle := range handles {
		detail, err := s.Session(ctx, handle)
		if err != nil {
			return nil, err
		}
		details = append(details, detail)
	}
	return details, nil
}

func (s *Store) SessionHandlesForWorkspace(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `select session_key from agent_sessions where workspace_id=? and closed_at is null order by created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var session string
		if err := rows.Scan(&session); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if sessions == nil {
		sessions = []string{}
	}
	return sessions, rows.Err()
}

func (s *Store) RecordBlockedWorktreeClose(ctx context.Context, wt ManagedWorktree, findings []SafetyFinding) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	attemptID := NewID()
	now := Now()
	workspace := worktreeHandle(wt)
	if _, err := tx.ExecContext(ctx, `insert into close_attempts(close_attempt_id, close_attempt_key, target_json, requested_by_actor_json, requested_at)
		values(?, ?, ?, '{"kind":"User"}', ?)`, attemptID, Handle("cls_", attemptID), worktreeTargetJSON(wt), now); err != nil {
		return "", err
	}
	blockers := make([]string, 0, len(findings))
	for _, finding := range findings {
		evidence, err := json.Marshal(finding.Evidence)
		if err != nil {
			return "", err
		}
		blockers = append(blockers, finding.Blocker)
		if _, err := tx.ExecContext(ctx, `insert into close_blockers(close_blocker_id, close_attempt_id, blocker, summary, evidence_json, created_at)
			values(?, ?, ?, ?, ?, ?)`, NewID(), attemptID, finding.Blocker, finding.Message, string(evidence), now); err != nil {
			return "", err
		}
	}
	if err := insertWorkspaceEvent(ctx, tx, wt.WorkspaceID, string(eventlog.CloseAttemptStarted), `{"target":"Workspace","workspace":"`+workspace+`"}`); err != nil {
		return "", err
	}
	if err := insertWorkspaceEvent(ctx, tx, wt.WorkspaceID, string(eventlog.CloseBlocked), `{"target":"Workspace","workspace":"`+workspace+`","blockers":`+stringListJSON(blockers)+`}`); err != nil {
		return "", err
	}
	return attemptID, tx.Commit()
}

func (s *Store) BeginWorktreeRemoval(ctx context.Context, wt ManagedWorktree) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var liveSessions int
	if err := tx.QueryRowContext(ctx, `select count(*)
		from agent_sessions s
		join agent_runs r on r.session_id=s.session_id and r.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)
		left join run_terminal_outcomes o on o.run_id=r.run_id
		where s.workspace_id=? and s.closed_at is null and o.run_terminal_outcome_id is null`, wt.WorkspaceID).Scan(&liveSessions); err != nil {
		return "", err
	}
	if liveSessions != 0 {
		return "", errors.New("SessionStillRunning")
	}
	now := Now()
	attemptID := NewID()
	workspace := worktreeHandle(wt)
	if _, err := tx.ExecContext(ctx, `insert into close_attempts(close_attempt_id, close_attempt_key, target_json, requested_by_actor_json, requested_at)
		values(?, ?, ?, '{"kind":"User"}', ?)`, attemptID, Handle("cls_", attemptID), worktreeTargetJSON(wt), now); err != nil {
		return "", err
	}
	if err := insertWorkspaceEvent(ctx, tx, wt.WorkspaceID, string(eventlog.CloseAttemptStarted), `{"target":"Workspace","workspace":"`+workspace+`"}`); err != nil {
		return "", err
	}
	res, err := tx.ExecContext(ctx, `delete from active_worktrees where managed_worktree_id=?`, wt.ManagedWorktreeID)
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", err
	} else if n != 1 {
		return "", errors.New("ActiveWorktreeNotFound")
	}
	memo := `{"path":` + quoteJSON(wt.Path) + `,"branch":` + quoteJSON(wt.Branch) + `,"baseRef":` + quoteJSON(wt.BaseRef) + `,"baseSha":` + quoteJSON(wt.BaseSHA) + `,"repositoryId":"` + wt.RepositoryID + `","gitCommonDir":` + quoteJSON(wt.GitCommonDir) + `}`
	if _, err := tx.ExecContext(ctx, `insert into closing_worktrees(closing_worktree_id, managed_worktree_id, close_attempt_id, memo_json, created_at)
		values(?, ?, ?, ?, ?)`, NewID(), wt.ManagedWorktreeID, attemptID, memo, now); err != nil {
		return "", err
	}
	return attemptID, tx.Commit()
}

func (s *Store) CompleteWorktreeRemoval(ctx context.Context, managedWorktreeID, attemptID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	res, err := tx.ExecContext(ctx, `delete from closing_worktrees where managed_worktree_id=? and close_attempt_id=?`, managedWorktreeID, attemptID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("ClosingWorktreeNotFound")
	}
	if _, err := tx.ExecContext(ctx, `insert into removed_worktrees(removed_worktree_id, managed_worktree_id, removed_at, removal_reason_json)
		values(?, ?, ?, '{"kind":"UserClosed"}')`, NewID(), managedWorktreeID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update workspaces set closed_at=? where workspace_id=(select workspace_id from managed_worktrees where managed_worktree_id=?)`, now, managedWorktreeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update close_attempts set completed_at=? where close_attempt_id=?`, now, attemptID); err != nil {
		return err
	}
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `select workspace_id from managed_worktrees where managed_worktree_id=?`, managedWorktreeID).Scan(&workspaceID); err != nil {
		return err
	}
	if err := insertWorkspaceEvent(ctx, tx, workspaceID, string(eventlog.ManagedWorktreeRemoved), `{"workspace":"`+Handle("wks_", workspaceID)+`"}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailWorktreeRemoval(ctx context.Context, attemptID, detail string) error {
	_, err := s.DB.ExecContext(ctx, `update close_attempts set failed_at=?, failure_json=? where close_attempt_id=?`, Now(), `{"code":"GitWorktreeRemoveFailed","detail":`+quoteJSON(detail)+`}`, attemptID)
	return err
}

func (s *Store) CreateAdditionalRunFromPrevious(ctx context.Context, sessionHandle string) (CreatedRun, error) {
	if _, err := s.Session(ctx, sessionHandle); err != nil {
		return CreatedRun{}, err
	}
	var sessionID, hostID, workspaceID, providerKey, argvJSON, envJSON, cwd string
	err := s.DB.QueryRowContext(ctx, `select s.session_id, s.host_id, s.workspace_id, p.provider_key, r.launch_argv_json, r.launch_env_json, r.working_directory
		from agent_sessions s
		join agent_profile_revisions apr on apr.profile_revision_id=s.profile_revision_id
		join agent_providers p on p.provider_id=apr.provider_id
		join agent_runs r on r.session_id=s.session_id
		where s.session_key=? and r.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)`, sessionHandle).Scan(&sessionID, &hostID, &workspaceID, &providerKey, &argvJSON, &envJSON, &cwd)
	if err != nil {
		return CreatedRun{}, err
	}
	var argv []string
	if err := json.Unmarshal([]byte(argvJSON), &argv); err != nil {
		return CreatedRun{}, err
	}
	// The provider adapter owns the resume mechanism (e.g. Claude --continue).
	if resumed := provider.ContinueArgv(providerKey, argv); len(resumed) != len(argv) {
		argv = resumed
		raw, err := json.Marshal(argv)
		if err != nil {
			return CreatedRun{}, err
		}
		argvJSON = string(raw)
	}
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CreatedRun{}, err
	}
	defer tx.Rollback()
	if err := ensureWorkspaceRunnable(ctx, tx, workspaceID); err != nil {
		return CreatedRun{}, err
	}
	var seq int
	if err := tx.QueryRowContext(ctx, `select coalesce(max(run_seq), 0)+1 from agent_runs where session_id=?`, sessionID).Scan(&seq); err != nil {
		return CreatedRun{}, err
	}
	runID := NewID()
	tmuxID := NewID()
	tmuxKey := Handle("tmux_", tmuxID)
	targetJSON := `{"session":"agency-` + sessionHandle + `-run-` + strconv.Itoa(seq) + `","window":"main","pane":"agent"}`
	if _, err := tx.ExecContext(ctx, `insert into tmux_targets(tmux_target_id, tmux_target_key, host_id, tmux_server_id, target_spec_json, created_at)
		values(?, ?, ?, (select tmux_server_id from tmux_servers where tmux_server_key='default'), ?, ?)`, tmuxID, tmuxKey, hostID, targetJSON, now); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `update session_tmux_targets set detached_at=? where session_id=? and detached_at is null`, now, sessionID); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into session_tmux_targets(session_tmux_target_id, session_id, tmux_target_id, attached_at)
		values(?, ?, ?, ?)`, NewID(), sessionID, tmuxID, now); err != nil {
		return CreatedRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into agent_runs(run_id, session_id, run_seq, launch_argv_json, launch_env_json, working_directory, requested_at)
		values(?, ?, ?, ?, ?, ?, ?)`, runID, sessionID, seq, argvJSON, envJSON, cwd, now); err != nil {
		return CreatedRun{}, err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunRequested), `{"run":"`+fmtRun(seq)+`","session":"`+sessionHandle+`"}`); err != nil {
		return CreatedRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreatedRun{}, err
	}
	return CreatedRun{SessionID: sessionID, SessionKey: sessionHandle, Session: sessionHandle, RunID: runID, RunSeq: seq, Run: fmtRun(seq), TmuxTarget: tmuxKey, TmuxTargetID: tmuxID, Argv: argv, WorkingDir: cwd}, nil
}

func (s *Store) BindRunner(ctx context.Context, runID, tmuxTargetID, endpoint string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	var terminalCount int
	if err := tx.QueryRowContext(ctx, `select count(*) from run_terminal_outcomes where run_id=?`, runID).Scan(&terminalCount); err != nil {
		return err
	}
	if terminalCount > 0 {
		return errors.New("RunAlreadyTerminal")
	}
	if _, err := tx.ExecContext(ctx, `insert into active_runner_bindings(runner_binding_id, run_id, tmux_target_id, runner_endpoint_json, runner_protocol_version, runner_binary_version, last_heartbeat_at, bound_at)
		values(?, ?, ?, ?, 1, 'dev', ?, ?)`, NewID(), runID, tmuxTargetID, `{"kind":"UnixSocket","path":"`+endpoint+`"}`, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update agent_runs set started_at=? where run_id=?`, now, runID); err != nil {
		return err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunnerHeartbeatAccepted), `{"runnerProtocolVersion":1,"runnerBinaryVersion":"dev"}`); err != nil {
		return err
	}
	return tx.Commit()
}

// RunPromptTarget identifies a live run's tmux pane and provider, so the prompt
// detector can capture the rendered snapshot and classify it per provider.
type RunPromptTarget struct {
	RunID       string
	SessionName string
	ProviderKey string
}

// LiveRunPromptTargets lists runs that are bound, not terminal, in an open
// session, with an active tmux target — the runs whose rendered panes the prompt
// detector inspects.
func (s *Store) LiveRunPromptTargets(ctx context.Context) ([]RunPromptTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `select r.run_id, t.target_spec_json, p.provider_key
		from agent_runs r
		join agent_sessions s on s.session_id=r.session_id
		join active_runner_bindings b on b.run_id=r.run_id
		left join run_terminal_outcomes o on o.run_id=r.run_id
		join session_tmux_targets st on st.session_id=s.session_id and st.detached_at is null
		join tmux_targets t on t.tmux_target_id=st.tmux_target_id
		join agent_profile_revisions pr on pr.profile_revision_id=s.profile_revision_id
		join agent_providers p on p.provider_id=pr.provider_id
		where s.closed_at is null and o.run_terminal_outcome_id is null`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []RunPromptTarget
	for rows.Next() {
		var runID, targetSpec, providerKey string
		if err := rows.Scan(&runID, &targetSpec, &providerKey); err != nil {
			return nil, err
		}
		var spec map[string]any
		sessionName := ""
		if json.Unmarshal([]byte(targetSpec), &spec) == nil {
			if value, ok := spec["session"].(string); ok {
				sessionName = value
			}
		}
		if sessionName == "" {
			continue
		}
		targets = append(targets, RunPromptTarget{RunID: runID, SessionName: sessionName, ProviderKey: providerKey})
	}
	return targets, rows.Err()
}

// ZombieRun is a run whose start crashed between creating the tmux target and
// binding the runner: it has an active tmux target but no binding or outcome. The
// tmux target id lets reconciliation rebind if the runner is rediscovered alive.
type ZombieRun struct {
	RunID        string
	TmuxTargetID string
}

// ZombieRuns returns the latest runs of open sessions that have an active tmux
// target but neither a runner binding nor a terminal outcome, and whose start
// was requested before olderThan. The grace on requested_at excludes runs still
// mid-launch.
func (s *Store) ZombieRuns(ctx context.Context, olderThan string) ([]ZombieRun, error) {
	rows, err := s.DB.QueryContext(ctx, `select r.run_id, st.tmux_target_id
		from agent_runs r
		join agent_sessions s on s.session_id=r.session_id
		join session_tmux_targets st on st.session_id=s.session_id and st.detached_at is null
		left join active_runner_bindings b on b.run_id=r.run_id
		left join run_terminal_outcomes o on o.run_id=r.run_id
		where s.closed_at is null
			and r.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)
			and b.runner_binding_id is null
			and o.run_terminal_outcome_id is null
			and r.requested_at < ?
		order by r.requested_at`, olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var zombies []ZombieRun
	for rows.Next() {
		var z ZombieRun
		if err := rows.Scan(&z.RunID, &z.TmuxTargetID); err != nil {
			return nil, err
		}
		zombies = append(zombies, z)
	}
	return zombies, rows.Err()
}

// ManagedWorkspaceExists reports whether a workspace id has a managed_worktrees
// row, used to detect on-disk agency worktree markers that no longer (or never)
// had a managed worktree record.
func (s *Store) ManagedWorkspaceExists(ctx context.Context, workspaceID string) (bool, error) {
	var count int
	if err := s.DB.QueryRowContext(ctx, `select count(*) from managed_worktrees where workspace_id=?`, workspaceID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// RecordDoctorIssueObserved appends a DoctorIssueObserved event on the host
// subject. Used by the mutating reconciliation engine when it observes an issue
// (for example an adoptable orphan worktree) that has no owning domain subject.
func (s *Store) RecordDoctorIssueObserved(ctx context.Context, hostKey, payloadJSON string) error {
	if !json.Valid([]byte(payloadJSON)) {
		return errors.New("invalid doctor issue payload")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hostID string
	if err := tx.QueryRowContext(ctx, `select host_id from hosts where host_key=? and archived_at is null`, hostKey).Scan(&hostID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("HostNotFound")
		}
		return err
	}
	subjectID, err := ensureHostSubject(ctx, tx, hostID)
	if err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, subjectID, string(eventlog.DoctorIssueObserved), `{"kind":"Supervisor"}`, payloadJSON, hostID, ""); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkStartFailed(ctx context.Context, runID string, errText string) error {
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `delete from active_runner_bindings where run_id=?`, runID)
	_, _ = tx.ExecContext(ctx, `update tmux_targets set retired_at=? where retired_at is null and tmux_target_id in (
		select st.tmux_target_id
		from session_tmux_targets st
		join agent_runs r on r.session_id=st.session_id
		where r.run_id=? and st.detached_at is null
	)`, now, runID)
	_, _ = tx.ExecContext(ctx, `update session_tmux_targets set detached_at=? where detached_at is null and tmux_target_id in (
		select st.tmux_target_id
		from session_tmux_targets st
		join agent_runs r on r.session_id=st.session_id
		where r.run_id=?
	)`, now, runID)
	if _, err := tx.ExecContext(ctx, `insert into run_terminal_outcomes(run_terminal_outcome_id, run_id, outcome, termination_json, occurred_at)
		values(?, ?, 'StartFailed', ?, ?)`, NewID(), runID, `{"outcome":"StartFailed","failure":{"code":"SpawnFailed","detail":`+quoteJSON(errText)+`}}`, now); err != nil {
		return err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunFailed), `{"outcome":"StartFailed","detail":`+quoteJSON(errText)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListActiveRunnerBindings(ctx context.Context) ([]ActiveRunnerBinding, error) {
	rows, err := s.DB.QueryContext(ctx, `select b.run_id, ts.tmux_server_key, b.runner_endpoint_json, b.runner_protocol_version, b.runner_binary_version, b.last_heartbeat_at
		from active_runner_bindings b
		join tmux_targets t on t.tmux_target_id=b.tmux_target_id
		join tmux_servers ts on ts.tmux_server_id=t.tmux_server_id
		order by b.bound_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveRunnerBinding
	for rows.Next() {
		var binding ActiveRunnerBinding
		var endpointJSON, heartbeat string
		if err := rows.Scan(&binding.RunID, &binding.TmuxServerKey, &endpointJSON, &binding.ProtocolVersion, &binding.BinaryVersion, &heartbeat); err != nil {
			return nil, err
		}
		var endpoint struct {
			Kind string `json:"kind"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(endpointJSON), &endpoint); err != nil {
			return nil, err
		}
		if endpoint.Kind != "UnixSocket" || endpoint.Path == "" {
			return nil, errors.New("runner endpoint is not a UnixSocket path")
		}
		t, err := time.Parse(time.RFC3339Nano, heartbeat)
		if err != nil {
			return nil, err
		}
		binding.EndpointPath = endpoint.Path
		binding.LastHeartbeat = t
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *Store) TmuxServerIdentity(ctx context.Context, key string) (string, bool, error) {
	var raw sql.NullString
	err := s.DB.QueryRowContext(ctx, `select server_identity_json from tmux_servers where tmux_server_key=? and retired_at is null`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, errors.New("TmuxServerNotFound")
	}
	if err != nil || !raw.Valid {
		return "", false, err
	}
	return raw.String, true, nil
}

func (s *Store) RecordTmuxServerIdentity(ctx context.Context, key, identityJSON string) error {
	if !json.Valid([]byte(identityJSON)) {
		return errors.New("InvalidTmuxServerIdentity")
	}
	res, err := s.DB.ExecContext(ctx, `update tmux_servers set server_identity_json=? where tmux_server_key=? and retired_at is null`, identityJSON, key)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("TmuxServerNotFound")
	}
	return nil
}

func (s *Store) MarkTmuxServerRestarted(ctx context.Context, key, identityJSON, detail string) (int, error) {
	if identityJSON != "" && !json.Valid([]byte(identityJSON)) {
		return 0, errors.New("InvalidTmuxServerIdentity")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var serverID string
	if err := tx.QueryRowContext(ctx, `select tmux_server_id from tmux_servers where tmux_server_key=? and retired_at is null`, key).Scan(&serverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errors.New("TmuxServerNotFound")
		}
		return 0, err
	}
	now := Now()
	rows, err := tx.QueryContext(ctx, `select distinct r.run_id
		from agent_runs r
		join agent_sessions s on s.session_id=r.session_id
		join session_tmux_targets st on st.session_id=s.session_id and st.detached_at is null
		join tmux_targets t on t.tmux_target_id=st.tmux_target_id
		left join run_terminal_outcomes o on o.run_id=r.run_id
		where t.tmux_server_id=? and o.run_terminal_outcome_id is null
		order by r.requested_at`, serverID)
	if err != nil {
		return 0, err
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return 0, err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, runID := range runIDs {
		if _, err := tx.ExecContext(ctx, `delete from active_runner_bindings where run_id=?`, runID); err != nil {
			return 0, err
		}
		if err := deleteRunStatusSnapshot(ctx, tx, runID); err != nil {
			return 0, err
		}
		payload := `{"outcome":"Orphaned","detail":` + quoteJSON(detail) + `}`
		if _, err := tx.ExecContext(ctx, `insert into run_terminal_outcomes(run_terminal_outcome_id, run_id, outcome, termination_json, occurred_at)
			values(?, ?, 'Orphaned', ?, ?)`, NewID(), runID, payload, now); err != nil {
			return 0, err
		}
		if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunFailed), payload); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `update session_tmux_targets set detached_at=? where detached_at is null and tmux_target_id in (
		select tmux_target_id from tmux_targets where tmux_server_id=?
	)`, now, serverID)
	if err != nil {
		return 0, err
	}
	if _, err := res.RowsAffected(); err != nil {
		return 0, err
	}
	res, err = tx.ExecContext(ctx, `update tmux_targets set retired_at=? where retired_at is null and tmux_server_id=?`, now, serverID)
	if err != nil {
		return 0, err
	}
	if _, err := res.RowsAffected(); err != nil {
		return 0, err
	}
	var identity any
	if identityJSON != "" {
		identity = identityJSON
	}
	if _, err := tx.ExecContext(ctx, `update tmux_servers set server_identity_json=? where tmux_server_id=?`, identity, serverID); err != nil {
		return 0, err
	}
	if len(runIDs) > 0 {
		subjectID, err := ensureTmuxServerSubject(ctx, tx, key)
		if err != nil {
			return 0, err
		}
		if err := insertEvent(ctx, tx, subjectID, string(eventlog.TmuxServerRestarted), `{"kind":"Supervisor"}`, `{"tmuxServer":`+quoteJSON(key)+`,"detail":`+quoteJSON(detail)+`,"orphanedRuns":`+strconv.Itoa(len(runIDs))+`}`, serverID, ""); err != nil {
			return 0, err
		}
	}
	return len(runIDs), tx.Commit()
}

// RefreshRunnerHeartbeat updates only the wall-clock last_heartbeat_at (display
// and audit) for an already-adopted binding on a periodic reconcile pass. Unlike
// MarkRunnerAdopted it emits no RunnerAdopted event and does not touch the status
// snapshot, so a live prompt hint (NeedsInput/NeedsApproval) is never clobbered
// and the event log is not spammed once per reconcile interval. A missing
// binding (a run that went terminal concurrently) is a benign no-op.
func (s *Store) RefreshRunnerHeartbeat(ctx context.Context, runID string) error {
	_, err := s.DB.ExecContext(ctx, `update active_runner_bindings set last_heartbeat_at=? where run_id=?`, Now(), runID)
	return err
}

func (s *Store) MarkRunnerAdopted(ctx context.Context, runID string, protocolVersion int, binaryVersion string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	res, err := tx.ExecContext(ctx, `update active_runner_bindings
		set runner_protocol_version=?, runner_binary_version=?, last_heartbeat_at=?
		where run_id=?`, protocolVersion, binaryVersion, now, runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("ActiveRunnerBindingNotFound")
	}
	if err := deleteRunStatusSnapshot(ctx, tx, runID); err != nil {
		return err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunnerAdopted), `{"runnerProtocolVersion":`+strconv.Itoa(protocolVersion)+`,"runnerBinaryVersion":`+quoteJSON(binaryVersion)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordRunnerHeartbeat(ctx context.Context, runID string) error {
	res, err := s.DB.ExecContext(ctx, `update active_runner_bindings set last_heartbeat_at=? where run_id=?`, Now(), runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("ActiveRunnerBindingNotFound")
	}
	return nil
}

func (s *Store) MarkRunnerQuarantined(ctx context.Context, runID string, protocolVersion int, binaryVersion, detail string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `delete from active_runner_bindings where run_id=?`, runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("ActiveRunnerBindingNotFound")
	}
	if err := insertRunStatusSnapshot(ctx, tx, runID, "Reconciliation", `{"runStatus":"RepairRequired","detail":`+quoteJSON(detail)+`}`); err != nil {
		return err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunnerQuarantined), `{"runnerProtocolVersion":`+strconv.Itoa(protocolVersion)+`,"runnerBinaryVersion":`+quoteJSON(binaryVersion)+`,"detail":`+quoteJSON(detail)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkRunnerOrphaned(ctx context.Context, runID string, detail string) error {
	now := Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `delete from active_runner_bindings where run_id=?`, runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("ActiveRunnerBindingNotFound")
	}
	if err := deleteRunStatusSnapshot(ctx, tx, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into run_terminal_outcomes(run_terminal_outcome_id, run_id, outcome, termination_json, occurred_at)
		values(?, ?, 'Orphaned', ?, ?)`, NewID(), runID, `{"outcome":"Orphaned","detail":`+quoteJSON(detail)+`}`, now); err != nil {
		return err
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RunFailed), `{"outcome":"Orphaned","detail":`+quoteJSON(detail)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) NextOutputChunkSeq(ctx context.Context, runID string) (int64, error) {
	var seq int64
	err := s.DB.QueryRowContext(ctx, `select coalesce(max(chunk_seq), 0)+1 from runner_output_chunks where run_id=?`, runID).Scan(&seq)
	return seq, err
}

func (s *Store) AppendOutputChunk(ctx context.Context, runID string, chunkSeq int64, stream string, bytes []byte) error {
	if stream != "Stdout" && stream != "Stderr" {
		return errors.New("InvalidOutputStream")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRowContext(ctx, `select count(*) from runner_output_chunks where run_id=? and chunk_seq=?`, runID, chunkSeq).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return tx.Commit()
	}
	payload, _ := json.Marshal(map[string]any{"kind": "Inline", "bytesBase64": base64.StdEncoding.EncodeToString(bytes)})
	if _, err := tx.ExecContext(ctx, `insert into runner_output_chunks(runner_output_chunk_id, run_id, chunk_seq, stream, content_ref_json, captured_at)
		values(?, ?, ?, ?, ?, ?)`, NewID(), runID, chunkSeq, stream, string(payload), Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordPromptHint(ctx context.Context, runID, status, detail string) error {
	if status != "NeedsInput" && status != "NeedsApproval" {
		return errors.New("InvalidPromptStatus")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := latestRunStatusSnapshot(ctx, tx, runID)
	if err != nil {
		return err
	}
	if current == status {
		return tx.Commit()
	}
	if err := insertRunStatusSnapshot(ctx, tx, runID, "Event", `{"runStatus":"`+status+`","source":"PtyOutput","detail":`+quoteJSON(detail)+`}`); err != nil {
		return err
	}
	eventType := string(eventlog.RunNeedsInput)
	if status == "NeedsApproval" {
		eventType = string(eventlog.RunNeedsApproval)
	}
	if err := insertRunEvent(ctx, tx, runID, eventType, `{"runStatus":"`+status+`","source":"PtyOutput","detail":`+quoteJSON(detail)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearPromptHint removes a stale NeedsInput/NeedsApproval pin once the pane no
// longer shows that prompt, reverting the run to its derived Live/Quiet status.
// It is a no-op unless the current snapshot is a prompt state, so it never
// disturbs a RepairRequired or reconciliation snapshot. The historical
// status_snapshots row and the RunNeedsInput/Approval event are retained for
// audit; only the "latest" pointer is dropped so derivation resumes.
func (s *Store) ClearPromptHint(ctx context.Context, runID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := latestRunStatusSnapshot(ctx, tx, runID)
	if err != nil {
		return err
	}
	if current != "NeedsInput" && current != "NeedsApproval" {
		return tx.Commit()
	}
	if err := deleteRunStatusSnapshot(ctx, tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListOutputChunks(ctx context.Context, runID string, fromChunkSeq int64) ([]OutputChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `select chunk_seq, stream, content_ref_json, captured_at
		from runner_output_chunks where run_id=? and chunk_seq>=? order by chunk_seq`, runID, fromChunkSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutputChunk
	for rows.Next() {
		var chunk OutputChunk
		var contentRef string
		chunk.RunID = runID
		if err := rows.Scan(&chunk.ChunkSeq, &chunk.Stream, &contentRef, &chunk.CapturedAt); err != nil {
			return nil, err
		}
		var ref struct {
			Kind        string `json:"kind"`
			BytesBase64 string `json:"bytesBase64"`
		}
		if err := json.Unmarshal([]byte(contentRef), &ref); err != nil {
			return nil, err
		}
		if ref.Kind != "Inline" {
			return nil, errors.New("output content_ref_json is not Inline")
		}
		bytes, err := base64.StdEncoding.DecodeString(ref.BytesBase64)
		if err != nil {
			return nil, err
		}
		chunk.Bytes = bytes
		out = append(out, chunk)
	}
	return out, rows.Err()
}

func (s *Store) SessionOutput(ctx context.Context, sessionHandle string, fromChunkSeq int64) ([]OutputChunk, error) {
	detail, err := s.Session(ctx, sessionHandle)
	if err != nil {
		return nil, err
	}
	if detail.RunID == "" {
		return nil, errors.New("RunNotFound")
	}
	return s.ListOutputChunks(ctx, detail.RunID, fromChunkSeq)
}

func (s *Store) ListSessionEvents(ctx context.Context, sessionHandle string) ([]Event, error) {
	var sessionID string
	if err := s.DB.QueryRowContext(ctx, `select session_id from agent_sessions where session_key=?`, sessionHandle).Scan(&sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("SessionNotFound")
		}
		return nil, err
	}
	runHandles := map[string]string{}
	runRows, err := s.DB.QueryContext(ctx, `select run_id, run_seq from agent_runs where session_id=?`, sessionID)
	if err != nil {
		return nil, err
	}
	for runRows.Next() {
		var runID string
		var runSeq int
		if err := runRows.Scan(&runID, &runSeq); err != nil {
			runRows.Close()
			return nil, err
		}
		runHandles[runID] = fmtRun(runSeq)
	}
	if err := runRows.Close(); err != nil {
		return nil, err
	}
	if err := runRows.Err(); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `select e.event_id, e.event_seq, e.occurred_at, e.event_type, ss.subject_hash, e.actor_json, e.payload_json, e.correlation_json
		from events e
		join status_subjects ss on ss.status_subject_id=e.status_subject_id
		where ss.subject_hash=? or ss.subject_hash in (select 'Run:' || run_id from agent_runs where session_id=?)
		order by e.occurred_at, e.event_id`, "Session:"+sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var event Event
		var subjectHash, actorJSON, payloadJSON, correlationJSON string
		if err := rows.Scan(&event.EventID, &event.EventSeq, &event.OccurredAt, &event.EventType, &subjectHash, &actorJSON, &payloadJSON, &correlationJSON); err != nil {
			return nil, err
		}
		if !eventlog.Valid(event.EventType) {
			return nil, errors.New("unknown event type: " + event.EventType)
		}
		event.Label = eventlog.Label(event.EventType)
		actor, err := decodeJSONObject(actorJSON)
		if err != nil {
			return nil, err
		}
		payload, err := decodeJSONObject(payloadJSON)
		if err != nil {
			return nil, err
		}
		correlation, err := decodeJSONObject(correlationJSON)
		if err != nil {
			return nil, err
		}
		event.Subject = publicEventSubject(subjectHash, sessionID, sessionHandle, runHandles)
		event.Actor = actor
		event.Payload = payload
		event.Correlation = correlation
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	rows, err := s.DB.QueryContext(ctx, `select s.session_key, s.title, pr.project_key, w.workspace_key, w.path, p.provider_key, coalesce(m.model_key, ''), coalesce(r.effort_key, ''),
		s.closed_at, o.outcome, ss.snapshot_json, b.runner_binding_id is not null, coalesce(le.last_event_at, s.created_at), coalesce(ar.run_id, ''), w.workspace_id,
		b.last_heartbeat_at, b.bound_at, lo.last_output_at
		from agent_sessions s
		join projects pr on pr.project_id=s.project_id
		join workspaces w on w.workspace_id=s.workspace_id
		join agent_profile_revisions r on r.profile_revision_id=s.profile_revision_id
		join agent_providers p on p.provider_id=r.provider_id
		left join provider_models m on m.provider_model_id=r.provider_model_id
		left join agent_runs ar on ar.session_id=s.session_id and ar.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)
		left join active_runner_bindings b on b.run_id=ar.run_id
		left join run_terminal_outcomes o on o.run_id=ar.run_id
		left join status_subjects rs on rs.subject_hash='Run:' || ar.run_id
		left join latest_status_snapshots lss on lss.status_subject_id=rs.status_subject_id
		left join status_snapshots ss on ss.status_snapshot_id=lss.status_snapshot_id
		left join (
			select session_id, max(occurred_at) last_event_at from (
				select s2.session_id, e.occurred_at
				from agent_sessions s2
				join status_subjects subject on subject.subject_hash='Session:' || s2.session_id
				join events e on e.status_subject_id=subject.status_subject_id
				union all
				select r2.session_id, e.occurred_at
				from agent_runs r2
				join status_subjects subject on subject.subject_hash='Run:' || r2.run_id
				join events e on e.status_subject_id=subject.status_subject_id
			) group by session_id
		) le on le.session_id=s.session_id
		left join (
			select run_id, max(captured_at) last_output_at
			from runner_output_chunks
			group by run_id
		) lo on lo.run_id=ar.run_id
		order by s.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var session, title, project, workspace, path, provider, model, effort, created, runID, workspaceID string
		var closedAt, outcome, snapshot, heartbeatAt, boundAt, outputAt sql.NullString
		var live bool
		if err := rows.Scan(&session, &title, &project, &workspace, &path, &provider, &model, &effort, &closedAt, &outcome, &snapshot, &live, &created, &runID, &workspaceID, &heartbeatAt, &boundAt, &outputAt); err != nil {
			return nil, err
		}
		status := deriveRunStatus(s.quietThreshold, closedAt, outcome, snapshot, live, heartbeatAt, boundAt, outputAt)
		out = append(out, SessionSummary{
			Session: session, Title: title, Provider: provider, Project: project, Workspace: Handle("wks_", workspaceID),
			RunStatus: status, Git: map[string]any{"summary": "Clean", "presence": "Present", "tree": "Clean", "untracked": false, "ignoredUserFiles": false, "conflicts": false, "upstream": "Current"},
			Model: model, Effort: effort, Close: map[string]any{"closable": !activeRunStatus(status), "summary": closeSummary(status), "blockers": closeBlockers(status)},
			LastEvent: created, WorkspaceKey: workspace, Path: path, RunID: runID, WorkspaceID: workspaceID, HasActiveBinding: live,
		})
	}
	return out, rows.Err()
}

func (s *Store) Session(ctx context.Context, handle string) (SessionDetail, error) {
	summaries, err := s.ListSessions(ctx)
	if err != nil {
		return SessionDetail{}, err
	}
	for _, summary := range summaries {
		if summary.Session != handle {
			continue
		}
		var id string
		var runSeq int
		var targetKey, targetSpec, runnerEndpoint, argvJSON sql.NullString
		err := s.DB.QueryRowContext(ctx, `select s.session_id, coalesce(r.run_seq, 0), t.tmux_target_key, t.target_spec_json, b.runner_endpoint_json, r.launch_argv_json
				from agent_sessions s
				left join session_tmux_targets st on st.session_id=s.session_id and st.detached_at is null
				left join tmux_targets t on t.tmux_target_id=st.tmux_target_id
				left join agent_runs r on r.session_id=s.session_id and r.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)
				left join active_runner_bindings b on b.run_id=r.run_id
				where s.session_key=?`, handle).Scan(&id, &runSeq, &targetKey, &targetSpec, &runnerEndpoint, &argvJSON)
		if err != nil {
			return SessionDetail{}, err
		}
		var argv []string
		if argvJSON.Valid {
			if err := json.Unmarshal([]byte(argvJSON.String), &argv); err != nil {
				return SessionDetail{}, err
			}
		}
		tmuxName := "agency-" + handle
		if targetSpec.Valid {
			var spec map[string]any
			if json.Unmarshal([]byte(targetSpec.String), &spec) == nil {
				if value, ok := spec["session"].(string); ok && value != "" {
					tmuxName = value
				}
			}
		}
		runnerSocket := ""
		if runnerEndpoint.Valid {
			var endpoint map[string]string
			if json.Unmarshal([]byte(runnerEndpoint.String), &endpoint) == nil {
				runnerSocket = endpoint["path"]
			}
		}
		run := ""
		if runSeq > 0 {
			run = fmtRun(runSeq)
		}
		return SessionDetail{Summary: summary, ID: id, RunID: summary.RunID, Run: run, Argv: argv, TmuxTargetKey: targetKey.String, TmuxSessionName: tmuxName, RunnerSocket: runnerSocket}, nil
	}
	return SessionDetail{}, errors.New("SessionNotFound")
}

func (s *Store) RenameSession(ctx context.Context, handle, title string) error {
	res, err := s.DB.ExecContext(ctx, `update agent_sessions set title=? where session_key=? and closed_at is null`, title, handle)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("SessionNotFound")
	}
	return nil
}

func (s *Store) StopRun(ctx context.Context, handle string) error {
	detail, err := s.Session(ctx, handle)
	if err != nil {
		return err
	}
	if detail.RunID == "" {
		return errors.New("RunNotLive")
	}
	if detail.RunnerSocket == "" {
		switch detail.Summary.RunStatus {
		case "Exited", "Stopped", "Killed", "Failed":
			return nil
		}
		return errors.New("RunNotLive")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	_, _ = tx.ExecContext(ctx, `delete from active_runner_bindings where run_id=?`, detail.RunID)
	if err := insertRunEvent(ctx, tx, detail.RunID, string(eventlog.StopRequested), `{"session":"`+detail.Summary.Session+`"}`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `insert into run_terminal_outcomes(run_terminal_outcome_id, run_id, outcome, termination_json, occurred_at)
		values(?, ?, 'UserStopped', '{"outcome":"UserStopped"}', ?) on conflict(run_id) do nothing`, NewID(), detail.RunID, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		causation, _, err := latestRunEventID(ctx, tx, detail.RunID, string(eventlog.StopRequested))
		if err != nil {
			return err
		}
		if err := insertRunEventCaused(ctx, tx, detail.RunID, string(eventlog.RunStopped), `{"outcome":"UserStopped"}`, causation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecordStopRequested(ctx context.Context, runID, session string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.StopRequested), `{"session":"`+session+`"}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) KillRun(ctx context.Context, handle string) error {
	detail, err := s.Session(ctx, handle)
	if err != nil {
		return err
	}
	if detail.RunID == "" {
		return errors.New("RunNotLive")
	}
	// Killing an already-terminated run is an idempotent no-op: the process is
	// already gone, which is exactly what kill wants. Recording UserKilled over
	// an existing outcome (for example ProviderExited when the agent exited on
	// its own just before the kill) would surface a TerminalOutcomeConflict for
	// what is really success.
	var terminal int
	if err := s.DB.QueryRowContext(ctx, `select count(*) from run_terminal_outcomes where run_id=?`, detail.RunID).Scan(&terminal); err != nil {
		return err
	}
	if terminal > 0 {
		return nil
	}
	return s.terminalOutcome(ctx, detail.RunID, "UserKilled", `{"outcome":"UserKilled","signal":"SIGKILL"}`)
}

func (s *Store) CloseSession(ctx context.Context, handle string) error {
	detail, err := s.Session(ctx, handle)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	attemptID := NewID()
	now := Now()
	if _, err := tx.ExecContext(ctx, `insert into close_attempts(close_attempt_id, close_attempt_key, target_json, requested_by_actor_json, requested_at)
		values(?, ?, ?, '{"kind":"User"}', ?)`, attemptID, Handle("cls_", attemptID), sessionTargetJSON(detail), now); err != nil {
		return err
	}
	if activeRunStatus(detail.Summary.RunStatus) {
		if _, err := tx.ExecContext(ctx, `insert into close_blockers(close_blocker_id, close_attempt_id, blocker, summary, evidence_json, created_at)
			values(?, ?, 'SessionStillRunning', 'StopRequired', ?, ?)`, NewID(), attemptID, `{"session":"`+detail.Summary.Session+`","runId":"`+detail.RunID+`"}`, now); err != nil {
			return err
		}
		if err := insertSessionEvent(ctx, tx, detail.ID, string(eventlog.CloseAttemptStarted), `{"target":"Session","session":"`+detail.Summary.Session+`"}`); err != nil {
			return err
		}
		if err := insertSessionEvent(ctx, tx, detail.ID, string(eventlog.CloseBlocked), `{"target":"Session","session":"`+detail.Summary.Session+`","blockers":["SessionStillRunning"]}`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("SessionStillRunning")
	}
	res, err := tx.ExecContext(ctx, `update agent_sessions set closed_at=? where session_key=? and closed_at is null`, now, handle)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("SessionNotFound")
	}
	if err := insertSessionEvent(ctx, tx, detail.ID, string(eventlog.CloseAttemptStarted), `{"target":"Session","session":"`+detail.Summary.Session+`"}`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update close_attempts set completed_at=? where close_attempt_id=?`, now, attemptID); err != nil {
		return err
	}
	if err := insertSessionEvent(ctx, tx, detail.ID, string(eventlog.SessionClosed), `{"session":"`+detail.Summary.Session+`"}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CloseTerminalSessions(ctx context.Context) (int, error) {
	res, err := s.DB.ExecContext(ctx, `update agent_sessions set closed_at=? where closed_at is null and session_id in (
		select s.session_id from agent_sessions s
		join agent_runs r on r.session_id=s.session_id
		join run_terminal_outcomes o on o.run_id=r.run_id
		where r.run_seq=(select max(run_seq) from agent_runs where session_id=s.session_id)
	)`, Now())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) terminalOutcome(ctx context.Context, runID, outcome, payload string) error {
	if err := validateTerminalOutcome(outcome, payload); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingOutcome, existingPayload string
	err = tx.QueryRowContext(ctx, `select outcome, termination_json from run_terminal_outcomes where run_id=?`, runID).Scan(&existingOutcome, &existingPayload)
	if err == nil {
		if existingOutcome == outcome && existingPayload == payload {
			return tx.Commit()
		}
		return errors.New("TerminalOutcomeConflict")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, _ = tx.ExecContext(ctx, `delete from active_runner_bindings where run_id=?`, runID)
	_, err = tx.ExecContext(ctx, `insert into run_terminal_outcomes(run_terminal_outcome_id, run_id, outcome, termination_json, occurred_at)
		values(?, ?, ?, ?, ?)`, NewID(), runID, outcome, payload, Now())
	if err != nil {
		return err
	}
	eventType, err := eventTypeForOutcome(outcome)
	if err != nil {
		return err
	}
	// A user-initiated terminal outcome is caused by the preceding stop request;
	// link them so the operation can be traced.
	causation := ""
	if outcome == "UserStopped" || outcome == "UserKilled" {
		if id, ok, err := latestRunEventID(ctx, tx, runID, string(eventlog.StopRequested)); err != nil {
			return err
		} else if ok {
			causation = id
		}
	}
	if err := insertRunEventCaused(ctx, tx, runID, eventType, payload, causation); err != nil {
		return err
	}
	return tx.Commit()
}

func validateTerminalOutcome(outcome, payload string) error {
	if _, err := eventTypeForOutcome(outcome); err != nil {
		return err
	}
	var row struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		return err
	}
	if row.Outcome != outcome {
		return errors.New("TerminalOutcomePayloadMismatch")
	}
	return nil
}

func (s *Store) RecordTerminalOutcome(ctx context.Context, runID, outcome, payload string) error {
	if runID == "" {
		return errors.New("run id is required")
	}
	if outcome == "" {
		return errors.New("terminal outcome is required")
	}
	return s.terminalOutcome(ctx, runID, outcome, payload)
}

func (s *Store) RecordRunRepairCompleted(ctx context.Context, runID, payload string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.RepairCompleted), payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddInputEvent(ctx context.Context, runID string, input []byte) (int, error) {
	payload, _ := json.Marshal(map[string]any{"kind": "Inline", "bytesBase64": base64.StdEncoding.EncodeToString(input)})
	var seq int
	if err := s.DB.QueryRowContext(ctx, `insert into run_input_events(run_input_event_id, run_id, input_seq, input_ref_json, delivery_json, created_at)
		select ?, ?, coalesce(max(input_seq), 0)+1, ?, '{"state":"Pending"}', ? from run_input_events where run_id=?
		returning input_seq`, NewID(), runID, string(payload), Now(), runID).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *Store) MarkInputAccepted(ctx context.Context, runID string, seq int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `update run_input_events set delivery_json=? where run_id=? and input_seq=?`, `{"state":"Accepted","at":"`+Now()+`"}`, runID, seq)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("InputEventNotFound")
	}
	current, err := latestRunStatusSnapshot(ctx, tx, runID)
	if err != nil {
		return err
	}
	if current == "NeedsInput" || current == "NeedsApproval" {
		if err := deleteRunStatusSnapshot(ctx, tx, runID); err != nil {
			return err
		}
	}
	if err := insertRunEvent(ctx, tx, runID, string(eventlog.InputAccepted), `{"inputSeq":`+strconv.Itoa(seq)+`}`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkInputFailed(ctx context.Context, runID string, seq int, detail string) error {
	_, err := s.DB.ExecContext(ctx, `update run_input_events set delivery_json=? where run_id=? and input_seq=?`, `{"state":"Failed","at":"`+Now()+`","failure":{"code":"RunnerInputFailed","detail":`+quoteJSON(detail)+`}}`, runID, seq)
	return err
}

// PendingNotification is one (channel, event) delivery the worker still owes.
type PendingNotification struct {
	ChannelID    string
	ChannelKey   string
	ChannelType  string // Terminal | Desktop
	EventID      string
	EventType    string
	SessionKey   string // best-effort human subject; "" if not a session/run event
	SessionTitle string // session title, for content-design bodies
	Workspace    string // workspace key of the session, for close/location context
	Provider     string // provider display name, for actor phrasing
	Payload      string // event payload_json, carries failure/blocker detail
	AttemptSeq   int    // the attempt number to record for the next try
}

// SyncNotificationChannels makes notification_channels match the configured set:
// config is authoritative. Each configured channel is upserted by key (type and
// event spec refreshed, re-enabled), and any channel no longer configured is
// soft-disabled so it stops delivering without losing its delivery history. A
// configured Desktop channel is therefore reachable by the worker, not inert.
func (s *Store) SyncNotificationChannels(ctx context.Context, channels map[string]config.Notification) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	configured := make([]string, 0, len(channels))
	for key, ch := range channels {
		configured = append(configured, key)
		spec, err := json.Marshal(map[string]any{"events": ch.Events})
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `update notification_channels
			set channel_type=?, channel_spec_json=?, disabled_at=null where notification_channel_key=?`,
			ch.Type, string(spec), key)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `insert into notification_channels(notification_channel_id, notification_channel_key, channel_type, channel_spec_json, created_at)
				values(?, ?, ?, ?, ?)`, NewID(), key, ch.Type, string(spec), now); err != nil {
				return err
			}
		}
	}
	if len(configured) == 0 {
		if _, err := tx.ExecContext(ctx, `update notification_channels set disabled_at=? where disabled_at is null`, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	args := make([]any, 0, len(configured)+1)
	args = append(args, now)
	for _, k := range configured {
		args = append(args, k)
	}
	q := `update notification_channels set disabled_at=? where disabled_at is null and notification_channel_key not in (?` +
		strings.Repeat(", ?", len(configured)-1) + `)`
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingNotifications returns undelivered (channel, event) pairs whose event
// type is in the channel's configured event set and whose delivery is neither
// delivered nor terminally failed and has not exhausted maxAttempts. It reads
// state only; the worker performs delivery and records the outcome.
func (s *Store) PendingNotifications(ctx context.Context, maxAttempts int) ([]PendingNotification, error) {
	channels, err := s.DB.QueryContext(ctx, `select notification_channel_id, notification_channel_key, channel_type, channel_spec_json from notification_channels where disabled_at is null`)
	if err != nil {
		return nil, err
	}
	type channel struct {
		id, key, kind string
		events        map[string]bool
	}
	var configured []channel
	for channels.Next() {
		var id, key, kind, rawSpec string
		if err := channels.Scan(&id, &key, &kind, &rawSpec); err != nil {
			channels.Close()
			return nil, err
		}
		var spec struct {
			Events []string `json:"events"`
		}
		if err := json.Unmarshal([]byte(rawSpec), &spec); err != nil {
			channels.Close()
			return nil, err
		}
		set := map[string]bool{}
		for _, e := range spec.Events {
			set[e] = true
		}
		configured = append(configured, channel{id: id, key: key, kind: kind, events: set})
	}
	if err := channels.Close(); err != nil {
		return nil, err
	}
	if err := channels.Err(); err != nil {
		return nil, err
	}

	var pending []PendingNotification
	for _, ch := range configured {
		rows, err := s.DB.QueryContext(ctx, `select e.event_id, e.event_type, coalesce(d.attempt_seq, 0),
				coalesce(sess.session_key, runsess.session_key, ''),
				coalesce(sess.title, runsess.title, ''),
				coalesce(sw.workspace_key, rw.workspace_key, ''),
				coalesce(sp.display_name, rp.display_name, ''),
				e.payload_json
			from events e
			left join notification_deliveries d on d.event_id=e.event_id and d.notification_channel_id=?
			left join status_subjects subj on subj.status_subject_id=e.status_subject_id
			left join agent_sessions sess on subj.subject_hash='Session:' || sess.session_id
			left join agent_runs run on subj.subject_hash='Run:' || run.run_id
			left join agent_sessions runsess on runsess.session_id=run.session_id
			left join workspaces sw on sw.workspace_id=sess.workspace_id
			left join workspaces rw on rw.workspace_id=runsess.workspace_id
			left join agent_profile_revisions srev on srev.profile_revision_id=sess.profile_revision_id
			left join agent_providers sp on sp.provider_id=srev.provider_id
			left join agent_profile_revisions rrev on rrev.profile_revision_id=runsess.profile_revision_id
			left join agent_providers rp on rp.provider_id=rrev.provider_id
			where d.delivered_at is null and d.failed_at is null and coalesce(d.attempt_seq, 0) < ?
			order by e.occurred_at asc`, ch.id, maxAttempts)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eventID, eventType, sessionKey, title, workspace, providerName, payload string
			var attemptSeq int
			if err := rows.Scan(&eventID, &eventType, &attemptSeq, &sessionKey, &title, &workspace, &providerName, &payload); err != nil {
				rows.Close()
				return nil, err
			}
			if !ch.events[eventType] {
				continue
			}
			pending = append(pending, PendingNotification{
				ChannelID: ch.id, ChannelKey: ch.key, ChannelType: ch.kind,
				EventID: eventID, EventType: eventType, SessionKey: sessionKey,
				SessionTitle: title, Workspace: workspace, Provider: providerName, Payload: payload,
				AttemptSeq: attemptSeq + 1,
			})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return pending, nil
}

// RecordNotificationDelivered upserts the delivery row as delivered at attemptSeq.
func (s *Store) RecordNotificationDelivered(ctx context.Context, channelID, eventID string, attemptSeq int) error {
	now := Now()
	_, err := s.DB.ExecContext(ctx, `insert into notification_deliveries(notification_delivery_id, notification_channel_id, event_id, attempt_seq, attempted_at, delivered_at)
		values(?, ?, ?, ?, ?, ?)
		on conflict(notification_channel_id, event_id) do update set attempt_seq=excluded.attempt_seq, attempted_at=excluded.attempted_at, delivered_at=excluded.delivered_at, failed_at=null, failure_json=null`,
		NewID(), channelID, eventID, attemptSeq, now, now)
	return err
}

// RecordNotificationFailed upserts the delivery row with the failed attempt.
// terminal marks the delivery as permanently failed (retry budget exhausted).
func (s *Store) RecordNotificationFailed(ctx context.Context, channelID, eventID string, attemptSeq int, failureJSON string, terminal bool) error {
	if !json.Valid([]byte(failureJSON)) {
		return errors.New("invalid notification failure JSON")
	}
	now := Now()
	// Schema invariant: failure_json is present iff failed_at is present. A
	// non-terminal (retryable) attempt records neither; only exhaustion sets both.
	failedAt := any(nil)
	failure := any(nil)
	if terminal {
		failedAt = now
		failure = failureJSON
	}
	_, err := s.DB.ExecContext(ctx, `insert into notification_deliveries(notification_delivery_id, notification_channel_id, event_id, attempt_seq, attempted_at, failed_at, failure_json)
		values(?, ?, ?, ?, ?, ?, ?)
		on conflict(notification_channel_id, event_id) do update set attempt_seq=excluded.attempt_seq, attempted_at=excluded.attempted_at, failed_at=excluded.failed_at, failure_json=excluded.failure_json`,
		NewID(), channelID, eventID, attemptSeq, now, failedAt, failure)
	return err
}

// PruneResult reports how many rows each retention sweep removed.
type PruneResult struct {
	IdempotencyKeys        int `json:"idempotencyKeys"`
	NotificationDeliveries int `json:"notificationDeliveries"`
	CloseAttempts          int `json:"closeAttempts"`
	SafetyCheckRuns        int `json:"safetyCheckRuns"`
	OutputChunks           int `json:"outputChunks"`
	StatusSnapshots        int `json:"statusSnapshots"`
	Events                 int `json:"events"`
}

// Prune enforces the configured retention windows, deleting past-retention rows
// child-before-parent in one serializable transaction, then checkpointing the
// WAL and running incremental vacuum. Events are append-only in the write path
// (never mutated in place) but age out per event_retention_days: only events of
// closed sessions and terminal runs are pruned, so a live run's projection and
// audit trail are never truncated. Timestamps are ISO-8601 UTC text, so string
// comparison against a formatted cutoff is correct.
func (s *Store) Prune(ctx context.Context, retention config.Retention) (PruneResult, error) {
	now := time.Now().UTC()
	cutoff := func(days int) string {
		if days <= 0 {
			days = 1
		}
		return now.AddDate(0, 0, -days).Format("2006-01-02T15:04:05.000Z")
	}
	idempCut := cutoff(retention.IdempotencyRetentionDays)
	eventCut := cutoff(retention.EventRetentionDays)
	outputCut := cutoff(retention.OutputRetentionDays)

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return PruneResult{}, err
	}
	defer tx.Rollback()

	var result PruneResult
	del := func(dst *int, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if dst != nil {
			*dst += int(n)
		}
		return nil
	}

	if err := del(&result.IdempotencyKeys, `delete from idempotency_keys where state='Completed' and completed_at is not null and completed_at < ?`, idempCut); err != nil {
		return PruneResult{}, err
	}
	if err := del(&result.NotificationDeliveries, `delete from notification_deliveries where (delivered_at is not null and delivered_at < ?) or (failed_at is not null and failed_at < ?)`, idempCut, idempCut); err != nil {
		return PruneResult{}, err
	}
	// close_blockers (child) before close_attempts (parent). A close_attempt that
	// still backs a closing_worktrees teardown row is live state and must not be
	// pruned (its FK child would otherwise abort the whole sweep).
	if err := del(nil, `delete from close_blockers where close_attempt_id in (
		select close_attempt_id from close_attempts
		where coalesce(completed_at, failed_at) is not null and coalesce(completed_at, failed_at) < ?
			and close_attempt_id not in (select close_attempt_id from closing_worktrees))`, eventCut); err != nil {
		return PruneResult{}, err
	}
	if err := del(&result.CloseAttempts, `delete from close_attempts
		where coalesce(completed_at, failed_at) is not null and coalesce(completed_at, failed_at) < ?
			and close_attempt_id not in (select close_attempt_id from closing_worktrees)`, eventCut); err != nil {
		return PruneResult{}, err
	}
	// safety_check_findings (child) before safety_check_runs (parent).
	if err := del(nil, `delete from safety_check_findings where safety_check_run_id in (
		select safety_check_run_id from safety_check_runs where coalesce(completed_at, failed_at) is not null and coalesce(completed_at, failed_at) < ?)`, eventCut); err != nil {
		return PruneResult{}, err
	}
	if err := del(&result.SafetyCheckRuns, `delete from safety_check_runs where coalesce(completed_at, failed_at) is not null and coalesce(completed_at, failed_at) < ?`, eventCut); err != nil {
		return PruneResult{}, err
	}
	// Output chunks for closed sessions past the output window.
	if err := del(&result.OutputChunks, `delete from runner_output_chunks where captured_at < ? and run_id in (
		select r.run_id from agent_runs r join agent_sessions s on s.session_id=r.session_id where s.closed_at is not null)`, outputCut); err != nil {
		return PruneResult{}, err
	}
	// Events past event_retention_days for closed sessions and terminal runs.
	// Excludes any event still referenced by a notification delivery or named as
	// another event's causation parent, so neither foreign key can abort the
	// sweep; a parent kept this round ages out once its referrers are gone.
	if err := del(&result.Events, `delete from events
		where occurred_at < ?
			and event_id not in (select event_id from notification_deliveries)
			and event_id not in (select causation_event_id from events where causation_event_id is not null)
			and status_subject_id in (
				select subject.status_subject_id from status_subjects subject
				join agent_runs r on subject.subject_hash = 'Run:' || r.run_id
				join run_terminal_outcomes o on o.run_id = r.run_id
				union
				select subject.status_subject_id from status_subjects subject
				join agent_sessions ses on subject.subject_hash = 'Session:' || ses.session_id
				where ses.closed_at is not null)`, eventCut); err != nil {
		return PruneResult{}, err
	}
	// Historical status snapshots beyond the per-subject retention window.
	// status_snapshot_window is a COUNT (keep the newest N per subject), not a
	// time span, so this is windowed by row_number, not by a captured_at cut. The
	// authoritative latest snapshot is always retained regardless of the window.
	snapshotWindow := retention.StatusSnapshotWindow
	if snapshotWindow < 0 {
		snapshotWindow = 0
	}
	if err := del(&result.StatusSnapshots, `delete from status_snapshots
		where status_snapshot_id not in (select status_snapshot_id from latest_status_snapshots)
			and status_snapshot_id in (
				select status_snapshot_id from (
					select status_snapshot_id,
						row_number() over (partition by status_subject_id order by captured_at desc, status_snapshot_id desc) rn
					from status_snapshots
				) where rn > ?)`, snapshotWindow); err != nil {
		return PruneResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PruneResult{}, err
	}

	if _, err := s.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return PruneResult{}, err
	}
	if _, err := s.DB.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return PruneResult{}, err
	}
	return result, nil
}

func fmtRun(seq int) string {
	return "run_" + strconv.Itoa(seq)
}

func readProjectByKey(ctx context.Context, tx *sql.Tx, key string) (Project, bool, error) {
	var p Project
	err := tx.QueryRowContext(ctx, `select p.project_id, p.project_key, p.display_name, p.root_path, p.default_base_ref, p.default_worktree_mode, p.managed_worktree_root_path, w.workspace_id, w.host_id, h.host_key
		from projects p
		join workspaces w on w.project_id=p.project_id
		join project_root_workspaces r on r.workspace_id=w.workspace_id
		join hosts h on h.host_id=w.host_id
		where p.project_key=? and p.archived_at is null`, key).Scan(&p.ID, &p.Key, &p.DisplayName, &p.RootPath, &p.DefaultBaseRef, &p.DefaultWorktreeMode, &p.ManagedWorktreeRoot, &p.ProjectRootWorkspace, &p.HostID, &p.HostKey)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, false, nil
	}
	return p, err == nil, err
}

func scanHost(row scanner) (Host, error) {
	var host Host
	var raw string
	if err := row.Scan(&host.Key, &host.DisplayName, &raw); err != nil {
		return Host{}, err
	}
	if err := json.Unmarshal([]byte(raw), &host.Access); err != nil {
		return Host{}, err
	}
	switch host.Access.Mode {
	case "Local":
		host.Access.HostAlias = ""
		host.Access.SocketForwarding = false
	case "Ssh":
		if !ValidHostAlias(host.Access.HostAlias) {
			return Host{}, errors.New("InvalidHostAlias")
		}
	case "AttachOnlyMosh":
		if !ValidHostAlias(host.Access.HostAlias) {
			return Host{}, errors.New("InvalidHostAlias")
		}
		host.Access.SocketForwarding = false
	default:
		return Host{}, errors.New("InvalidHostAccess")
	}
	return host, nil
}

func ParseHostAccess(value string) (HostAccess, error) {
	if value == "local" {
		return HostAccess{Mode: "Local"}, nil
	}
	if alias, ok := strings.CutPrefix(value, "ssh:"); ok {
		if !ValidHostAlias(alias) {
			return HostAccess{}, errors.New("InvalidHostAlias")
		}
		return HostAccess{Mode: "Ssh", HostAlias: alias, SocketForwarding: true}, nil
	}
	if alias, ok := strings.CutPrefix(value, "mosh:"); ok {
		if !ValidHostAlias(alias) {
			return HostAccess{}, errors.New("InvalidHostAlias")
		}
		return HostAccess{Mode: "AttachOnlyMosh", HostAlias: alias}, nil
	}
	return HostAccess{}, errors.New("InvalidHostAccess")
}

func ParseWorktreeMode(value string) (string, error) {
	switch value {
	case "prompt", "Prompt":
		return "Prompt", nil
	case "always", "Always":
		return "Always", nil
	case "never", "Never":
		return "Never", nil
	default:
		return "", errors.New("InvalidWorktreeMode")
	}
}

func WorktreeModeConfigValue(value string) string {
	switch value {
	case "Prompt":
		return "prompt"
	case "Always":
		return "always"
	case "Never":
		return "never"
	default:
		return value
	}
}

func ValidHostKey(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func ValidHostAlias(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == ':' || r == '@' {
			continue
		}
		return false
	}
	return true
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func keyFromName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func runStatusFromOutcome(outcome string) string {
	switch outcome {
	case "ProviderExited":
		return "Exited"
	case "UserStopped":
		return "Stopped"
	case "UserKilled":
		return "Killed"
	case "RunnerFailed", "StartFailed", "Orphaned":
		return "Failed"
	default:
		return "RepairRequired"
	}
}

func deriveRunStatus(quietThreshold time.Duration, closedAt, outcome, snapshot sql.NullString, live bool, heartbeatAt, boundAt, outputAt sql.NullString) string {
	if closedAt.Valid {
		return "Closed"
	}
	if outcome.Valid {
		return runStatusFromOutcome(outcome.String)
	}
	if snapshot.Valid && runStatusFromSnapshot(snapshot.String) == "RepairRequired" {
		return "RepairRequired"
	}
	// LostRunner is decided by the supervisor on the monotonic clock, not here;
	// see supervisor.listSessions. The store treats a live binding as
	// Live/Quiet and lets the supervisor override to LostRunner when the
	// monotonic heartbeat TTL has elapsed.
	if snapshot.Valid {
		return runStatusFromSnapshot(snapshot.String)
	}
	if live {
		if outputAt.Valid {
			if staleTimestamp(outputAt, quietThreshold) {
				return "Quiet"
			}
		} else if staleTimestamp(boundAt, quietThreshold) {
			return "Quiet"
		}
		return "Live"
	}
	return "Starting"
}

func staleTimestamp(value sql.NullString, ttl time.Duration) bool {
	if !value.Valid {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return true
	}
	return time.Since(t) >= ttl
}

func eventTypeForOutcome(outcome string) (string, error) {
	switch outcome {
	case "ProviderExited":
		return string(eventlog.ProviderProcessExited), nil
	case "UserStopped":
		return string(eventlog.RunStopped), nil
	case "UserKilled":
		return string(eventlog.RunKilled), nil
	case "RunnerFailed", "StartFailed", "Orphaned":
		return string(eventlog.RunFailed), nil
	default:
		return "", errors.New("unknown terminal outcome: " + outcome)
	}
}

func runStatusFromSnapshot(snapshot string) string {
	var row struct {
		RunStatus string `json:"runStatus"`
	}
	if err := json.Unmarshal([]byte(snapshot), &row); err != nil {
		return "RepairRequired"
	}
	if validRunStatus(row.RunStatus) {
		return row.RunStatus
	}
	return "RepairRequired"
}

func validRunStatus(status string) bool {
	switch status {
	case "Starting", "Live", "NeedsInput", "NeedsApproval", "Quiet", "Exited", "Stopped", "Killed", "Failed", "LostTmuxTarget", "LostRunner", "Closed", "RepairRequired":
		return true
	default:
		return false
	}
}

func closeSummary(status string) string {
	if activeRunStatus(status) {
		return "StopRequired"
	}
	return "Closable"
}

func closeBlockers(status string) []string {
	if activeRunStatus(status) {
		return []string{"SessionStillRunning"}
	}
	return []string{}
}

func activeRunStatus(status string) bool {
	switch status {
	case "Starting", "Live", "NeedsInput", "NeedsApproval", "Quiet":
		return true
	default:
		return false
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func decodeJSONObject(raw string) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func publicEventSubject(subjectHash, sessionID, sessionHandle string, runHandles map[string]string) map[string]any {
	if subjectHash == "Session:"+sessionID {
		return map[string]any{"kind": "Session", "session": sessionHandle}
	}
	if runID, ok := strings.CutPrefix(subjectHash, "Run:"); ok {
		return map[string]any{"kind": "Run", "session": sessionHandle, "run": runHandles[runID]}
	}
	// ListSessionEvents only ever joins Session and Run subjects, so reaching
	// here means a non-session/run subject leaked into a session's event stream:
	// a broken invariant, not a value to paper over with a non-canonical kind.
	panic("publicEventSubject: unexpected subject hash " + subjectHash)
}

func insertRunStatusSnapshot(ctx context.Context, tx *sql.Tx, runID, source, snapshotJSON string) error {
	if source != "Event" && source != "Reconciliation" {
		return errors.New("InvalidStatusSnapshotSource")
	}
	var snapshot struct {
		RunStatus string `json:"runStatus"`
	}
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		return err
	}
	if !validRunStatus(snapshot.RunStatus) {
		return errors.New("InvalidRunStatus")
	}
	subjectID, err := ensureRunSubject(ctx, tx, runID)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(snapshotJSON))
	snapshotHash := hex.EncodeToString(sum[:])
	var latestHash string
	err = tx.QueryRowContext(ctx, `select s.snapshot_hash
		from latest_status_snapshots latest
		join status_snapshots s on s.status_snapshot_id=latest.status_snapshot_id
		where latest.status_subject_id=?`, subjectID).Scan(&latestHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if latestHash == snapshotHash {
		return nil
	}
	snapshotID := NewID()
	now := Now()
	if _, err := tx.ExecContext(ctx, `insert into status_snapshots(status_snapshot_id, status_subject_id, captured_at, source, snapshot_hash, snapshot_json)
		values(?, ?, ?, ?, ?, ?)`, snapshotID, subjectID, now, source, snapshotHash, snapshotJSON); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from latest_status_snapshots where status_subject_id=?`, subjectID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `insert into latest_status_snapshots(status_subject_id, status_snapshot_id, updated_at)
		values(?, ?, ?)`, subjectID, snapshotID, now)
	return err
}

func ensureWorkspaceRunnable(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	var closedAt, managedID, activeID, closingID, removedID sql.NullString
	err := tx.QueryRowContext(ctx, `select w.closed_at, mw.managed_worktree_id, aw.active_worktree_id, cw.closing_worktree_id, rw.removed_worktree_id
		from workspaces w
		left join managed_worktrees mw on mw.workspace_id=w.workspace_id
		left join active_worktrees aw on aw.managed_worktree_id=mw.managed_worktree_id
		left join closing_worktrees cw on cw.managed_worktree_id=mw.managed_worktree_id
		left join removed_worktrees rw on rw.managed_worktree_id=mw.managed_worktree_id
		where w.workspace_id=?`, workspaceID).Scan(&closedAt, &managedID, &activeID, &closingID, &removedID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("WorkspaceNotFound")
	}
	if err != nil {
		return err
	}
	if closedAt.Valid {
		return errors.New("WorkspaceClosed")
	}
	if !managedID.Valid {
		return nil
	}
	if closingID.Valid {
		return errors.New("WorkspaceClosing")
	}
	if removedID.Valid {
		return errors.New("WorkspaceRemoved")
	}
	if !activeID.Valid {
		return errors.New("ManagedWorktreeNotActive")
	}
	return nil
}

func latestRunStatusSnapshot(ctx context.Context, tx *sql.Tx, runID string) (string, error) {
	subjectID, ok, err := readRunSubject(ctx, tx, runID)
	if err != nil || !ok {
		return "", err
	}
	var raw string
	err = tx.QueryRowContext(ctx, `select s.snapshot_json
		from latest_status_snapshots latest
		join status_snapshots s on s.status_snapshot_id=latest.status_snapshot_id
		where latest.status_subject_id=?`, subjectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return runStatusFromSnapshot(raw), nil
}

func deleteRunStatusSnapshot(ctx context.Context, tx *sql.Tx, runID string) error {
	subjectID, ok, err := readRunSubject(ctx, tx, runID)
	if err != nil || !ok {
		return err
	}
	_, err = tx.ExecContext(ctx, `delete from latest_status_snapshots where status_subject_id=?`, subjectID)
	return err
}

func insertRunEvent(ctx context.Context, tx *sql.Tx, runID, eventType, payloadJSON string) error {
	subjectID, err := ensureRunSubject(ctx, tx, runID)
	if err != nil {
		return err
	}
	return insertEvent(ctx, tx, subjectID, eventType, `{"kind":"Supervisor"}`, payloadJSON, runID, "")
}

func ensureRunSubject(ctx context.Context, tx *sql.Tx, runID string) (string, error) {
	if subjectID, ok, err := readRunSubject(ctx, tx, runID); err != nil || ok {
		return subjectID, err
	}
	subjectID := NewID()
	_, err := tx.ExecContext(ctx, `insert into status_subjects(status_subject_id, subject_json, subject_hash, created_at)
		values(?, ?, ?, ?)`, subjectID, `{"kind":"Run","runId":"`+runID+`"}`, "Run:"+runID, Now())
	return subjectID, err
}

func readRunSubject(ctx context.Context, tx *sql.Tx, runID string) (string, bool, error) {
	var subjectID string
	err := tx.QueryRowContext(ctx, `select status_subject_id from status_subjects where subject_hash=?`, "Run:"+runID).Scan(&subjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return subjectID, err == nil, err
}

func managedWorktreeSelect() string {
	return `select mw.managed_worktree_id, w.workspace_id, w.workspace_key, w.path, mw.branch_name, mw.base_ref, mw.base_sha,
		mw.marker_file_path, mw.marker_file_hash, p.project_id, p.project_key, p.root_path, p.managed_worktree_root_path, r.repository_id, r.git_common_dir_path
		from managed_worktrees mw
		join active_worktrees aw on aw.managed_worktree_id=mw.managed_worktree_id
		join workspaces w on w.workspace_id=mw.workspace_id
		join projects p on p.project_id=w.project_id
		join repositories r on r.repository_id=w.repository_id`
}

type scanner interface {
	Scan(dest ...any) error
}

func scanManagedWorktree(row scanner) (ManagedWorktree, error) {
	var wt ManagedWorktree
	err := row.Scan(&wt.ManagedWorktreeID, &wt.WorkspaceID, &wt.WorkspaceKey, &wt.Path, &wt.Branch, &wt.BaseRef, &wt.BaseSHA, &wt.MarkerFilePath, &wt.MarkerFileHash, &wt.ProjectID, &wt.ProjectKey, &wt.ProjectRoot, &wt.ManagedRoot, &wt.RepositoryID, &wt.GitCommonDir)
	return wt, err
}

func worktreeTargetJSON(wt ManagedWorktree) string {
	return `{"kind":"Workspace","workspace":"` + worktreeHandle(wt) + `","path":` + quoteJSON(wt.Path) + `}`
}

func worktreeHandle(wt ManagedWorktree) string {
	return Handle("wks_", wt.WorkspaceID)
}

func sessionTargetJSON(detail SessionDetail) string {
	return `{"kind":"Session","session":"` + detail.Summary.Session + `"}`
}

func closeSummaryForBlocker(blocker string) string {
	switch blocker {
	case "SessionStillRunning":
		return "StopRequired"
	case "WorkspaceShared":
		return "SharedWorkspaceBlocked"
	case "WorktreeDirty":
		return "DirtyWorktreeBlocked"
	case "WorktreeHasUntrackedFiles":
		return "UntrackedFilesBlocked"
	case "WorktreeHasIgnoredUserFiles":
		return "IgnoredUserFilesBlocked"
	case "WorktreeHasConflicts":
		return "ConflictsBlocked"
	case "BranchHasUnpushedCommits", "NoRemoteTrackingProof":
		return "UnpushedCommitsBlocked"
	default:
		return "RepairRequired"
	}
}

func stringListJSON(values []string) string {
	raw, _ := json.Marshal(values)
	return string(raw)
}

func insertWorkspaceEvent(ctx context.Context, tx *sql.Tx, workspaceID, eventType, payloadJSON string) error {
	subjectID, err := ensureWorkspaceSubject(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	return insertEvent(ctx, tx, subjectID, eventType, `{"kind":"User"}`, payloadJSON, workspaceID, "")
}

func insertSessionEvent(ctx context.Context, tx *sql.Tx, sessionID, eventType, payloadJSON string) error {
	subjectID, err := ensureSessionSubject(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	return insertEvent(ctx, tx, subjectID, eventType, `{"kind":"User"}`, payloadJSON, sessionID, "")
}

// insertEvent appends an event. correlationID ties an entity's events together
// (its run/session/workspace/host id) so a request can be traced across lines in
// both the event log and the operational log; causationEventID (optional) links
// an effect event to the event that caused it.
func insertEvent(ctx context.Context, tx *sql.Tx, subjectID, eventType, actorJSON, payloadJSON, correlationID, causationEventID string) error {
	if !eventlog.Valid(eventType) {
		return errors.New("unknown event type: " + eventType)
	}
	if !json.Valid([]byte(actorJSON)) || !json.Valid([]byte(payloadJSON)) {
		return errors.New("invalid event JSON")
	}
	if correlationID == "" {
		correlationID = subjectID
	}
	eventID := NewID()
	now := Now()
	var eventSeq int
	if err := tx.QueryRowContext(ctx, `select coalesce(max(event_seq), 0)+1 from events where status_subject_id=?`, subjectID).Scan(&eventSeq); err != nil {
		return err
	}
	var causation any
	if causationEventID != "" {
		causation = causationEventID
	}
	// Notification delivery is intentionally NOT performed here. It is decoupled
	// from the state-mutating transaction and driven asynchronously by the
	// notification worker (see PendingNotifications), so a delivery failure can
	// never roll back canonical run state.
	_, err := tx.ExecContext(ctx, `insert into events(event_id, status_subject_id, event_seq, occurred_at, event_type, actor_json, payload_json, correlation_json, causation_event_id)
		values(?, ?, ?, ?, ?, ?, ?, ?, ?)`, eventID, subjectID, eventSeq, now, eventType, actorJSON, payloadJSON, `{"correlationId":`+quoteJSON(correlationID)+`}`, causation)
	return err
}

// latestRunEventID returns the most recent event id of a type on a run subject,
// used to link an effect event to its cause (causation).
func latestRunEventID(ctx context.Context, tx *sql.Tx, runID, eventType string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `select e.event_id from events e
		join status_subjects s on s.status_subject_id=e.status_subject_id
		where s.subject_hash='Run:'||? and e.event_type=?
		order by e.event_seq desc limit 1`, runID, eventType).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// insertRunEventCaused appends a run event linked to the event that caused it.
func insertRunEventCaused(ctx context.Context, tx *sql.Tx, runID, eventType, payloadJSON, causationEventID string) error {
	subjectID, err := ensureRunSubject(ctx, tx, runID)
	if err != nil {
		return err
	}
	return insertEvent(ctx, tx, subjectID, eventType, `{"kind":"Supervisor"}`, payloadJSON, runID, causationEventID)
}

func ensureSessionSubject(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	subjectID, ok, err := readSessionSubject(ctx, tx, sessionID)
	if err != nil || ok {
		return subjectID, err
	}
	subjectID = NewID()
	_, err = tx.ExecContext(ctx, `insert into status_subjects(status_subject_id, subject_json, subject_hash, created_at)
		values(?, ?, ?, ?)`, subjectID, `{"kind":"Session","sessionId":"`+sessionID+`"}`, "Session:"+sessionID, Now())
	return subjectID, err
}

func readSessionSubject(ctx context.Context, tx *sql.Tx, sessionID string) (string, bool, error) {
	var subjectID string
	err := tx.QueryRowContext(ctx, `select status_subject_id from status_subjects where subject_hash=?`, "Session:"+sessionID).Scan(&subjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return subjectID, err == nil, err
}

func ensureWorkspaceSubject(ctx context.Context, tx *sql.Tx, workspaceID string) (string, error) {
	subjectID, ok, err := readWorkspaceSubject(ctx, tx, workspaceID)
	if err != nil || ok {
		return subjectID, err
	}
	subjectID = NewID()
	_, err = tx.ExecContext(ctx, `insert into status_subjects(status_subject_id, subject_json, subject_hash, created_at)
		values(?, ?, ?, ?)`, subjectID, `{"kind":"Workspace","workspaceId":"`+workspaceID+`"}`, "Workspace:"+workspaceID, Now())
	return subjectID, err
}

func readWorkspaceSubject(ctx context.Context, tx *sql.Tx, workspaceID string) (string, bool, error) {
	var subjectID string
	err := tx.QueryRowContext(ctx, `select status_subject_id from status_subjects where subject_hash=?`, "Workspace:"+workspaceID).Scan(&subjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return subjectID, err == nil, err
}

// ensureTmuxServerSubject returns the Host status subject for the host that owns
// the named tmux server. A tmux-server-restart event is a fact about that host;
// StatusSubjectKind has no TmuxServer member (canonical kinds are Session, Run,
// Workspace, Host), so the event attaches to the Host subject.
func ensureTmuxServerSubject(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var hostID string
	if err := tx.QueryRowContext(ctx, `select host_id from tmux_servers where tmux_server_key=? and retired_at is null`, key).Scan(&hostID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("TmuxServerNotFound")
		}
		return "", err
	}
	return ensureHostSubject(ctx, tx, hostID)
}

func ensureHostSubject(ctx context.Context, tx *sql.Tx, hostID string) (string, error) {
	var subjectID string
	err := tx.QueryRowContext(ctx, `select status_subject_id from status_subjects where subject_hash=?`, "Host:"+hostID).Scan(&subjectID)
	if err == nil {
		return subjectID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	subjectID = NewID()
	_, err = tx.ExecContext(ctx, `insert into status_subjects(status_subject_id, subject_json, subject_hash, created_at)
		values(?, ?, ?, ?)`, subjectID, `{"kind":"Host","hostId":"`+hostID+`"}`, "Host:"+hostID, Now())
	return subjectID, err
}
