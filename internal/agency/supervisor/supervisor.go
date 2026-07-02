package supervisor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"agency-two/internal/agency/gitx"
	"agency-two/internal/agency/provider"
	"agency-two/internal/agency/runnerproto"
	"agency-two/internal/agency/safety"
	"agency-two/internal/agency/storage"
	tmuxadapter "agency-two/internal/agency/tmux"
)

const Version = "dev"

const (
	reconcileInterval = 30 * time.Second
	runnerDialTimeout = 300 * time.Millisecond
	runnerOrphanTTL   = 30 * time.Second
	outputRetryDelay  = 250 * time.Millisecond
)

type Config struct {
	StateDB    string
	SocketPath string
	LockPath   string
}

type Health struct {
	Status                string `json:"status"`
	Version               string `json:"version"`
	RunnerProtocolVersion int    `json:"runnerProtocolVersion"`
	DBPath                string `json:"dbPath"`
	SocketPath            string `json:"socketPath"`
	UptimeMS              int64  `json:"uptimeMs"`
	Sessions              int    `json:"sessions"`
	AdoptedRunners        int    `json:"adoptedRunners"`
	QuarantinedRunners    int    `json:"quarantinedRunners"`
	OrphanedRunners       int    `json:"orphanedRunners"`
	LastReconcileAt       string `json:"lastReconcileAt"`
}

type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type SessionOutputParams struct {
	Session      string `json:"session"`
	FromChunkSeq int64  `json:"fromChunkSeq"`
}

type SessionEventsParams struct {
	Session string `json:"session"`
}

type SessionStatusParams struct {
	Session string `json:"session"`
}

type SessionStatusResult struct {
	storage.SessionSummary
	WorkspaceKey string                `json:"workspaceKey"`
	Path         string                `json:"path"`
	Tmux         map[string]any        `json:"tmux"`
	Launch       map[string]any        `json:"launch"`
	RecentOutput []storage.OutputChunk `json:"recentOutput"`
	Events       []storage.Event       `json:"events"`
	Diff         map[string]any        `json:"diff"`
}

type RenameSessionParams struct {
	Session   string `json:"session"`
	Title     string `json:"title"`
	ReplayKey string `json:"replayKey"`
}

type CloseSessionParams struct {
	Session   string `json:"session"`
	ReplayKey string `json:"replayKey"`
}

type CloseSessionResult struct {
	Session      string `json:"session"`
	Workspace    string `json:"workspace"`
	WorkspaceKey string `json:"workspaceKey"`
}

type StopRunParams struct {
	Session   string `json:"session"`
	ReplayKey string `json:"replayKey"`
}

type StopRunResult struct {
	Session string `json:"session"`
	Run     string `json:"run"`
}

type KillRunParams struct {
	Session string `json:"session"`
}

type KillRunResult struct {
	Session string `json:"session"`
	Run     string `json:"run"`
}

type SendInputParams struct {
	Session   string `json:"session"`
	Input     []byte `json:"input"`
	ReplayKey string `json:"replayKey"`
}

type SendInputResult struct {
	RunID    string `json:"-"`
	Run      string `json:"run"`
	InputSeq int    `json:"inputSeq"`
}

type CloseWorktreeParams struct {
	Workspace string `json:"workspace"`
	ReplayKey string `json:"replayKey"`
}

type CloseWorktreeResult struct {
	CloseAttempt string               `json:"closeAttempt"`
	Target       map[string]any       `json:"target"`
	Closed       bool                 `json:"closed"`
	Status       string               `json:"status"`
	Workspace    string               `json:"workspace"`
	WorkspaceKey string               `json:"workspaceKey"`
	Path         string               `json:"path"`
	Branch       string               `json:"branch"`
	Blockers     []CloseWorktreeBlock `json:"blockers,omitempty"`
}

type CloseWorktreeBlock struct {
	Blocker  string         `json:"blocker"`
	Summary  string         `json:"summary"`
	Evidence map[string]any `json:"evidence"`
}

type StartRunParams struct {
	Session   string `json:"session"`
	ReplayKey string `json:"replayKey"`
}

type StartRunResult struct {
	Session string `json:"session"`
	Run     string `json:"run"`
}

type StartSessionParams struct {
	Provider       string   `json:"provider"`
	Profile        string   `json:"profile"`
	Title          string   `json:"title"`
	Prompt         string   `json:"prompt"`
	CWD            string   `json:"cwd"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	AddDirs        []string `json:"addDirs"`
	Env            []string `json:"env"`
	PermissionMode string   `json:"permissionMode"`
	SandboxMode    string   `json:"sandboxMode"`
	ApprovalPolicy string   `json:"approvalPolicy"`
	AllowDangerous bool     `json:"allowDangerous"`
	Worktree       bool     `json:"worktree"`
	NoWorktree     bool     `json:"noWorktree"`
	WorktreeName   string   `json:"worktreeName"`
	BaseRef        string   `json:"baseRef"`
	ReplayKey      string   `json:"replayKey"`
}

type StartSessionResult struct {
	Session      string   `json:"session"`
	Run          string   `json:"run"`
	Provider     string   `json:"provider"`
	Title        string   `json:"title"`
	Workspace    string   `json:"workspace"`
	WorkspaceKey string   `json:"workspaceKey"`
	Path         string   `json:"path"`
	Tmux         string   `json:"tmux"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Argv         []string `json:"argv"`
}

type DiffParams struct {
	Target string `json:"target"`
}

type DiffResult struct {
	Base  string `json:"base"`
	Stat  string `json:"stat"`
	Patch string `json:"patch"`
}

type ListWorktreesParams struct {
	CWD string `json:"cwd"`
}

type WorktreeSummary struct {
	Workspace       string         `json:"workspace"`
	WorkspaceKey    string         `json:"workspaceKey"`
	Project         string         `json:"project"`
	Path            string         `json:"path"`
	ManagedWorktree bool           `json:"managedWorktree"`
	Branch          string         `json:"branch"`
	BaseRef         string         `json:"baseRef"`
	BaseSHA         string         `json:"baseSha"`
	Git             map[string]any `json:"git"`
	Close           map[string]any `json:"close"`
	Sessions        []string       `json:"sessions"`
	Marker          map[string]any `json:"marker"`
	Diff            map[string]any `json:"diff"`
}

type WorktreeStatusParams struct {
	Workspace string `json:"workspace"`
}

type WorktreeStatusResult = WorktreeSummary

type DoctorReport struct {
	StateDB string        `json:"stateDb"`
	Issues  []DoctorIssue `json:"issues"`
}

type DoctorIssue struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	Target   map[string]any `json:"target"`
	Summary  string         `json:"summary"`
	Detail   string         `json:"detail"`
	Repair   string         `json:"repair,omitempty"`
}

type AttachParams struct {
	Session string `json:"session"`
}

type AttachResult struct {
	Argv []string `json:"argv"`
}

type ProjectParams struct {
	CWD       string `json:"cwd"`
	ReplayKey string `json:"replayKey"`
}

type ProjectResult struct {
	Project              string `json:"project"`
	DisplayName          string `json:"displayName"`
	RootPath             string `json:"rootPath"`
	DefaultBaseRef       string `json:"defaultBaseRef"`
	DefaultHost          string `json:"defaultHost"`
	DefaultWorktreeMode  string `json:"defaultWorktreeMode"`
	ManagedWorktreeRoot  string `json:"managedWorktreeRoot"`
	ProjectRootWorkspace string `json:"projectRootWorkspace"`
}

type ListModelsParams struct {
	Provider string `json:"provider"`
}

type ProfileSaveParams struct {
	Profile        string `json:"profile"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	PermissionMode string `json:"permissionMode"`
	SandboxMode    string `json:"sandboxMode"`
	ApprovalPolicy string `json:"approvalPolicy"`
}

type ProfileSaveResult struct {
	Profile  string `json:"profile"`
	Provider string `json:"provider"`
}

type ProfileDefaultParams struct {
	CWD     string `json:"cwd"`
	Profile string `json:"profile"`
}

type ProfileDefaultResult struct {
	Project string `json:"project"`
	Profile string `json:"profile"`
}

type ConfigSetParams struct {
	CWD       string `json:"cwd"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	ReplayKey string `json:"replayKey"`
}

type ConfigGetParams struct {
	CWD string `json:"cwd"`
	Key string `json:"key"`
}

type ConfigSetResult struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type HostListResult struct {
	Hosts []storage.Host `json:"hosts"`
}

type ExportParams struct {
	Path string `json:"path"`
}

type ExportResult struct {
	Path string `json:"path"`
}

type RepairParams struct {
	Key       string `json:"key"`
	ReplayKey string `json:"replayKey"`
}

type RepairResult struct {
	Key     string `json:"key"`
	Session string `json:"session"`
	Status  string `json:"status"`
}

type CloseTerminalSessionsResult struct {
	Count int `json:"count"`
}

type CloseTerminalSessionsParams struct {
	ReplayKey string `json:"replayKey"`
}

type Server struct {
	cfg     Config
	started time.Time
	store   *storage.Store
	lock    *os.File

	mu                  sync.Mutex
	adoptedRunners      int
	quarantinedRunners  int
	orphanedRunners     int
	lastReconcileAt     string
	outputSubscriptions map[string]context.CancelFunc
}

func Serve(ctx context.Context, cfg Config) error {
	server, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer server.close()
	if err := server.reconcile(ctx); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0700); err != nil {
		return err
	}
	if err := removeStaleSocket(cfg.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(cfg.SocketPath)
	if err := os.Chmod(cfg.SocketPath, 0600); err != nil {
		return err
	}

	errs := make(chan error, 1)
	go server.reconcileLoop(ctx)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				errs <- err
				return
			}
			go server.handle(conn)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errs:
		select {
		case <-ctx.Done():
			return nil
		default:
			return err
		}
	}
}

func HealthCheck(ctx context.Context, socketPath string) (Health, error) {
	var health Health
	if err := Call(ctx, socketPath, "health", nil, &health); err != nil {
		return Health{}, err
	}
	return health, nil
}

func ListSessions(ctx context.Context, socketPath string) ([]storage.SessionSummary, error) {
	var sessions []storage.SessionSummary
	if err := Call(ctx, socketPath, "listSessions", nil, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func SessionOutput(ctx context.Context, socketPath, session string, fromChunkSeq int64) ([]storage.OutputChunk, error) {
	var chunks []storage.OutputChunk
	if err := Call(ctx, socketPath, "sessionOutput", SessionOutputParams{Session: session, FromChunkSeq: fromChunkSeq}, &chunks); err != nil {
		return nil, err
	}
	return chunks, nil
}

func SessionEvents(ctx context.Context, socketPath, session string) ([]storage.Event, error) {
	var events []storage.Event
	if err := Call(ctx, socketPath, "sessionEvents", SessionEventsParams{Session: session}, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func SessionStatus(ctx context.Context, socketPath, session string) (SessionStatusResult, error) {
	var result SessionStatusResult
	if err := Call(ctx, socketPath, "sessionStatus", SessionStatusParams{Session: session}, &result); err != nil {
		return SessionStatusResult{}, err
	}
	return result, nil
}

func RenameSession(ctx context.Context, socketPath, session, title, replayKey string) error {
	return Call(ctx, socketPath, "renameSession", RenameSessionParams{Session: session, Title: title, ReplayKey: replayKey}, nil)
}

func CloseSession(ctx context.Context, socketPath, session, replayKey string) (CloseSessionResult, error) {
	var result CloseSessionResult
	if err := Call(ctx, socketPath, "closeSession", CloseSessionParams{Session: session, ReplayKey: replayKey}, &result); err != nil {
		return CloseSessionResult{}, err
	}
	return result, nil
}

func CloseTerminalSessions(ctx context.Context, socketPath, replayKey string) (int, error) {
	var result CloseTerminalSessionsResult
	if err := Call(ctx, socketPath, "closeTerminalSessions", CloseTerminalSessionsParams{ReplayKey: replayKey}, &result); err != nil {
		return 0, err
	}
	return result.Count, nil
}

func StopRun(ctx context.Context, socketPath, session, replayKey string) (StopRunResult, error) {
	var result StopRunResult
	if err := Call(ctx, socketPath, "stopRun", StopRunParams{Session: session, ReplayKey: replayKey}, &result); err != nil {
		return StopRunResult{}, err
	}
	return result, nil
}

func KillRun(ctx context.Context, socketPath, session string) (KillRunResult, error) {
	var result KillRunResult
	if err := Call(ctx, socketPath, "killRun", KillRunParams{Session: session}, &result); err != nil {
		return KillRunResult{}, err
	}
	return result, nil
}

func SendInput(ctx context.Context, socketPath, session string, input []byte, replayKey string) (SendInputResult, error) {
	var result SendInputResult
	if err := Call(ctx, socketPath, "sendInput", SendInputParams{Session: session, Input: input, ReplayKey: replayKey}, &result); err != nil {
		return SendInputResult{}, err
	}
	return result, nil
}

func CloseWorktree(ctx context.Context, socketPath, workspace, replayKey string) (CloseWorktreeResult, error) {
	var result CloseWorktreeResult
	if err := Call(ctx, socketPath, "closeWorktree", CloseWorktreeParams{Workspace: workspace, ReplayKey: replayKey}, &result); err != nil {
		return CloseWorktreeResult{}, err
	}
	return result, nil
}

func StartRun(ctx context.Context, socketPath, session, replayKey string) (StartRunResult, error) {
	var result StartRunResult
	if err := Call(ctx, socketPath, "startRun", StartRunParams{Session: session, ReplayKey: replayKey}, &result); err != nil {
		return StartRunResult{}, err
	}
	return result, nil
}

func StartSession(ctx context.Context, socketPath string, params StartSessionParams) (StartSessionResult, error) {
	var result StartSessionResult
	if err := Call(ctx, socketPath, "startSession", params, &result); err != nil {
		return StartSessionResult{}, err
	}
	return result, nil
}

func SessionDiff(ctx context.Context, socketPath, session string) (DiffResult, error) {
	var result DiffResult
	if err := Call(ctx, socketPath, "sessionDiff", DiffParams{Target: session}, &result); err != nil {
		return DiffResult{}, err
	}
	return result, nil
}

func ListWorktrees(ctx context.Context, socketPath, cwd string) ([]WorktreeSummary, error) {
	var result []WorktreeSummary
	if err := Call(ctx, socketPath, "listWorktrees", ListWorktreesParams{CWD: cwd}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func WorktreeStatus(ctx context.Context, socketPath, workspace string) (WorktreeStatusResult, error) {
	var result WorktreeStatusResult
	if err := Call(ctx, socketPath, "worktreeStatus", WorktreeStatusParams{Workspace: workspace}, &result); err != nil {
		return WorktreeStatusResult{}, err
	}
	return result, nil
}

func WorktreeDiff(ctx context.Context, socketPath, workspace string) (DiffResult, error) {
	var result DiffResult
	if err := Call(ctx, socketPath, "worktreeDiff", DiffParams{Target: workspace}, &result); err != nil {
		return DiffResult{}, err
	}
	return result, nil
}

func AttachCommand(ctx context.Context, socketPath, session string) (AttachResult, error) {
	var result AttachResult
	if err := Call(ctx, socketPath, "attachCommand", AttachParams{Session: session}, &result); err != nil {
		return AttachResult{}, err
	}
	return result, nil
}

func InitProject(ctx context.Context, socketPath, cwd, replayKey string) (ProjectResult, error) {
	var result ProjectResult
	if err := Call(ctx, socketPath, "initProject", ProjectParams{CWD: cwd, ReplayKey: replayKey}, &result); err != nil {
		return ProjectResult{}, err
	}
	return result, nil
}

func ListProjects(ctx context.Context, socketPath string) ([]ProjectResult, error) {
	var result []ProjectResult
	if err := Call(ctx, socketPath, "listProjects", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func ListModels(ctx context.Context, socketPath, provider string) ([]storage.Model, error) {
	var result []storage.Model
	if err := Call(ctx, socketPath, "listModels", ListModelsParams{Provider: provider}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func ListProfiles(ctx context.Context, socketPath string) ([]storage.Profile, error) {
	var result []storage.Profile
	if err := Call(ctx, socketPath, "listProfiles", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func SaveProfile(ctx context.Context, socketPath string, params ProfileSaveParams) (ProfileSaveResult, error) {
	var result ProfileSaveResult
	if err := Call(ctx, socketPath, "saveProfile", params, &result); err != nil {
		return ProfileSaveResult{}, err
	}
	return result, nil
}

func SetDefaultProfile(ctx context.Context, socketPath string, params ProfileDefaultParams) (ProfileDefaultResult, error) {
	var result ProfileDefaultResult
	if err := Call(ctx, socketPath, "setDefaultProfile", params, &result); err != nil {
		return ProfileDefaultResult{}, err
	}
	return result, nil
}

func SetConfig(ctx context.Context, socketPath string, params ConfigSetParams) (ConfigSetResult, error) {
	var result ConfigSetResult
	if err := Call(ctx, socketPath, "setConfig", params, &result); err != nil {
		return ConfigSetResult{}, err
	}
	return result, nil
}

func GetConfig(ctx context.Context, socketPath string, params ConfigGetParams) (ConfigSetResult, error) {
	var result ConfigSetResult
	if err := Call(ctx, socketPath, "getConfig", params, &result); err != nil {
		return ConfigSetResult{}, err
	}
	return result, nil
}

func ListHosts(ctx context.Context, socketPath string) ([]storage.Host, error) {
	var result HostListResult
	if err := Call(ctx, socketPath, "listHosts", nil, &result); err != nil {
		return nil, err
	}
	return result.Hosts, nil
}

func Doctor(ctx context.Context, socketPath string) (DoctorReport, error) {
	var result DoctorReport
	if err := Call(ctx, socketPath, "doctor", nil, &result); err != nil {
		return DoctorReport{}, err
	}
	return result, nil
}

func Prune(ctx context.Context, socketPath string) error {
	return Call(ctx, socketPath, "prune", nil, nil)
}

func Export(ctx context.Context, socketPath, path string) (ExportResult, error) {
	var result ExportResult
	if err := Call(ctx, socketPath, "export", ExportParams{Path: path}, &result); err != nil {
		return ExportResult{}, err
	}
	return result, nil
}

func Repair(ctx context.Context, socketPath, key, replayKey string) (RepairResult, error) {
	var result RepairResult
	if err := Call(ctx, socketPath, "repair", RepairParams{Key: key, ReplayKey: replayKey}, &result); err != nil {
		return RepairResult{}, err
	}
	return result, nil
}

func Call(ctx context.Context, socketPath, method string, params any, result any) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		rawParams = nil
	}
	if err := json.NewEncoder(conn).Encode(request{Method: method, Params: rawParams}); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func open(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.StateDB == "" {
		return nil, errors.New("state db path is required")
	}
	if cfg.SocketPath == "" {
		return nil, errors.New("socket path is required")
	}
	if cfg.LockPath == "" {
		cfg.LockPath = cfg.StateDB + ".lock"
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LockPath), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(cfg.LockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		detail, _ := os.ReadFile(cfg.LockPath)
		lock.Close()
		return nil, fmt.Errorf("SupervisorAlreadyRunning: %s: %w", strings.TrimSpace(string(detail)), err)
	}
	if err := lock.Truncate(0); err != nil {
		lock.Close()
		return nil, err
	}
	if _, err := lock.Seek(0, 0); err != nil {
		lock.Close()
		return nil, err
	}
	if _, err := lock.WriteString(fmt.Sprintf("pid=%d\nstarted=%s\nversion=%s\n", os.Getpid(), storage.Now(), Version)); err != nil {
		lock.Close()
		return nil, err
	}
	store, err := storage.Open(ctx, cfg.StateDB)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Server{cfg: cfg, started: time.Now(), store: store, lock: lock, outputSubscriptions: map[string]context.CancelFunc{}}, nil
}

func (s *Server) close() {
	s.mu.Lock()
	for _, cancel := range s.outputSubscriptions {
		cancel()
	}
	s.mu.Unlock()
	if s.store != nil {
		_ = s.store.Close()
	}
	if s.lock != nil {
		_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		_ = s.lock.Close()
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	if err := verifyPeer(conn); err != nil {
		_ = writeResponse(conn, nil, err)
		return
	}
	var req request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		_ = writeResponse(conn, nil, err)
		return
	}
	switch req.Method {
	case "health":
		sessions, err := s.store.ListSessions(context.Background())
		if err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		s.mu.Lock()
		adopted := s.adoptedRunners
		quarantined := s.quarantinedRunners
		orphaned := s.orphanedRunners
		lastReconcileAt := s.lastReconcileAt
		s.mu.Unlock()
		_ = writeResponse(conn, Health{
			Status: "Live", Version: Version, RunnerProtocolVersion: runnerproto.ProtocolVersion,
			DBPath: s.cfg.StateDB, SocketPath: s.cfg.SocketPath,
			UptimeMS: time.Since(s.started).Milliseconds(), Sessions: len(sessions),
			AdoptedRunners: adopted, QuarantinedRunners: quarantined, OrphanedRunners: orphaned,
			LastReconcileAt: lastReconcileAt,
		}, nil)
	case "listSessions":
		sessions, err := s.listSessions(context.Background())
		_ = writeResponse(conn, sessions, err)
	case "sessionOutput":
		var params SessionOutputParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		if params.FromChunkSeq == 0 {
			params.FromChunkSeq = 1
		}
		chunks, err := s.store.SessionOutput(context.Background(), params.Session, params.FromChunkSeq)
		_ = writeResponse(conn, chunks, err)
	case "sessionEvents":
		var params SessionEventsParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		events, err := s.store.ListSessionEvents(context.Background(), params.Session)
		_ = writeResponse(conn, events, err)
	case "sessionStatus":
		var params SessionStatusParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.sessionStatus(context.Background(), params.Session)
		_ = writeResponse(conn, result, err)
	case "renameSession":
		var params RenameSessionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		_ = writeResponse(conn, nil, s.renameSession(context.Background(), params))
	case "closeSession":
		var params CloseSessionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.closeSession(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "closeTerminalSessions":
		var params CloseTerminalSessionsParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				_ = writeResponse(conn, nil, err)
				return
			}
		}
		count, err := s.closeTerminalSessions(context.Background(), params.ReplayKey)
		_ = writeResponse(conn, CloseTerminalSessionsResult{Count: count}, err)
	case "stopRun":
		var params StopRunParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.stopRun(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "killRun":
		var params KillRunParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.killRun(context.Background(), params.Session)
		_ = writeResponse(conn, result, err)
	case "sendInput":
		var params SendInputParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.sendInput(context.Background(), params.Session, params.Input, params.ReplayKey)
		_ = writeResponse(conn, result, err)
	case "closeWorktree":
		var params CloseWorktreeParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.closeWorktree(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "startRun":
		var params StartRunParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.startRun(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "startSession":
		var params StartSessionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.startSession(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "sessionDiff":
		var params DiffParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.sessionDiff(context.Background(), params.Target)
		_ = writeResponse(conn, result, err)
	case "listWorktrees":
		var params ListWorktreesParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.listWorktrees(context.Background(), params.CWD)
		_ = writeResponse(conn, result, err)
	case "worktreeStatus":
		var params WorktreeStatusParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.worktreeStatus(context.Background(), params.Workspace)
		_ = writeResponse(conn, result, err)
	case "worktreeDiff":
		var params DiffParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.worktreeDiff(context.Background(), params.Target)
		_ = writeResponse(conn, result, err)
	case "attachCommand":
		var params AttachParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.attachCommand(context.Background(), params.Session)
		_ = writeResponse(conn, result, err)
	case "initProject":
		var params ProjectParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.initProject(context.Background(), params.CWD, params.ReplayKey)
		_ = writeResponse(conn, result, err)
	case "listProjects":
		result, err := s.listProjects(context.Background())
		_ = writeResponse(conn, result, err)
	case "listModels":
		var params ListModelsParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		models, err := s.store.ListModels(context.Background(), params.Provider)
		_ = writeResponse(conn, models, err)
	case "listProfiles":
		profiles, err := s.store.ListProfiles(context.Background())
		_ = writeResponse(conn, profiles, err)
	case "saveProfile":
		var params ProfileSaveParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.saveProfile(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "setDefaultProfile":
		var params ProfileDefaultParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.setDefaultProfile(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "setConfig":
		var params ConfigSetParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.setConfig(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "getConfig":
		var params ConfigGetParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.getConfig(context.Background(), params)
		_ = writeResponse(conn, result, err)
	case "listHosts":
		hosts, err := s.store.ListHosts(context.Background())
		_ = writeResponse(conn, HostListResult{Hosts: hosts}, err)
	case "doctor":
		result, err := s.doctor(context.Background())
		_ = writeResponse(conn, result, err)
	case "prune":
		_ = writeResponse(conn, nil, s.store.Prune(context.Background()))
	case "export":
		var params ExportParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		err := s.store.Export(context.Background(), params.Path)
		_ = writeResponse(conn, ExportResult{Path: params.Path}, err)
	case "repair":
		var params RepairParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = writeResponse(conn, nil, err)
			return
		}
		result, err := s.repair(context.Background(), params)
		_ = writeResponse(conn, result, err)
	default:
		_ = writeResponse(conn, nil, fmt.Errorf("unknown supervisor method %q", req.Method))
	}
}

func (s *Server) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.reconcile(ctx)
		}
	}
}

func (s *Server) reconcile(ctx context.Context) error {
	tmuxOrphaned, err := s.reconcileTmuxServer(ctx)
	if err != nil {
		return err
	}
	bindings, err := s.store.ListActiveRunnerBindings(ctx)
	if err != nil {
		return err
	}
	adopted := 0
	quarantined := 0
	orphaned := tmuxOrphaned
	for _, binding := range bindings {
		preamble, err := readRunnerPreamble(ctx, binding.EndpointPath)
		if err != nil {
			if time.Since(binding.LastHeartbeat) >= runnerOrphanTTL {
				if markErr := s.store.MarkRunnerOrphaned(ctx, binding.RunID, err.Error()); markErr != nil {
					return markErr
				}
				orphaned++
			}
			continue
		}
		if preamble.RunID != binding.RunID {
			if err := s.store.MarkRunnerQuarantined(ctx, binding.RunID, preamble.RunnerProtocolVersion, preamble.RunnerBinaryVersion, "runner preamble run_id does not match active binding"); err != nil {
				return err
			}
			quarantined++
			continue
		}
		if preamble.RunnerProtocolVersion != runnerproto.ProtocolVersion {
			if err := s.store.MarkRunnerQuarantined(ctx, binding.RunID, preamble.RunnerProtocolVersion, preamble.RunnerBinaryVersion, "runner protocol version is not supported"); err != nil {
				return err
			}
			quarantined++
			continue
		}
		heartbeat, err := readRunnerHeartbeat(ctx, binding.EndpointPath)
		if err != nil {
			if time.Since(binding.LastHeartbeat) >= runnerOrphanTTL {
				if markErr := s.store.MarkRunnerOrphaned(ctx, binding.RunID, err.Error()); markErr != nil {
					return markErr
				}
				orphaned++
			}
			continue
		}
		if heartbeat.RunID != binding.RunID || heartbeat.ProtocolVersion != runnerproto.ProtocolVersion {
			if err := s.store.MarkRunnerQuarantined(ctx, binding.RunID, preamble.RunnerProtocolVersion, preamble.RunnerBinaryVersion, "runner heartbeat does not match active binding"); err != nil {
				return err
			}
			quarantined++
			continue
		}
		if err := s.store.MarkRunnerAdopted(ctx, binding.RunID, preamble.RunnerProtocolVersion, preamble.RunnerBinaryVersion); err != nil {
			return err
		}
		s.startOutputSubscription(ctx, binding.RunID, binding.EndpointPath)
		adopted++
	}
	s.mu.Lock()
	s.adoptedRunners = adopted
	s.quarantinedRunners = quarantined
	s.orphanedRunners = orphaned
	s.lastReconcileAt = storage.Now()
	s.mu.Unlock()
	if err := s.reconcileUnpublishedManagedWorktrees(ctx); err != nil {
		return err
	}
	return s.resumeClosingWorktrees(ctx)
}

func (s *Server) reconcileTmuxServer(ctx context.Context) (int, error) {
	const key = "default"
	identity, err := tmuxClient().ServerIdentity(ctx)
	identityJSON := ""
	if err == nil {
		raw, err := json.Marshal(identity)
		if err != nil {
			return 0, err
		}
		identityJSON = string(raw)
	} else if !errors.Is(err, tmuxadapter.ErrServerMissing) {
		return 0, err
	}
	stored, ok, err := s.store.TmuxServerIdentity(ctx, key)
	if err != nil {
		return 0, err
	}
	if identityJSON == "" {
		if !ok {
			return 0, nil
		}
		return s.store.MarkTmuxServerRestarted(ctx, key, "", "tmux server default is missing")
	}
	if !ok {
		return 0, s.store.RecordTmuxServerIdentity(ctx, key, identityJSON)
	}
	if stored != identityJSON {
		return s.store.MarkTmuxServerRestarted(ctx, key, identityJSON, "tmux server default identity changed")
	}
	return 0, nil
}

func (s *Server) resumeClosingWorktrees(ctx context.Context) error {
	worktrees, err := s.store.ListClosingWorktrees(ctx)
	if err != nil {
		return err
	}
	for _, closing := range worktrees {
		if err := s.finishClosingWorktree(ctx, closing); err != nil {
			continue
		}
	}
	return nil
}

func (s *Server) finishClosingWorktree(ctx context.Context, closing storage.ClosingWorktree) error {
	wt := closing.ManagedWorktree
	if err := os.Remove(wt.MarkerFilePath); err != nil && !os.IsNotExist(err) {
		_ = s.store.FailWorktreeRemoval(ctx, closing.AttemptID, err.Error())
		return err
	}
	if _, err := os.Stat(wt.Path); err == nil {
		if err := gitx.RemoveWorktree(ctx, wt.ProjectRoot, wt.Path); err != nil {
			restoreWorktreeMarker(wt)
			_ = s.store.FailWorktreeRemoval(ctx, closing.AttemptID, err.Error())
			return err
		}
	} else if !os.IsNotExist(err) {
		restoreWorktreeMarker(wt)
		_ = s.store.FailWorktreeRemoval(ctx, closing.AttemptID, err.Error())
		return err
	}
	if _, err := os.Stat(wt.Path); err == nil {
		detail := "worktree path still exists after git worktree remove"
		restoreWorktreeMarker(wt)
		_ = s.store.FailWorktreeRemoval(ctx, closing.AttemptID, detail)
		return errors.New(detail)
	} else if !os.IsNotExist(err) {
		restoreWorktreeMarker(wt)
		_ = s.store.FailWorktreeRemoval(ctx, closing.AttemptID, err.Error())
		return err
	}
	return s.store.CompleteWorktreeRemoval(ctx, wt.ManagedWorktreeID, closing.AttemptID)
}

func (s *Server) reconcileUnpublishedManagedWorktrees(ctx context.Context) error {
	worktrees, err := s.store.ListUnpublishedManagedWorktrees(ctx)
	if err != nil {
		return err
	}
	for _, wt := range worktrees {
		raw, err := os.ReadFile(wt.MarkerFilePath)
		if err == nil {
			if strings.TrimSpace(string(raw)) == wt.MarkerFileHash {
				if err := s.store.PublishManagedWorkspace(ctx, wt.WorkspaceID); err != nil {
					return err
				}
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Stat(wt.Path); os.IsNotExist(err) {
			if err := s.store.DeleteUnpublishedManagedWorkspace(ctx, wt.WorkspaceID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) startOutputSubscription(ctx context.Context, runID, socketPath string) {
	s.mu.Lock()
	if _, ok := s.outputSubscriptions[runID]; ok {
		s.mu.Unlock()
		return
	}
	subCtx, cancel := context.WithCancel(ctx)
	s.outputSubscriptions[runID] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.outputSubscriptions, runID)
			s.mu.Unlock()
		}()
		for {
			if err := s.subscribeRunnerOutput(subCtx, runID, socketPath); err == nil {
				return
			}
			select {
			case <-subCtx.Done():
				return
			case <-time.After(outputRetryDelay):
			}
		}
	}()
}

func (s *Server) subscribeRunnerOutput(ctx context.Context, runID, socketPath string) error {
	fromSeq, err := s.store.NextOutputChunkSeq(ctx, runID)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{Timeout: runnerDialTimeout}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		return err
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSubscribe(true, fromSeq)); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}
		switch m := msg.(type) {
		case runnerproto.Heartbeat:
			if m.RunID != runID || m.ProtocolVersion != runnerproto.ProtocolVersion {
				return errors.New("runner heartbeat does not match active binding")
			}
			if err := s.store.RecordRunnerHeartbeat(ctx, runID); err != nil {
				return err
			}
		case runnerproto.OutputChunk:
			if err := s.store.AppendOutputChunk(ctx, runID, m.ChunkSeq, m.Stream, m.Bytes); err != nil {
				return err
			}
			if status, detail := promptHintFromOutput(m.Bytes); status != "" {
				if err := s.store.RecordPromptHint(ctx, runID, status, detail); err != nil {
					return err
				}
			}
		case runnerproto.Exit:
			raw, err := json.Marshal(m.Termination)
			if err != nil {
				return err
			}
			return s.store.RecordTerminalOutcome(ctx, runID, m.Termination.Outcome, string(raw))
		}
	}
}

func (s *Server) listSessions(ctx context.Context) ([]storage.SessionSummary, error) {
	sessions, err := s.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		status, err := gitx.Status(ctx, sessions[i].Path)
		if err != nil {
			return nil, err
		}
		if sessions[i].WorkspaceKey != "project_root" {
			status = gitx.StatusWithoutPaths(status, ".agency-worktree")
		}
		sessions[i].Git = map[string]any{
			"summary": string(status.Summary), "presence": string(status.Presence), "tree": string(status.Tree),
			"untracked": status.Untracked, "ignoredUserFiles": status.IgnoredUserFiles, "conflicts": status.Conflicts, "upstream": string(status.Upstream),
		}
		if activeSessionStatus(sessions[i].RunStatus) {
			detail, err := s.store.Session(ctx, sessions[i].Session)
			if err != nil {
				return nil, err
			}
			if detail.RunnerSocket != "" {
				ok, err := tmuxClient().TargetExists(ctx, detail.TmuxSessionName)
				if err != nil {
					return nil, err
				}
				if !ok {
					sessions[i].RunStatus = "LostTmuxTarget"
					sessions[i].Close = map[string]any{"closable": false, "summary": "RepairRequired", "blockers": []string{"TmuxTargetMissing"}}
				}
			}
		}
	}
	return sessions, nil
}

func (s *Server) sessionStatus(ctx context.Context, session string) (SessionStatusResult, error) {
	sessions, err := s.listSessions(ctx)
	if err != nil {
		return SessionStatusResult{}, err
	}
	var summary storage.SessionSummary
	found := false
	for _, candidate := range sessions {
		if candidate.Session == session {
			summary = candidate
			found = true
			break
		}
	}
	if !found {
		return SessionStatusResult{}, errors.New("SessionNotFound")
	}
	detail, err := s.store.Session(ctx, session)
	if err != nil {
		return SessionStatusResult{}, err
	}
	chunks, err := s.store.SessionOutput(ctx, session, 1)
	if err != nil {
		return SessionStatusResult{}, err
	}
	if chunks == nil {
		chunks = []storage.OutputChunk{}
	}
	events, err := s.store.ListSessionEvents(ctx, session)
	if err != nil {
		return SessionStatusResult{}, err
	}
	if events == nil {
		events = []storage.Event{}
	}
	tmuxArgv, tmuxErr := tmuxClient().AttachCommand(detail.TmuxSessionName)
	tmux := map[string]any{"target": detail.TmuxSessionName, "attachArgv": tmuxArgv}
	if tmuxErr != nil {
		tmux = map[string]any{"target": detail.TmuxSessionName, "attachAvailable": false, "error": tmuxErr.Error()}
	}
	diff := map[string]any{"available": false}
	if got, err := s.sessionDiff(ctx, session); err == nil {
		diff = map[string]any{"available": true, "base": got.Base, "stat": got.Stat}
	} else {
		diff["error"] = err.Error()
	}
	launch := map[string]any{"provider": summary.Provider, "model": summary.Model, "effort": summary.Effort, "workingDirectory": summary.Path}
	for i := 0; i < len(detail.Argv); i++ {
		if detail.Argv[i] == "--permission-mode" && i+1 < len(detail.Argv) {
			launch["permissionMode"] = detail.Argv[i+1]
		}
		if detail.Argv[i] == "--sandbox" && i+1 < len(detail.Argv) {
			launch["sandboxMode"] = detail.Argv[i+1]
		}
		if detail.Argv[i] == "--ask-for-approval" && i+1 < len(detail.Argv) {
			launch["approvalPolicy"] = detail.Argv[i+1]
		}
	}
	return SessionStatusResult{
		SessionSummary: summary,
		WorkspaceKey:   summary.WorkspaceKey,
		Path:           summary.Path,
		Tmux:           tmux,
		Launch:         launch,
		RecentOutput:   chunks,
		Events:         events,
		Diff:           diff,
	}, nil
}

func promptHintFromOutput(raw []byte) (string, string) {
	text := strings.ToLower(string(raw))
	for _, phrase := range []string{
		"approval required",
		"requires approval",
		"waiting for approval",
		"approve this action",
		"approve?",
	} {
		if strings.Contains(text, phrase) {
			return "NeedsApproval", phrase
		}
	}
	for _, phrase := range []string{
		"input required",
		"waiting for input",
		"enter your prompt",
		"type your message",
		"press enter to continue",
	} {
		if strings.Contains(text, phrase) {
			return "NeedsInput", phrase
		}
	}
	return "", ""
}

func activeSessionStatus(status string) bool {
	switch status {
	case "Starting", "Live", "NeedsInput", "NeedsApproval", "Quiet":
		return true
	default:
		return false
	}
}

func (s *Server) renameSession(ctx context.Context, params RenameSessionParams) error {
	if params.ReplayKey != "" {
		hash := requestHash(RenameSessionParams{Session: params.Session, Title: params.Title})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "renameSession", hash)
		if err != nil {
			return err
		}
		if replayed {
			return nil
		}
		if err := s.renameSession(ctx, RenameSessionParams{Session: params.Session, Title: params.Title}); err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "renameSession", hash)
			return err
		}
		if raw != "" {
			return errors.New("ReplayResultUnexpected")
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "renameSession", hash, `{}`); err != nil {
			return err
		}
		return nil
	}
	return s.store.RenameSession(ctx, params.Session, params.Title)
}

func (s *Server) closeTerminalSessions(ctx context.Context, replayKey string) (int, error) {
	if replayKey != "" {
		hash := requestHash(CloseTerminalSessionsParams{})
		raw, replayed, err := s.store.BeginIdempotency(ctx, replayKey, "closeTerminalSessions", hash)
		if err != nil {
			return 0, err
		}
		if replayed {
			var result CloseTerminalSessionsResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return 0, err
			}
			return result.Count, nil
		}
		count, err := s.closeTerminalSessions(ctx, "")
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, replayKey, "closeTerminalSessions", hash)
			return 0, err
		}
		body, err := json.Marshal(CloseTerminalSessionsResult{Count: count})
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, replayKey, "closeTerminalSessions", hash)
			return 0, err
		}
		if err := s.store.CompleteIdempotency(ctx, replayKey, "closeTerminalSessions", hash, string(body)); err != nil {
			return 0, err
		}
		return count, nil
	}
	return s.store.CloseTerminalSessions(ctx)
}

func (s *Server) stopRun(ctx context.Context, params StopRunParams) (StopRunResult, error) {
	if params.ReplayKey != "" {
		hash := requestHash(StopRunParams{Session: params.Session})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "stopRun", hash)
		if err != nil {
			return StopRunResult{}, err
		}
		if replayed {
			var result StopRunResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return StopRunResult{}, err
			}
			return result, nil
		}
		result, err := s.stopRun(ctx, StopRunParams{Session: params.Session})
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "stopRun", hash)
			return StopRunResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "stopRun", hash)
			return StopRunResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "stopRun", hash, string(body)); err != nil {
			return StopRunResult{}, err
		}
		return result, nil
	}
	detail, err := s.store.Session(ctx, params.Session)
	if err != nil {
		return StopRunResult{}, err
	}
	if detail.RunnerSocket == "" {
		return StopRunResult{}, errors.New("RunNotLive")
	}
	if err := s.store.RecordStopRequested(ctx, detail.RunID, params.Session); err != nil {
		return StopRunResult{}, err
	}
	termination, err := requestRunnerStop(detail.RunnerSocket)
	if err != nil {
		return StopRunResult{}, err
	}
	raw, err := json.Marshal(termination)
	if err != nil {
		return StopRunResult{}, err
	}
	if err := s.store.RecordTerminalOutcome(ctx, detail.RunID, termination.Outcome, string(raw)); err != nil {
		return StopRunResult{}, err
	}
	return StopRunResult{Session: params.Session, Run: detail.Run}, nil
}

func (s *Server) killRun(ctx context.Context, session string) (KillRunResult, error) {
	detail, err := s.store.Session(ctx, session)
	if err != nil {
		return KillRunResult{}, err
	}
	if detail.RunnerSocket != "" {
		termination, err := requestRunnerKill(detail.RunnerSocket)
		if err != nil {
			return KillRunResult{}, err
		}
		raw, err := json.Marshal(termination)
		if err != nil {
			return KillRunResult{}, err
		}
		if err := s.store.RecordTerminalOutcome(ctx, detail.RunID, termination.Outcome, string(raw)); err != nil {
			return KillRunResult{}, err
		}
		return KillRunResult{Session: session, Run: detail.Run}, nil
	}
	if err := s.store.KillRun(ctx, session); err != nil {
		return KillRunResult{}, err
	}
	return KillRunResult{Session: session, Run: detail.Run}, nil
}

func (s *Server) closeSession(ctx context.Context, params CloseSessionParams) (CloseSessionResult, error) {
	if params.ReplayKey != "" {
		hash := requestHash(CloseSessionParams{Session: params.Session})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "closeSession", hash)
		if err != nil {
			return CloseSessionResult{}, err
		}
		if replayed {
			var result CloseSessionResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return CloseSessionResult{}, err
			}
			return result, nil
		}
		result, err := s.closeSession(ctx, CloseSessionParams{Session: params.Session})
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "closeSession", hash)
			return CloseSessionResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "closeSession", hash)
			return CloseSessionResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "closeSession", hash, string(body)); err != nil {
			return CloseSessionResult{}, err
		}
		return result, nil
	}
	detail, err := s.store.Session(ctx, params.Session)
	if err != nil {
		return CloseSessionResult{}, err
	}
	if err := s.store.CloseSession(ctx, params.Session); err != nil {
		return CloseSessionResult{}, err
	}
	return CloseSessionResult{
		Session:      detail.Summary.Session,
		Workspace:    detail.Summary.Workspace,
		WorkspaceKey: detail.Summary.WorkspaceKey,
	}, nil
}

func (s *Server) sendInput(ctx context.Context, session string, input []byte, replayKey string) (SendInputResult, error) {
	if replayKey != "" {
		hash := requestHash(SendInputParams{Session: session, Input: input})
		raw, replayed, err := s.store.BeginIdempotency(ctx, replayKey, "sendInput", hash)
		if err != nil {
			return SendInputResult{}, err
		}
		if replayed {
			var result SendInputResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return SendInputResult{}, err
			}
			return result, nil
		}
		result, err := s.sendInput(ctx, session, input, "")
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, replayKey, "sendInput", hash)
			return SendInputResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, replayKey, "sendInput", hash)
			return SendInputResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, replayKey, "sendInput", hash, string(body)); err != nil {
			return SendInputResult{}, err
		}
		return result, nil
	}
	detail, err := s.store.Session(ctx, session)
	if err != nil {
		return SendInputResult{}, err
	}
	if detail.RunID == "" || detail.RunnerSocket == "" {
		return SendInputResult{}, errors.New("No live runner socket is recorded")
	}
	seq, err := s.store.AddInputEvent(ctx, detail.RunID, input)
	if err != nil {
		return SendInputResult{}, err
	}
	if err := requestRunnerInput(detail.RunnerSocket, int64(seq), input); err != nil {
		_ = s.store.MarkInputFailed(ctx, detail.RunID, seq, err.Error())
		return SendInputResult{}, err
	}
	if err := s.store.MarkInputAccepted(ctx, detail.RunID, seq); err != nil {
		return SendInputResult{}, err
	}
	return SendInputResult{RunID: detail.RunID, Run: detail.Run, InputSeq: seq}, nil
}

func (s *Server) closeWorktree(ctx context.Context, params CloseWorktreeParams) (CloseWorktreeResult, error) {
	if params.ReplayKey != "" {
		hash := requestHash(CloseWorktreeParams{Workspace: params.Workspace})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "closeWorktree", hash)
		if err != nil {
			return CloseWorktreeResult{}, err
		}
		if replayed {
			var result CloseWorktreeResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return CloseWorktreeResult{}, err
			}
			return result, nil
		}
		result, err := s.closeWorktree(ctx, CloseWorktreeParams{Workspace: params.Workspace})
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "closeWorktree", hash)
			return CloseWorktreeResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "closeWorktree", hash)
			return CloseWorktreeResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "closeWorktree", hash, string(body)); err != nil {
			return CloseWorktreeResult{}, err
		}
		return result, nil
	}
	wt, err := s.store.ActiveManagedWorktree(ctx, params.Workspace)
	if err != nil {
		if closing, closeErr := s.store.ClosingManagedWorktree(ctx, params.Workspace); closeErr == nil {
			result := CloseWorktreeResult{
				CloseAttempt: storage.Handle("cls_", closing.AttemptID),
				Target:       map[string]any{"kind": "Workspace", "workspace": storage.Handle("wks_", closing.WorkspaceID)},
				Workspace:    storage.Handle("wks_", closing.WorkspaceID),
				WorkspaceKey: closing.WorkspaceKey,
				Path:         closing.Path,
				Branch:       closing.Branch,
			}
			if err := s.finishClosingWorktree(ctx, closing); err != nil {
				return CloseWorktreeResult{}, err
			}
			result.Status = "Removed"
			result.Closed = true
			return result, nil
		}
		return CloseWorktreeResult{}, err
	}
	status, err := gitx.Status(ctx, wt.Path)
	if err != nil {
		return CloseWorktreeResult{}, err
	}
	findings, err := s.evaluateWorktreeClose(ctx, wt, status)
	if err != nil {
		return CloseWorktreeResult{}, err
	}
	workspaceHandle := storage.Handle("wks_", wt.WorkspaceID)
	storedFindings := make([]storage.SafetyFinding, 0, len(findings))
	for _, finding := range findings {
		storedFindings = append(storedFindings, storage.SafetyFinding{
			Blocker: string(finding.Blocker), Severity: finding.Severity, Location: finding.Location, Message: finding.Message, Evidence: finding.Evidence,
		})
	}
	if _, err := s.store.RecordWorkspaceSafetyCheck(ctx, wt.WorkspaceID, "WorktreeClose", storedFindings); err != nil {
		return CloseWorktreeResult{}, err
	}
	result := CloseWorktreeResult{
		Target:       map[string]any{"kind": "Workspace", "workspace": workspaceHandle},
		Workspace:    workspaceHandle,
		WorkspaceKey: wt.WorkspaceKey,
		Path:         wt.Path,
		Branch:       wt.Branch,
	}
	if len(findings) > 0 {
		result.Blockers = make([]CloseWorktreeBlock, 0, len(findings))
		for _, finding := range findings {
			result.Blockers = append(result.Blockers, CloseWorktreeBlock{
				Blocker:  string(finding.Blocker),
				Summary:  finding.Message,
				Evidence: finding.Evidence,
			})
		}
		attemptID, err := s.store.RecordBlockedWorktreeClose(ctx, wt, storedFindings)
		if err != nil {
			return CloseWorktreeResult{}, err
		}
		result.CloseAttempt = storage.Handle("cls_", attemptID)
		result.Status = "Blocked"
		return result, nil
	}
	attemptID, err := s.store.BeginWorktreeRemoval(ctx, wt)
	if err != nil {
		return CloseWorktreeResult{}, err
	}
	result.CloseAttempt = storage.Handle("cls_", attemptID)
	if err := s.finishClosingWorktree(ctx, storage.ClosingWorktree{ManagedWorktree: wt, AttemptID: attemptID}); err != nil {
		return CloseWorktreeResult{}, err
	}
	result.Status = "Removed"
	result.Closed = true
	return result, nil
}

func (s *Server) repair(ctx context.Context, params RepairParams) (RepairResult, error) {
	if params.ReplayKey != "" {
		hash := requestHash(RepairParams{Key: params.Key})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "repair", hash)
		if err != nil {
			return RepairResult{}, err
		}
		if replayed {
			var result RepairResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return RepairResult{}, err
			}
			return result, nil
		}
		result, err := s.repair(ctx, RepairParams{Key: params.Key})
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "repair", hash)
			return RepairResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "repair", hash)
			return RepairResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "repair", hash, string(body)); err != nil {
			return RepairResult{}, err
		}
		return result, nil
	}
	session, ok := strings.CutPrefix(params.Key, "lost-tmux-target:")
	if !ok || session == "" {
		return RepairResult{}, errors.New("RepairNotFound")
	}
	detail, err := s.store.Session(ctx, session)
	if err != nil {
		return RepairResult{}, err
	}
	if detail.RunID == "" {
		return RepairResult{}, errors.New("RunNotLive")
	}
	ok, err = tmuxClient().TargetExists(ctx, detail.TmuxSessionName)
	if err != nil {
		return RepairResult{}, err
	}
	if ok {
		return RepairResult{}, errors.New("RepairNotNeeded")
	}
	detailJSON, err := json.Marshal("tmux target " + detail.TmuxSessionName + " is missing")
	if err != nil {
		return RepairResult{}, err
	}
	payload := `{"outcome":"RunnerFailed","failure":{"code":"TmuxTargetMissing","detail":` + string(detailJSON) + `}}`
	if err := s.store.RecordTerminalOutcome(ctx, detail.RunID, "RunnerFailed", payload); err != nil {
		return RepairResult{}, err
	}
	if err := s.store.RecordRunRepairCompleted(ctx, detail.RunID, `{"repair":"LostTmuxTarget","session":"`+session+`"}`); err != nil {
		return RepairResult{}, err
	}
	return RepairResult{Key: params.Key, Session: session, Status: "Repaired"}, nil
}

func (s *Server) doctor(ctx context.Context) (DoctorReport, error) {
	report := DoctorReport{StateDB: s.cfg.StateDB, Issues: []DoctorIssue{}}
	for _, cap := range provider.Catalog() {
		if _, err := exec.LookPath(cap.Command); err != nil {
			report.Issues = append(report.Issues, DoctorIssue{
				Code: "ProviderCLIMissing", Severity: "Warning",
				Target:  map[string]any{"provider": cap.ProviderKey, "command": cap.Command},
				Summary: cap.DisplayName + " CLI is missing.",
				Detail:  err.Error(),
			})
		}
	}
	sessions, err := s.listSessions(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	for _, session := range sessions {
		switch session.RunStatus {
		case "LostTmuxTarget":
			report.Issues = append(report.Issues, DoctorIssue{
				Code: "TmuxTargetMissing", Severity: "Blocker",
				Target:  map[string]any{"session": session.Session, "workspace": session.Workspace},
				Summary: "Expected tmux target is missing.",
				Detail:  "Session " + session.Session + " has a live runner binding but no tmux target.",
				Repair:  "lost-tmux-target:" + session.Session,
			})
		case "LostRunner":
			report.Issues = append(report.Issues, DoctorIssue{
				Code: "RunnerHeartbeatExpired", Severity: "Blocker",
				Target:  map[string]any{"session": session.Session, "workspace": session.Workspace},
				Summary: "Runner heartbeat expired.",
				Detail:  "Session " + session.Session + " still has an active binding but no recent heartbeat.",
			})
		case "RepairRequired":
			report.Issues = append(report.Issues, DoctorIssue{
				Code: "RepairRequired", Severity: "Blocker",
				Target:  map[string]any{"session": session.Session, "workspace": session.Workspace},
				Summary: "Session requires repair.",
				Detail:  "Session " + session.Session + " has contradictory durable state.",
			})
		}
	}
	envs, err := s.store.LaunchEnvSnapshots(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	for _, snapshot := range envs {
		set, _ := snapshot.Env["set"].(map[string]any)
		for name, raw := range set {
			entry, _ := raw.(map[string]any)
			value, _ := entry["value"].(string)
			if !secretEnvName(name) && !highEntropyValue(value) {
				continue
			}
			report.Issues = append(report.Issues, DoctorIssue{
				Code: "LaunchEnvSecretStored", Severity: "Defect",
				Target:  map[string]any{"session": snapshot.Session, "run": snapshot.Run, "env": name},
				Summary: "Launch environment snapshot contains an unredacted secret-shaped value.",
				Detail:  "Environment variable " + name + " should have been recorded only by name in the redacted set.",
			})
		}
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	for _, project := range projects {
		worktrees, err := s.store.ListActiveManagedWorktrees(ctx, project.ID)
		if err != nil {
			return DoctorReport{}, err
		}
		knownBranches := map[string]bool{}
		for _, wt := range worktrees {
			knownBranches[wt.Branch] = true
			status, err := gitx.Status(ctx, wt.Path)
			if err != nil {
				return DoctorReport{}, err
			}
			if status.Presence == gitx.PresenceMissing {
				report.Issues = append(report.Issues, DoctorIssue{
					Code: "GitWorktreeMissing", Severity: "Blocker",
					Target:  map[string]any{"workspace": storage.Handle("wks_", wt.WorkspaceID), "workspaceKey": wt.WorkspaceKey, "path": wt.Path},
					Summary: "Expected git worktree is missing.",
					Detail:  wt.Path + " does not exist.",
				})
				continue
			}
			if status.Presence == gitx.PresenceNotAWorktree {
				report.Issues = append(report.Issues, DoctorIssue{
					Code: "NotExpectedWorktree", Severity: "Blocker",
					Target:  map[string]any{"workspace": storage.Handle("wks_", wt.WorkspaceID), "workspaceKey": wt.WorkspaceKey, "path": wt.Path},
					Summary: "Path is not the expected git worktree.",
					Detail:  wt.Path + " exists but is not an agency git worktree.",
				})
			}
			raw, err := os.ReadFile(wt.MarkerFilePath)
			if err != nil {
				report.Issues = append(report.Issues, DoctorIssue{
					Code: "OwnershipMarkerMismatch", Severity: "Blocker",
					Target:  map[string]any{"workspace": storage.Handle("wks_", wt.WorkspaceID), "workspaceKey": wt.WorkspaceKey, "path": wt.MarkerFilePath},
					Summary: "Managed worktree ownership marker is missing or unreadable.",
					Detail:  err.Error(),
				})
				continue
			}
			if strings.TrimSpace(string(raw)) != wt.MarkerFileHash {
				report.Issues = append(report.Issues, DoctorIssue{
					Code: "OwnershipMarkerMismatch", Severity: "Blocker",
					Target:  map[string]any{"workspace": storage.Handle("wks_", wt.WorkspaceID), "workspaceKey": wt.WorkspaceKey, "path": wt.MarkerFilePath},
					Summary: "Managed worktree ownership marker does not match storage.",
					Detail:  "Marker content does not match the managed workspace identity.",
				})
			}
		}
		closing, err := s.store.ListClosingManagedWorktrees(ctx, project.ID)
		if err != nil {
			return DoctorReport{}, err
		}
		for _, wt := range closing {
			knownBranches[wt.Branch] = true
		}
		branches, err := gitx.LocalBranches(ctx, project.RootPath)
		if err != nil {
			return DoctorReport{}, err
		}
		for _, branch := range branches {
			if !strings.HasPrefix(branch, "agency/") || knownBranches[branch] {
				continue
			}
			report.Issues = append(report.Issues, DoctorIssue{
				Code: "OrphanedAgencyBranch", Severity: "Info",
				Target:  map[string]any{"project": project.Key, "branch": branch},
				Summary: "Agency-owned branch is not linked to an active managed worktree.",
				Detail:  "Branch " + branch + " remains in project " + project.Key + ". Doctor reports it but does not delete it.",
			})
		}
	}
	return report, nil
}

func (s *Server) sessionDiff(ctx context.Context, session string) (DiffResult, error) {
	detail, err := s.store.Session(ctx, session)
	if err != nil {
		return DiffResult{}, err
	}
	base, err := s.store.ProjectDefaultBaseRef(ctx, detail.Summary.Project)
	if err != nil {
		return DiffResult{}, err
	}
	if detail.Summary.WorkspaceKey != "project_root" {
		wt, err := s.store.ActiveManagedWorktree(ctx, detail.Summary.WorkspaceKey)
		if err != nil {
			return DiffResult{}, err
		}
		base = wt.BaseSHA
	}
	diff, err := gitx.Diff(ctx, detail.Summary.Path, base)
	if err != nil {
		return DiffResult{}, err
	}
	return DiffResult{Base: diff.Base, Stat: diff.Stat, Patch: diff.Patch}, nil
}

func (s *Server) listWorktrees(ctx context.Context, cwd string) ([]WorktreeSummary, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	project, err := s.store.ProjectForPath(ctx, cwd)
	if err != nil {
		return nil, err
	}
	worktrees, err := s.store.ListActiveManagedWorktrees(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	closingWorktrees, err := s.store.ListClosingManagedWorktrees(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	out := make([]WorktreeSummary, 0, len(worktrees)+len(closingWorktrees))
	for _, wt := range worktrees {
		summary, err := s.worktreeSummary(ctx, wt)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	for _, closing := range closingWorktrees {
		summary, err := s.closingWorktreeSummary(ctx, closing)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

func (s *Server) worktreeStatus(ctx context.Context, workspace string) (WorktreeStatusResult, error) {
	wt, err := s.store.ActiveManagedWorktree(ctx, workspace)
	if err != nil {
		if closing, closeErr := s.store.ClosingManagedWorktree(ctx, workspace); closeErr == nil {
			return s.closingWorktreeSummary(ctx, closing)
		}
		return WorktreeStatusResult{}, err
	}
	return s.worktreeSummary(ctx, wt)
}

func (s *Server) closingWorktreeSummary(ctx context.Context, closing storage.ClosingWorktree) (WorktreeSummary, error) {
	wt := closing.ManagedWorktree
	status, err := gitx.Status(ctx, wt.Path)
	if err != nil {
		return WorktreeStatusResult{}, err
	}
	status = gitx.StatusWithoutPaths(status, ".agency-worktree")
	sessions, err := s.store.SessionHandlesForWorkspace(ctx, wt.WorkspaceID)
	if err != nil {
		return WorktreeStatusResult{}, err
	}
	return WorktreeSummary{
		Workspace: storage.Handle("wks_", wt.WorkspaceID), WorkspaceKey: wt.WorkspaceKey, Project: wt.ProjectKey,
		Path: wt.Path, ManagedWorktree: true, Branch: wt.Branch, BaseRef: wt.BaseRef, BaseSHA: wt.BaseSHA,
		Git:      gitStatusMap(status),
		Close:    map[string]any{"closable": false, "summary": string(gitx.CloseSummaryRepairRequired), "blockers": []string{}},
		Sessions: sessions,
		Marker:   worktreeMarkerStatus(wt),
		Diff:     worktreeDiffStatus(ctx, wt),
	}, nil
}

func (s *Server) worktreeSummary(ctx context.Context, wt storage.ManagedWorktree) (WorktreeSummary, error) {
	status, err := gitx.Status(ctx, wt.Path)
	if err != nil {
		return WorktreeStatusResult{}, err
	}
	status = gitx.StatusWithoutPaths(status, ".agency-worktree")
	findings, err := s.evaluateWorktreeClose(ctx, wt, status)
	if err != nil {
		return WorktreeStatusResult{}, err
	}
	blockers := make([]gitx.CloseBlocker, 0, len(findings))
	rawBlockers := make([]string, 0, len(blockers))
	for _, finding := range findings {
		blockers = append(blockers, finding.Blocker)
		rawBlockers = append(rawBlockers, string(finding.Blocker))
	}
	sessions, err := s.store.SessionHandlesForWorkspace(ctx, wt.WorkspaceID)
	if err != nil {
		return WorktreeStatusResult{}, err
	}
	return WorktreeSummary{
		Workspace: storage.Handle("wks_", wt.WorkspaceID), WorkspaceKey: wt.WorkspaceKey, Project: wt.ProjectKey,
		Path: wt.Path, ManagedWorktree: true, Branch: wt.Branch, BaseRef: wt.BaseRef, BaseSHA: wt.BaseSHA,
		Git:      gitStatusMap(status),
		Close:    map[string]any{"closable": len(blockers) == 0, "summary": string(gitx.SummarizeClose(blockers)), "blockers": rawBlockers},
		Sessions: sessions,
		Marker:   worktreeMarkerStatus(wt),
		Diff:     worktreeDiffStatus(ctx, wt),
	}, nil
}

func gitStatusMap(status gitx.StatusSnapshot) map[string]any {
	return map[string]any{
		"summary": string(status.Summary), "presence": string(status.Presence), "tree": string(status.Tree),
		"untracked": status.Untracked, "ignoredUserFiles": status.IgnoredUserFiles, "conflicts": status.Conflicts, "upstream": string(status.Upstream),
	}
}

func worktreeMarkerStatus(wt storage.ManagedWorktree) map[string]any {
	raw, err := os.ReadFile(wt.MarkerFilePath)
	if err != nil {
		return map[string]any{"path": wt.MarkerFilePath, "present": false, "matches": false, "error": err.Error()}
	}
	return map[string]any{"path": wt.MarkerFilePath, "present": true, "matches": strings.TrimSpace(string(raw)) == wt.MarkerFileHash}
}

func worktreeDiffStatus(ctx context.Context, wt storage.ManagedWorktree) map[string]any {
	diff, err := gitx.Diff(ctx, wt.Path, wt.BaseSHA)
	if err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	return map[string]any{"available": true, "base": diff.Base, "stat": diff.Stat}
}

func (s *Server) worktreeDiff(ctx context.Context, workspace string) (DiffResult, error) {
	wt, err := s.store.ActiveManagedWorktree(ctx, workspace)
	if err != nil {
		return DiffResult{}, err
	}
	diff, err := gitx.Diff(ctx, wt.Path, wt.BaseSHA)
	if err != nil {
		return DiffResult{}, err
	}
	return DiffResult{Base: diff.Base, Stat: diff.Stat, Patch: diff.Patch}, nil
}

func (s *Server) attachCommand(ctx context.Context, session string) (AttachResult, error) {
	detail, err := s.store.Session(ctx, session)
	if err != nil {
		return AttachResult{}, err
	}
	if detail.Summary.RunStatus == "Closed" {
		return AttachResult{}, errors.New("SessionClosed")
	}
	tmux := tmuxClient()
	ok, err := tmux.TargetExists(ctx, detail.TmuxSessionName)
	if err != nil {
		return AttachResult{}, err
	}
	if !ok {
		return AttachResult{}, errors.New("LostTmuxTarget")
	}
	argv, err := tmux.AttachCommand(detail.TmuxSessionName)
	if err != nil {
		return AttachResult{}, err
	}
	return AttachResult{Argv: argv}, nil
}

func (s *Server) initProject(ctx context.Context, cwd, replayKey string) (ProjectResult, error) {
	if replayKey != "" {
		hash := requestHash(ProjectParams{CWD: cwd})
		raw, replayed, err := s.store.BeginIdempotency(ctx, replayKey, "initProject", hash)
		if err != nil {
			return ProjectResult{}, err
		}
		if replayed {
			var result ProjectResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return ProjectResult{}, err
			}
			return result, nil
		}
		result, err := s.initProject(ctx, cwd, "")
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, replayKey, "initProject", hash)
			return ProjectResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, replayKey, "initProject", hash)
			return ProjectResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, replayKey, "initProject", hash, string(body)); err != nil {
			return ProjectResult{}, err
		}
		return result, nil
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ProjectResult{}, err
		}
	}
	root, err := gitx.Root(ctx, cwd)
	if err != nil {
		return ProjectResult{}, err
	}
	project, err := s.store.EnsureProject(ctx, root)
	if err != nil {
		return ProjectResult{}, err
	}
	if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
		return ProjectResult{}, err
	}
	return publicProject(project), nil
}

func (s *Server) listProjects(ctx context.Context) ([]ProjectResult, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectResult, 0, len(projects))
	for _, project := range projects {
		out = append(out, publicProject(project))
	}
	return out, nil
}

func (s *Server) saveProfile(ctx context.Context, params ProfileSaveParams) (ProfileSaveResult, error) {
	if params.Profile == "" {
		return ProfileSaveResult{}, errors.New("profile is required")
	}
	existingProvider, existingModel, existingEffort, existingPolicy := "", "", "", ""
	if _, gotProvider, gotModel, gotEffort, gotPolicy, err := s.store.ProfileRevision(ctx, params.Profile); err == nil {
		existingProvider, existingModel, existingEffort, existingPolicy = gotProvider, gotModel, gotEffort, gotPolicy
	} else if params.Provider == "" {
		return ProfileSaveResult{}, err
	}
	if params.Provider == "" {
		params.Provider = existingProvider
	}
	if params.Model == "" {
		params.Model = existingModel
	}
	if params.Effort == "" {
		params.Effort = existingEffort
	}
	policy := map[string]string{}
	if existingProvider == params.Provider && existingPolicy != "" {
		if err := json.Unmarshal([]byte(existingPolicy), &policy); err != nil {
			return ProfileSaveResult{}, err
		}
	}
	switch params.Provider {
	case provider.KeyClaude:
		if params.Model == "" {
			params.Model = "sonnet"
		}
		if params.Effort == "" {
			params.Effort = "high"
		}
		if params.PermissionMode == "" {
			params.PermissionMode = policy["permissionMode"]
		}
		if params.PermissionMode == "" {
			params.PermissionMode = "default"
		}
	case provider.KeyCodex:
		if params.Model == "" {
			params.Model = "gpt-5.5"
		}
		if params.Effort == "" {
			params.Effort = "high"
		}
		if params.SandboxMode == "" {
			params.SandboxMode = policy["sandboxMode"]
		}
		if params.SandboxMode == "" {
			params.SandboxMode = "workspace-write"
		}
		if params.ApprovalPolicy == "" {
			params.ApprovalPolicy = policy["approvalPolicy"]
		}
		if params.ApprovalPolicy == "" {
			params.ApprovalPolicy = "on-request"
		}
	}
	if _, err := provider.BuildLaunchPlan(provider.LaunchInput{
		ProviderKey: params.Provider, Model: params.Model, Effort: params.Effort,
		PermissionMode: params.PermissionMode, SandboxMode: params.SandboxMode, ApprovalPolicy: params.ApprovalPolicy,
	}); err != nil {
		return ProfileSaveResult{}, err
	}
	policyJSON := "{}"
	if params.Provider == provider.KeyClaude {
		raw, err := json.Marshal(map[string]string{"permissionMode": params.PermissionMode})
		if err != nil {
			return ProfileSaveResult{}, err
		}
		policyJSON = string(raw)
	}
	if params.Provider == provider.KeyCodex {
		raw, err := json.Marshal(map[string]string{"sandboxMode": params.SandboxMode, "approvalPolicy": params.ApprovalPolicy})
		if err != nil {
			return ProfileSaveResult{}, err
		}
		policyJSON = string(raw)
	}
	if err := s.store.UpsertProfile(ctx, params.Profile, params.Provider, params.Model, params.Effort, policyJSON); err != nil {
		return ProfileSaveResult{}, err
	}
	if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
		return ProfileSaveResult{}, err
	}
	return ProfileSaveResult{Profile: params.Profile, Provider: params.Provider}, nil
}

func (s *Server) setDefaultProfile(ctx context.Context, params ProfileDefaultParams) (ProfileDefaultResult, error) {
	cwd := params.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ProfileDefaultResult{}, err
		}
	}
	project, err := s.store.ProjectForPath(ctx, cwd)
	if err != nil {
		return ProfileDefaultResult{}, err
	}
	if err := s.store.SetProjectDefaultProfile(ctx, project.Key, params.Profile); err != nil {
		return ProfileDefaultResult{}, err
	}
	if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
		return ProfileDefaultResult{}, err
	}
	return ProfileDefaultResult{Project: project.Key, Profile: params.Profile}, nil
}

func (s *Server) setConfig(ctx context.Context, params ConfigSetParams) (ConfigSetResult, error) {
	if params.ReplayKey != "" {
		hash := requestHash(ConfigSetParams{CWD: params.CWD, Key: params.Key, Value: params.Value})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "setConfig", hash)
		if err != nil {
			return ConfigSetResult{}, err
		}
		if replayed {
			var result ConfigSetResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return ConfigSetResult{}, err
			}
			return result, nil
		}
		copyParams := params
		copyParams.ReplayKey = ""
		result, err := s.setConfig(ctx, copyParams)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "setConfig", hash)
			return ConfigSetResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "setConfig", hash)
			return ConfigSetResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "setConfig", hash, string(body)); err != nil {
			return ConfigSetResult{}, err
		}
		return result, nil
	}
	if params.Key == "defaults.profile" {
		_, err := s.setDefaultProfile(ctx, ProfileDefaultParams{CWD: params.CWD, Profile: params.Value})
		if err != nil {
			return ConfigSetResult{}, err
		}
		return ConfigSetResult{Key: params.Key, Value: params.Value}, nil
	}
	if params.Key == "defaults.base_ref" {
		project, err := s.projectForConfig(ctx, params.CWD)
		if err != nil {
			return ConfigSetResult{}, err
		}
		if _, err := gitx.ResolveRef(ctx, project.RootPath, params.Value); err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.SetProjectDefaultBaseRef(ctx, project.Key, params.Value); err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
			return ConfigSetResult{}, err
		}
		return ConfigSetResult{Key: params.Key, Value: params.Value}, nil
	}
	if params.Key == "defaults.host" {
		project, err := s.projectForConfig(ctx, params.CWD)
		if err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.SetProjectDefaultHost(ctx, project.Key, params.Value); err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
			return ConfigSetResult{}, err
		}
		return ConfigSetResult{Key: params.Key, Value: params.Value}, nil
	}
	if params.Key == "defaults.worktree_mode" {
		project, err := s.projectForConfig(ctx, params.CWD)
		if err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.SetProjectDefaultWorktreeMode(ctx, project.Key, params.Value); err != nil {
			return ConfigSetResult{}, err
		}
		value, err := s.store.ProjectDefaultWorktreeMode(ctx, project.Key)
		if err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
			return ConfigSetResult{}, err
		}
		return ConfigSetResult{Key: params.Key, Value: storage.WorktreeModeConfigValue(value)}, nil
	}
	if key, ok := hostAccessConfigKey(params.Key); ok {
		if _, err := s.store.UpsertHostAccess(ctx, key, params.Value); err != nil {
			return ConfigSetResult{}, err
		}
		if err := s.store.RecordEffectiveConfigRevision(ctx, s.cfg.StateDB); err != nil {
			return ConfigSetResult{}, err
		}
		return ConfigSetResult{Key: params.Key, Value: params.Value}, nil
	}
	return ConfigSetResult{}, errors.New("unknown config key")
}

func (s *Server) getConfig(ctx context.Context, params ConfigGetParams) (ConfigSetResult, error) {
	project, err := s.projectForConfig(ctx, params.CWD)
	if err != nil {
		return ConfigSetResult{}, err
	}
	effective, err := s.store.LatestEffectiveConfig(ctx)
	if err != nil {
		return ConfigSetResult{}, err
	}
	value, err := configValueFromEffective(effective, project.Key, params.Key)
	if err != nil {
		return ConfigSetResult{}, err
	}
	return ConfigSetResult{Key: params.Key, Value: value}, nil
}

func configValueFromEffective(effective map[string]any, projectKey, key string) (string, error) {
	projects, ok := effective["projects"].(map[string]any)
	if !ok {
		return "", errors.New("EffectiveConfigMissingProjects")
	}
	project, ok := projects[projectKey].(map[string]any)
	if !ok {
		return "", errors.New("EffectiveConfigMissingProject")
	}
	switch key {
	case "defaults.profile":
		return stringConfigValue(project, "defaultProfile")
	case "defaults.base_ref":
		return stringConfigValue(project, "baseRef")
	case "defaults.host":
		return stringConfigValue(project, "defaultHost")
	case "defaults.worktree_mode":
		value, err := stringConfigValue(project, "worktreeMode")
		if err != nil {
			return "", err
		}
		return storage.WorktreeModeConfigValue(value), nil
	default:
		hostKey, ok := hostAccessConfigKey(key)
		if !ok {
			return "", errors.New("unknown config key")
		}
		hosts, ok := effective["hosts"].(map[string]any)
		if !ok {
			return "", errors.New("EffectiveConfigMissingHosts")
		}
		host, ok := hosts[hostKey].(map[string]any)
		if !ok {
			return "", errors.New("HostNotFound")
		}
		access, ok := host["access"].(map[string]any)
		if !ok {
			return "", errors.New("EffectiveConfigMissingHostAccess")
		}
		mode, err := stringConfigValue(access, "mode")
		if err != nil {
			return "", err
		}
		alias, _ := access["hostAlias"].(string)
		switch mode {
		case "Local":
			return "local", nil
		case "Ssh":
			return "ssh:" + alias, nil
		case "AttachOnlyMosh":
			return "mosh:" + alias, nil
		default:
			return "", errors.New("InvalidHostAccess")
		}
	}
}

func stringConfigValue(row map[string]any, key string) (string, error) {
	value, ok := row[key].(string)
	if !ok || value == "" {
		return "", errors.New("EffectiveConfigMissingValue")
	}
	return value, nil
}

func (s *Server) projectForConfig(ctx context.Context, cwd string) (storage.Project, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return storage.Project{}, err
		}
	}
	return s.store.ProjectForPath(ctx, cwd)
}

func hostAccessConfigValue(access storage.HostAccess) string {
	switch access.Mode {
	case "Local":
		return "local"
	case "Ssh":
		return "ssh:" + access.HostAlias
	case "AttachOnlyMosh":
		return "mosh:" + access.HostAlias
	default:
		return access.Mode
	}
}

func hostAccessConfigKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "hosts.") || !strings.HasSuffix(key, ".access") {
		return "", false
	}
	hostKey := strings.TrimSuffix(strings.TrimPrefix(key, "hosts."), ".access")
	if !storage.ValidHostKey(hostKey) {
		return "", false
	}
	return hostKey, true
}

func (s *Server) startSession(ctx context.Context, params StartSessionParams) (StartSessionResult, error) {
	if params.ReplayKey != "" {
		copyParams := params
		copyParams.ReplayKey = ""
		hash := requestHash(copyParams)
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "startSession", hash)
		if err != nil {
			return StartSessionResult{}, err
		}
		if replayed {
			var result StartSessionResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return StartSessionResult{}, err
			}
			return result, nil
		}
		result, err := s.startSession(ctx, copyParams)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "startSession", hash)
			return StartSessionResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "startSession", hash)
			return StartSessionResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "startSession", hash, string(body)); err != nil {
			return StartSessionResult{}, err
		}
		return result, nil
	}
	cwd := params.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return StartSessionResult{}, err
		}
	}
	project, err := s.store.ProjectForPath(ctx, cwd)
	if err != nil {
		gitRoot, rootErr := gitx.Root(ctx, cwd)
		if rootErr != nil {
			return StartSessionResult{}, rootErr
		}
		return StartSessionResult{}, fmt.Errorf("ProjectNotFound: %s", gitRoot)
	}
	workspaceID := project.ProjectRootWorkspace
	workspaceKey := "project_root"
	var profileRevisionID, gotProvider, defaultModel, defaultEffort, policy string
	if params.Profile != "" {
		profileRevisionID, gotProvider, defaultModel, defaultEffort, policy, err = s.store.ProfileRevision(ctx, params.Profile)
		if err == nil && params.Provider != "" && gotProvider != params.Provider {
			err = fmt.Errorf("profile %s is for provider %s, not %s", params.Profile, gotProvider, params.Provider)
		}
	} else {
		profileRevisionID, gotProvider, defaultModel, defaultEffort, policy, err = s.store.DefaultProfileRevision(ctx, project.ID, params.Provider)
	}
	if err != nil {
		return StartSessionResult{}, err
	}
	model := params.Model
	if model == "" {
		model = defaultModel
	}
	effort := params.Effort
	if effort == "" {
		effort = defaultEffort
	}
	permissionMode := params.PermissionMode
	if gotProvider == provider.KeyClaude && permissionMode == "" {
		permissionMode = "default"
	}
	sandboxMode := params.SandboxMode
	approvalPolicy := params.ApprovalPolicy
	if gotProvider == provider.KeyCodex {
		if sandboxMode == "" {
			sandboxMode = stringField(policy, "sandboxMode")
		}
		if approvalPolicy == "" {
			approvalPolicy = stringField(policy, "approvalPolicy")
		}
	}
	plan, err := provider.BuildLaunchPlan(provider.LaunchInput{
		ProviderKey: gotProvider, Model: model, Effort: effort, PermissionMode: permissionMode,
		SandboxMode: sandboxMode, ApprovalPolicy: approvalPolicy, AddDirs: params.AddDirs, AllowDangerous: params.AllowDangerous,
	})
	if err != nil {
		return StartSessionResult{}, err
	}
	if _, err := exec.LookPath(plan.Argv[0]); err != nil {
		return StartSessionResult{}, err
	}
	runnerBin, err := runnerBinary()
	if err != nil {
		return StartSessionResult{}, err
	}
	tmux := tmuxClient()
	if _, err := tmux.Probe(ctx); err != nil {
		return StartSessionResult{}, err
	}
	if _, err := s.reconcileTmuxServer(ctx); err != nil {
		return StartSessionResult{}, err
	}
	if (params.Worktree || project.DefaultWorktreeMode == "Always") && !params.NoWorktree {
		baseRef := params.BaseRef
		if baseRef == "" {
			baseRef = project.DefaultBaseRef
		}
		if baseRef == "" {
			baseRef = "HEAD"
		}
		worktreeName := params.WorktreeName
		if worktreeName == "" {
			worktreeName = slug(params.Title)
		}
		if worktreeName == "" {
			worktreeName = "session"
		}
		if worktreeName == "." || worktreeName == ".." {
			return StartSessionResult{}, errors.New("InvalidWorktreeName")
		}
		for _, r := range worktreeName {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
				continue
			}
			return StartSessionResult{}, errors.New("InvalidWorktreeName")
		}
		branch := "agency/" + worktreeName
		path := filepath.Join(project.ManagedWorktreeRoot, worktreeName)
		rel, err := filepath.Rel(project.ManagedWorktreeRoot, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return StartSessionResult{}, errors.New("InvalidWorktreePath")
		}
		if err := os.MkdirAll(project.ManagedWorktreeRoot, 0755); err != nil {
			return StartSessionResult{}, err
		}
		baseSHA, err := gitx.ResolveRef(ctx, project.RootPath, baseRef)
		if err != nil {
			return StartSessionResult{}, err
		}
		workspaceID, err = s.store.AddManagedWorkspace(ctx, project.ID, worktreeName, path, branch, baseRef, baseSHA, baseSHA)
		if err != nil {
			return StartSessionResult{}, err
		}
		markerPath := filepath.Join(path, ".agency-worktree")
		cleanupWorktree := func() {
			_ = os.Remove(markerPath)
			_ = gitx.RemoveWorktree(context.Background(), project.RootPath, path)
			_ = s.store.DeleteUnpublishedManagedWorkspace(context.Background(), workspaceID)
		}
		createdWorktree, err := gitx.AddWorktree(ctx, project.RootPath, path, branch, baseSHA)
		if err != nil {
			_ = s.store.DeleteUnpublishedManagedWorkspace(context.Background(), workspaceID)
			return StartSessionResult{}, err
		}
		if createdWorktree.HeadSHA != baseSHA {
			cleanupWorktree()
			return StartSessionResult{}, errors.New("ManagedWorktreeHeadMismatch")
		}
		if err := os.WriteFile(markerPath, []byte(workspaceID+"\n"), 0600); err != nil {
			cleanupWorktree()
			return StartSessionResult{}, err
		}
		if err := s.store.PublishManagedWorkspace(ctx, workspaceID); err != nil {
			cleanupWorktree()
			return StartSessionResult{}, err
		}
		cwd = path
		workspaceKey = worktreeName
	}
	created, err := s.store.CreateRunIntent(ctx, storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: workspaceID, ProfileRevisionID: profileRevisionID,
		HostID: project.HostID, Title: params.Title, WorkingDirectory: cwd,
		Argv: plan.Argv, Env: redactedLaunchEnv(params.Env),
	})
	if err != nil {
		return StartSessionResult{}, err
	}
	created.Env = params.Env
	if err := startRunnerInTmux(ctx, s.store, tmux, runnerBin, created, runnerTmuxSession(created)); err != nil {
		_ = s.store.MarkStartFailed(ctx, created.RunID, err.Error())
		return StartSessionResult{}, err
	}
	s.startOutputSubscription(ctx, created.RunID, defaultRunnerSocket(created.RunID))
	if params.Prompt != "" {
		if _, err := s.sendInput(ctx, created.Session, []byte(params.Prompt+"\n"), ""); err != nil {
			return StartSessionResult{}, err
		}
	}
	return StartSessionResult{
		Session: created.Session, Run: created.Run, Provider: gotProvider, Title: params.Title,
		Workspace: storage.Handle("wks_", workspaceID), WorkspaceKey: workspaceKey, Path: cwd, Tmux: "agency:" + created.Session,
		Model: model, Effort: effort, Argv: created.Argv,
	}, nil
}

func (s *Server) startRun(ctx context.Context, params StartRunParams) (StartRunResult, error) {
	if params.ReplayKey != "" {
		hash := requestHash(StartRunParams{Session: params.Session})
		raw, replayed, err := s.store.BeginIdempotency(ctx, params.ReplayKey, "startRun", hash)
		if err != nil {
			return StartRunResult{}, err
		}
		if replayed {
			var result StartRunResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return StartRunResult{}, err
			}
			return result, nil
		}
		result, err := s.startRun(ctx, StartRunParams{Session: params.Session})
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "startRun", hash)
			return StartRunResult{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			_ = s.store.AbortIdempotency(ctx, params.ReplayKey, "startRun", hash)
			return StartRunResult{}, err
		}
		if err := s.store.CompleteIdempotency(ctx, params.ReplayKey, "startRun", hash, string(body)); err != nil {
			return StartRunResult{}, err
		}
		return result, nil
	}
	runnerBin, err := runnerBinary()
	if err != nil {
		return StartRunResult{}, err
	}
	tmux := tmuxClient()
	if _, err := tmux.Probe(ctx); err != nil {
		return StartRunResult{}, err
	}
	if _, err := s.reconcileTmuxServer(ctx); err != nil {
		return StartRunResult{}, err
	}
	created, err := s.store.CreateAdditionalRunFromPrevious(ctx, params.Session)
	if err != nil {
		return StartRunResult{}, err
	}
	if err := startRunnerInTmux(ctx, s.store, tmux, runnerBin, created, runnerTmuxSession(created)); err != nil {
		_ = s.store.MarkStartFailed(ctx, created.RunID, err.Error())
		return StartRunResult{}, err
	}
	s.startOutputSubscription(ctx, created.RunID, defaultRunnerSocket(created.RunID))
	return StartRunResult{Session: created.Session, Run: created.Run}, nil
}

func (s *Server) evaluateWorktreeClose(ctx context.Context, wt storage.ManagedWorktree, status gitx.StatusSnapshot) ([]safety.Finding, error) {
	worktrees, err := gitx.WorktreeList(ctx, wt.ProjectRoot)
	if err != nil {
		return nil, err
	}
	liveDetails, err := s.store.LiveSessionDetailsForWorkspace(ctx, wt.WorkspaceID)
	if err != nil {
		return nil, err
	}
	tmux := tmuxClient()
	findings, err := safety.EvaluateWorktreeClose(ctx, safety.WorktreeCloseInput{
		Worktree: wt, GitStatus: status, Worktrees: worktrees, LiveSessions: liveDetails,
		TargetExists: tmux.TargetExists,
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

func restoreWorktreeMarker(wt storage.ManagedWorktree) {
	_ = os.WriteFile(wt.MarkerFilePath, []byte(wt.MarkerFileHash+"\n"), 0600)
}

func startRunnerInTmux(ctx context.Context, store *storage.Store, tmux tmuxadapter.Adapter, runnerBin string, created storage.CreatedRun, sessionName string) error {
	socket := defaultRunnerSocket(created.RunID)
	args := []string{runnerBin, "-run-id", created.RunID, "-socket", socket, "-cwd", created.WorkingDir}
	envFile := ""
	if len(created.Env) > 0 {
		envFile = defaultRunnerEnvFile(created.RunID)
		if err := writeRunnerEnvFile(envFile, created.Env); err != nil {
			return err
		}
		args = append(args, "-env-file", envFile)
	}
	runnerCommand := provider.ShellPreview(append(append(args, "--"), created.Argv...))
	if _, err := tmux.CreateRunnerTarget(ctx, sessionName, runnerCommand); err != nil {
		if envFile != "" {
			_ = os.Remove(envFile)
		}
		return err
	}
	cleanupTarget := func() {
		if envFile != "" {
			_ = os.Remove(envFile)
		}
		_ = tmux.KillTarget(context.Background(), sessionName)
	}
	identity, err := tmux.ServerIdentity(ctx)
	if err != nil {
		cleanupTarget()
		return err
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		cleanupTarget()
		return err
	}
	if err := store.RecordTmuxServerIdentity(ctx, "default", string(raw)); err != nil {
		cleanupTarget()
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cleanupTarget()
			return fmt.Errorf("runner control socket did not appear: %s", socket)
		}
		time.Sleep(20 * time.Millisecond)
	}
	err = store.BindRunner(ctx, created.RunID, created.TmuxTargetID, socket)
	if err != nil {
		cleanupTarget()
	}
	return err
}

func writeRunnerEnvFile(path string, env []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	for _, pair := range env {
		name, _, ok := strings.Cut(pair, "=")
		if !ok || name == "" || strings.ContainsAny(pair, "\x00\n\r") {
			return fmt.Errorf("invalid environment assignment %q", pair)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(env, "\n")+"\n"), 0600)
}

func runnerTmuxSession(created storage.CreatedRun) string {
	if created.RunSeq <= 1 {
		return "agency-" + created.Session
	}
	return "agency-" + created.Session + "-run-" + strings.TrimPrefix(created.Run, "run_")
}

func defaultRunnerSocket(runID string) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "agency", "runners", runID+".sock")
}

func defaultRunnerEnvFile(runID string) string {
	return defaultRunnerSocket(runID) + ".env"
}

func runnerBinary() (string, error) {
	if v := os.Getenv("AGENCY_RUNNER_BIN"); v != "" {
		return v, nil
	}
	return exec.LookPath("agency-runner")
}

func tmuxClient() tmuxadapter.Adapter {
	if socket := os.Getenv("AGENCY_TMUX_SOCKET"); socket != "" {
		return tmuxadapter.NewWithSocketPath(socket)
	}
	return tmuxadapter.New()
}

func stringField(raw, key string) string {
	var data map[string]string
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return ""
	}
	return data[key]
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func publicProject(project storage.Project) ProjectResult {
	return ProjectResult{
		Project:              project.Key,
		DisplayName:          project.DisplayName,
		RootPath:             project.RootPath,
		DefaultBaseRef:       project.DefaultBaseRef,
		DefaultHost:          project.HostKey,
		DefaultWorktreeMode:  storage.WorktreeModeConfigValue(project.DefaultWorktreeMode),
		ManagedWorktreeRoot:  project.ManagedWorktreeRoot,
		ProjectRootWorkspace: "project_root",
	}
}

func redactedLaunchEnv(env []string) map[string]any {
	set := map[string]any{}
	var redacted []string
	seenRedacted := map[string]bool{}
	for _, pair := range env {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			continue
		}
		if secretEnvName(name) || highEntropyValue(value) {
			if !seenRedacted[name] {
				seenRedacted[name] = true
				redacted = append(redacted, name)
			}
			continue
		}
		set[name] = map[string]any{"value": value, "source": "command"}
	}
	return map[string]any{"set": set, "inheritedNames": []string{"PATH", "HOME"}, "redacted": redacted}
}

func secretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	return strings.HasSuffix(upper, "_API_KEY") ||
		strings.HasSuffix(upper, "_TOKEN") ||
		strings.HasSuffix(upper, "_SECRET") ||
		strings.HasPrefix(upper, "AWS_") ||
		strings.HasPrefix(upper, "ANTHROPIC_") ||
		strings.HasPrefix(upper, "OPENAI_")
}

func highEntropyValue(value string) bool {
	if len(value) < 32 || strings.ContainsAny(value, " \t\n\r") {
		return false
	}
	hasLetter := false
	hasDigit := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
		}
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

func requestHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func requestRunnerStop(socketPath string) (runnerproto.Termination, error) {
	return requestRunnerTerminal(socketPath, runnerproto.NewStop(true))
}

func requestRunnerKill(socketPath string) (runnerproto.Termination, error) {
	return requestRunnerTerminal(socketPath, runnerproto.NewKill())
}

func requestRunnerTerminal(socketPath string, msg any) (runnerproto.Termination, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return runnerproto.Termination{}, err
	}
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		return runnerproto.Termination{}, err
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewHello()); err != nil {
		return runnerproto.Termination{}, err
	}
	if err := runnerproto.WriteMessage(conn, msg); err != nil {
		return runnerproto.Termination{}, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return runnerproto.Termination{}, err
		}
		if exit, ok := msg.(runnerproto.Exit); ok {
			return exit.Termination, nil
		}
	}
	return runnerproto.Termination{}, errors.New("runner did not report exit before timeout")
}

func requestRunnerInput(socketPath string, seq int64, input []byte) error {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		return err
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewHello()); err != nil {
		return err
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSendInput(seq, input)); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}
		switch m := msg.(type) {
		case runnerproto.Ack:
			if m.Seq == seq {
				return nil
			}
			return fmt.Errorf("runner acknowledged input seq %d, expected %d", m.Seq, seq)
		case runnerproto.Nack:
			return fmt.Errorf("%s: %s", m.Code, m.Detail)
		}
	}
	return errors.New("runner did not acknowledge input before timeout")
}

func readRunnerPreamble(ctx context.Context, socketPath string) (runnerproto.Preamble, error) {
	conn, err := (&net.Dialer{Timeout: runnerDialTimeout}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return runnerproto.Preamble{}, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(runnerDialTimeout)); err != nil {
		return runnerproto.Preamble{}, err
	}
	return runnerproto.ReadPreamble(conn)
}

func readRunnerHeartbeat(ctx context.Context, socketPath string) (runnerproto.Heartbeat, error) {
	conn, err := (&net.Dialer{Timeout: runnerDialTimeout}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return runnerproto.Heartbeat{}, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(runnerDialTimeout)); err != nil {
		return runnerproto.Heartbeat{}, err
	}
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		return runnerproto.Heartbeat{}, err
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewHello()); err != nil {
		return runnerproto.Heartbeat{}, err
	}
	msg, err := runnerproto.ReadMessage(conn)
	if err != nil {
		return runnerproto.Heartbeat{}, err
	}
	heartbeat, ok := msg.(runnerproto.Heartbeat)
	if !ok {
		return runnerproto.Heartbeat{}, fmt.Errorf("runner returned %T instead of Heartbeat", msg)
	}
	return heartbeat, nil
}

func writeResponse(conn net.Conn, result any, err error) error {
	if err != nil {
		return json.NewEncoder(conn).Encode(response{OK: false, Error: err.Error()})
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(response{OK: true, Result: raw})
}

func removeStaleSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		return errors.New("SupervisorAlreadyRunning: live API socket exists")
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return os.Remove(path)
	}
	return nil
}

func verifyPeer(conn net.Conn) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("supervisor requires Unix socket clients")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		cred, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if controlErr != nil {
		return controlErr
	}
	if cred == nil || cred.Uid != uint32(os.Getuid()) {
		return errors.New("supervisor peer credential mismatch")
	}
	return nil
}
