// Package config is the configuration service boundary. It parses TOML config
// sources once at ingress, converts human ingress vocabulary into owned
// PascalCase enums, validates into owned types, merges sources by precedence,
// and exposes a typed effective configuration. Runtime services read the typed
// Config; they never see raw TOML tokens.
//
// Per docs/product/agency/schema.md the authoring format is TOML and the enum
// encoding contract is: agency-owned enums are PascalCase everywhere; config
// ingress values are idiomatic lowercase/kebab and are converted here at the
// boundary; provider vocabulary (model slugs, permission/sandbox/approval modes,
// efforts) is preserved verbatim.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the owned, validated effective configuration. Enum-typed fields hold
// PascalCase owned values; provider vocabulary fields are verbatim.
type Config struct {
	Version       int                     `json:"version"`
	Paths         Paths                   `json:"paths"`
	Defaults      Defaults                `json:"defaults"`
	UI            UI                      `json:"ui"`
	Timing        Timing                  `json:"timing"`
	Retention     Retention               `json:"retention"`
	Security      Security                `json:"security"`
	Hosts         map[string]Host         `json:"hosts"`
	TmuxServers   map[string]TmuxServer   `json:"tmuxServers"`
	Providers     map[string]Provider     `json:"providers"`
	Profiles      map[string]Profile      `json:"profiles"`
	Projects      map[string]Project      `json:"projects"`
	SafetyPolicy  SafetyPolicy            `json:"safetyPolicy"`
	Notifications map[string]Notification `json:"notifications"`

	// declared records which boolean keys this source actually set. It is
	// unexported (never serialized) and consulted only during merge, so a key
	// whose zero value is meaningful — every bool that defaults true — is
	// overlaid only when the source declared it, not when it merely decoded to
	// false by absence. See declaredFlags and merge.
	declared declaredFlags
}

// declaredFlags mirrors the boolean config keys whose false value is a real
// setting rather than "unset". For an int a zero already means unset, but a bool
// that defaults true is indistinguishable from an absent key once decoded, so
// the TOML metadata (md.IsDefined) is captured here at parse time.
type declaredFlags struct {
	securityPeerCred     bool
	securityTunnelToken  bool
	securityLoopbackOnly bool
	suspendResumeReset   bool
	showClosed           bool
}

func (d declaredFlags) or(other declaredFlags) declaredFlags {
	return declaredFlags{
		securityPeerCred:     d.securityPeerCred || other.securityPeerCred,
		securityTunnelToken:  d.securityTunnelToken || other.securityTunnelToken,
		securityLoopbackOnly: d.securityLoopbackOnly || other.securityLoopbackOnly,
		suspendResumeReset:   d.suspendResumeReset || other.suspendResumeReset,
		showClosed:           d.showClosed || other.showClosed,
	}
}

type Paths struct {
	StateDir   string `json:"stateDir"`
	RuntimeDir string `json:"runtimeDir"`
	LogDir     string `json:"logDir"`
}

type Defaults struct {
	Project      string `json:"project"`
	Host         string `json:"host"`
	Profile      string `json:"profile"`
	WorktreeMode string `json:"worktreeMode"` // owned enum: Prompt|Always|Never
	BaseRef      string `json:"baseRef"`
}

type UI struct {
	Theme      string `json:"theme"` // owned: System|Light|Dark
	RefreshMs  int    `json:"refreshMs"`
	ShowClosed bool   `json:"showClosed"`
}

type Timing struct {
	HeartbeatIntervalMs   int  `json:"heartbeatIntervalMs"`
	HeartbeatTTLMs        int  `json:"heartbeatTtlMs"`
	QuietThresholdMs      int  `json:"quietThresholdMs"`
	GracefulStopTimeoutMs int  `json:"gracefulStopTimeoutMs"`
	ReconcileIntervalMs   int  `json:"reconcileIntervalMs"`
	SuspendResumeReset    bool `json:"suspendResumeReset"`
}

type Retention struct {
	OutputRetentionDays      int `json:"outputRetentionDays"`
	EventRetentionDays       int `json:"eventRetentionDays"`
	StatusSnapshotWindow     int `json:"statusSnapshotWindow"`
	IdempotencyRetentionDays int `json:"idempotencyRetentionDays"`
	BusyTimeoutMs            int `json:"busyTimeoutMs"`
	JournalSizeLimitBytes    int `json:"journalSizeLimitBytes"`
}

type Security struct {
	SocketPeerCredentialCheck bool `json:"socketPeerCredentialCheck"`
	TCPTunnelRequiresToken    bool `json:"tcpTunnelRequiresToken"`
	TCPTunnelLoopbackOnly     bool `json:"tcpTunnelLoopbackOnly"`
}

// HostAccess is a HostAccessSpec discriminated by Mode (owned enum
// Local|Ssh|AttachOnlyMosh).
type HostAccess struct {
	Mode             string `json:"mode"`
	HostAlias        string `json:"hostAlias,omitempty"`
	SocketForwarding bool   `json:"socketForwarding,omitempty"`
}

type Host struct {
	DisplayName string     `json:"displayName"`
	Access      HostAccess `json:"access"`
	TmuxServer  string     `json:"tmuxServer"`
}

type TmuxServer struct {
	Socket        string `json:"socket"`
	SessionPrefix string `json:"sessionPrefix"`
}

// Provider control lists are provider vocabulary preserved verbatim.
type Provider struct {
	DisplayName      string   `json:"displayName"`
	Command          string   `json:"command"`
	Models           []string `json:"models"`
	Efforts          []string `json:"efforts"`
	PermissionModes  []string `json:"permissionModes"`
	SandboxModes     []string `json:"sandboxModes"`
	ApprovalPolicies []string `json:"approvalPolicies"`
}

type Profile struct {
	DisplayName    string `json:"displayName"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	PermissionMode string `json:"permissionMode,omitempty"`
	SandboxMode    string `json:"sandboxMode,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
}

type Project struct {
	DisplayName         string `json:"displayName"`
	Root                string `json:"root"`
	DefaultHost         string `json:"defaultHost"`
	DefaultProfile      string `json:"defaultProfile"`
	BaseRef             string `json:"baseRef"`
	ManagedWorktreeRoot string `json:"managedWorktreeRoot"`
	GitCommonDir        string `json:"gitCommonDir"`
	DefaultBranchRef    string `json:"defaultBranchRef"`
}

// SafetyPolicy is the parsed [safety.policies.strict] block. The structural
// blockers are always enforced and intentionally not represented as toggles.
type SafetyPolicy struct {
	BlockDirtyWorktree             bool     `json:"blockDirtyWorktree"`
	BlockUntrackedFiles            bool     `json:"blockUntrackedFiles"`
	BlockIgnoredUserFiles          bool     `json:"blockIgnoredUserFiles"`
	BlockConflicts                 bool     `json:"blockConflicts"`
	BlockUnpushedCommits           bool     `json:"blockUnpushedCommits"`
	BlockMissingUpstreamProof      bool     `json:"blockMissingUpstreamProof"`
	DangerousClaudePermissionModes []string `json:"dangerousClaudePermissionModes"`
	DangerousCodexSandboxModes     []string `json:"dangerousCodexSandboxModes"`
	DangerousCodexFlags            []string `json:"dangerousCodexFlags"`
	DangerRequiresExplicitRequest  bool     `json:"dangerRequiresExplicitRequest"`
}

type Notification struct {
	Type   string   `json:"type"` // owned enum: Terminal|Desktop
	Events []string `json:"events"`
}

// Source identifies a config file and its merge priority; higher priority wins.
type Source struct {
	Key      string
	Path     string
	Priority int
}

// Load reads, parses, converts, validates, and merges the given sources over the
// built-in defaults, in ascending priority order. Unknown TOML keys, unknown
// ingress enum values, and same-priority conflicts are typed errors.
func Load(sources []Source) (Config, error) {
	effective := Default()
	ordered := append([]Source(nil), sources...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	seenPriority := map[int]string{}
	for _, src := range ordered {
		if prev, ok := seenPriority[src.Priority]; ok {
			return Config{}, fmt.Errorf("config sources %q and %q share priority %d; priorities must be distinct", prev, src.Key, src.Priority)
		}
		seenPriority[src.Priority] = src.Key
		raw, err := os.ReadFile(src.Path)
		if err != nil {
			return Config{}, fmt.Errorf("config source %q: %w", src.Key, err)
		}
		parsed, err := parse(src.Key, raw)
		if err != nil {
			return Config{}, err
		}
		effective = merge(effective, parsed)
	}
	if err := validate(effective); err != nil {
		return Config{}, err
	}
	return effective, nil
}

// DiscoverSources returns the standard config sources in precedence order: the
// XDG/HOME base config file, plus the CLI-managed override file in the state
// directory (higher priority). Absent files are skipped, not errors.
func DiscoverSources(stateDir string) []Source {
	var sources []Source
	if base := baseConfigPath(); base != "" {
		if _, err := os.Stat(base); err == nil {
			sources = append(sources, Source{Key: "base", Path: base, Priority: 10})
		}
	}
	if stateDir != "" {
		override := OverridePath(stateDir)
		if _, err := os.Stat(override); err == nil {
			sources = append(sources, Source{Key: "cli", Path: override, Priority: 100})
		}
	}
	return sources
}

// OverridePath is the higher-priority override config file in the state
// directory, read by DiscoverSources above the base config. It is the single
// source of truth for that filename. (Runtime config changes made via
// `agency config set` are persisted to the state database, not this file.)
func OverridePath(stateDir string) string {
	return filepath.Join(stateDir, "config.override.toml")
}

func baseConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "agency", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agency", "config.toml")
}

// rawConfig mirrors the TOML shape with ingress (lowercase/kebab) values.
type rawConfig struct {
	Version   int                    `toml:"version"`
	Paths     rawPaths               `toml:"paths"`
	Defaults  rawDefaults            `toml:"defaults"`
	UI        rawUI                  `toml:"ui"`
	Timing    rawTiming              `toml:"timing"`
	Retention rawRetention           `toml:"retention"`
	Security  rawSecurity            `toml:"security"`
	Hosts     map[string]rawHost     `toml:"hosts"`
	Tmux      rawTmux                `toml:"tmux"`
	Providers map[string]rawProvider `toml:"providers"`
	Profiles  map[string]rawProfile  `toml:"profiles"`
	Projects  map[string]rawProject  `toml:"projects"`
	Safety    rawSafety              `toml:"safety"`
	Notif     rawNotifications       `toml:"notifications"`
}

type rawPaths struct {
	StateDir   string `toml:"state_dir"`
	RuntimeDir string `toml:"runtime_dir"`
	LogDir     string `toml:"log_dir"`
}
type rawDefaults struct {
	Project      string `toml:"project"`
	Host         string `toml:"host"`
	Profile      string `toml:"profile"`
	WorktreeMode string `toml:"worktree_mode"`
	BaseRef      string `toml:"base_ref"`
}
type rawUI struct {
	Theme      string `toml:"theme"`
	RefreshMs  int    `toml:"refresh_ms"`
	ShowClosed bool   `toml:"show_closed"`
}
type rawTiming struct {
	HeartbeatIntervalMs   int  `toml:"heartbeat_interval_ms"`
	HeartbeatTTLMs        int  `toml:"heartbeat_ttl_ms"`
	QuietThresholdMs      int  `toml:"quiet_threshold_ms"`
	GracefulStopTimeoutMs int  `toml:"graceful_stop_timeout_ms"`
	ReconcileIntervalMs   int  `toml:"reconcile_interval_ms"`
	SuspendResumeReset    bool `toml:"suspend_resume_reset"`
}
type rawRetention struct {
	OutputRetentionDays      int `toml:"output_retention_days"`
	EventRetentionDays       int `toml:"event_retention_days"`
	StatusSnapshotWindow     int `toml:"status_snapshot_window"`
	IdempotencyRetentionDays int `toml:"idempotency_retention_days"`
	BusyTimeoutMs            int `toml:"busy_timeout_ms"`
	JournalSizeLimitBytes    int `toml:"journal_size_limit_bytes"`
}
type rawSecurity struct {
	SocketPeerCredentialCheck bool `toml:"socket_peer_credential_check"`
	TCPTunnelRequiresToken    bool `toml:"tcp_tunnel_requires_token"`
	TCPTunnelLoopbackOnly     bool `toml:"tcp_tunnel_loopback_only"`
}
type rawHostAccess struct {
	Mode             string `toml:"mode"`
	HostAlias        string `toml:"host_alias"`
	SocketForwarding bool   `toml:"socket_forwarding"`
}
type rawHost struct {
	DisplayName string        `toml:"display_name"`
	Access      rawHostAccess `toml:"access"`
	TmuxServer  string        `toml:"tmux_server"`
}
type rawTmux struct {
	Servers map[string]rawTmuxServer `toml:"servers"`
}
type rawTmuxServer struct {
	Socket        string `toml:"socket"`
	SessionPrefix string `toml:"session_prefix"`
}
type rawProvider struct {
	DisplayName string             `toml:"display_name"`
	Command     string             `toml:"command"`
	Controls    rawProviderControl `toml:"controls"`
}
type rawProviderControl struct {
	Models           []string `toml:"models"`
	Efforts          []string `toml:"efforts"`
	PermissionModes  []string `toml:"permission_modes"`
	SandboxModes     []string `toml:"sandbox_modes"`
	ApprovalPolicies []string `toml:"approval_policies"`
}
type rawProfile struct {
	DisplayName    string `toml:"display_name"`
	Provider       string `toml:"provider"`
	Model          string `toml:"model"`
	Effort         string `toml:"effort"`
	PermissionMode string `toml:"permission_mode"`
	SandboxMode    string `toml:"sandbox_mode"`
	ApprovalPolicy string `toml:"approval_policy"`
}
type rawProject struct {
	DisplayName         string                    `toml:"display_name"`
	Root                string                    `toml:"root"`
	DefaultHost         string                    `toml:"default_host"`
	DefaultProfile      string                    `toml:"default_profile"`
	BaseRef             string                    `toml:"base_ref"`
	ManagedWorktreeRoot string                    `toml:"managed_worktree_root"`
	Repositories        map[string]rawProjectRepo `toml:"repositories"`
}
type rawProjectRepo struct {
	GitCommonDir     string `toml:"git_common_dir"`
	DefaultBranchRef string `toml:"default_branch_ref"`
}
type rawSafety struct {
	Policies map[string]rawSafetyPolicy `toml:"policies"`
}
type rawSafetyPolicy struct {
	BlockDirtyWorktree             bool     `toml:"block_dirty_worktree"`
	BlockUntrackedFiles            bool     `toml:"block_untracked_files"`
	BlockIgnoredUserFiles          bool     `toml:"block_ignored_user_files"`
	BlockConflicts                 bool     `toml:"block_conflicts"`
	BlockUnpushedCommits           bool     `toml:"block_unpushed_commits"`
	BlockMissingUpstreamProof      bool     `toml:"block_missing_upstream_proof"`
	BlockMissingMarker             bool     `toml:"block_missing_marker"`
	BlockPathEscape                bool     `toml:"block_path_escape"`
	BlockLiveSessionWorkspaceClose bool     `toml:"block_live_session_workspace_close"`
	DangerousClaudePermissionModes []string `toml:"dangerous_claude_permission_modes"`
	DangerousCodexSandboxModes     []string `toml:"dangerous_codex_sandbox_modes"`
	DangerousCodexFlags            []string `toml:"dangerous_codex_flags"`
	DangerRequiresExplicitRequest  bool     `toml:"danger_requires_explicit_request"`
}
type rawNotifications struct {
	Channels map[string]rawNotifChannel `toml:"channels"`
}
type rawNotifChannel struct {
	Type   string   `toml:"type"`
	Events []string `toml:"events"`
}

// parse decodes one TOML source strictly (unknown keys are errors), converts
// ingress vocabulary to owned enums, and returns a partial Config carrying only
// the sections the source actually set.
func parse(sourceKey string, data []byte) (Config, error) {
	var raw rawConfig
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return Config{}, fmt.Errorf("config source %q: %w", sourceKey, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("config source %q has unknown keys: %s", sourceKey, strings.Join(keys, ", "))
	}
	out, err := convert(sourceKey, raw)
	if err != nil {
		return Config{}, err
	}
	// Record which default-true booleans this source explicitly set so merge can
	// tell "= false" from absent. Only these keys need it; int keys use zero.
	out.declared = declaredFlags{
		securityPeerCred:     md.IsDefined("security", "socket_peer_credential_check"),
		securityTunnelToken:  md.IsDefined("security", "tcp_tunnel_requires_token"),
		securityLoopbackOnly: md.IsDefined("security", "tcp_tunnel_loopback_only"),
		suspendResumeReset:   md.IsDefined("timing", "suspend_resume_reset"),
		showClosed:           md.IsDefined("ui", "show_closed"),
	}
	return out, nil
}

func convert(sourceKey string, raw rawConfig) (Config, error) {
	paths, err := expandPaths(Paths(raw.Paths))
	if err != nil {
		return Config{}, fmt.Errorf("config source %q: %w", sourceKey, err)
	}
	out := Config{
		Version:   raw.Version,
		Paths:     paths,
		UI:        UI{Theme: raw.UI.Theme, RefreshMs: raw.UI.RefreshMs, ShowClosed: raw.UI.ShowClosed},
		Timing:    Timing(raw.Timing),
		Retention: Retention(raw.Retention),
		Security:  Security(raw.Security),
	}
	if raw.UI.Theme != "" {
		theme, err := parseTheme(raw.UI.Theme)
		if err != nil {
			return Config{}, fmt.Errorf("config source %q: %w", sourceKey, err)
		}
		out.UI.Theme = theme
	}
	out.Defaults = Defaults{
		Project: raw.Defaults.Project, Host: raw.Defaults.Host, Profile: raw.Defaults.Profile, BaseRef: raw.Defaults.BaseRef,
	}
	if raw.Defaults.WorktreeMode != "" {
		mode, err := ParseWorktreeMode(raw.Defaults.WorktreeMode)
		if err != nil {
			return Config{}, fmt.Errorf("config source %q: %w", sourceKey, err)
		}
		out.Defaults.WorktreeMode = mode
	}
	if len(raw.Hosts) > 0 {
		out.Hosts = map[string]Host{}
		for key, host := range raw.Hosts {
			mode, err := ParseHostAccessMode(host.Access.Mode)
			if err != nil {
				return Config{}, fmt.Errorf("config source %q host %q: %w", sourceKey, key, err)
			}
			out.Hosts[key] = Host{
				DisplayName: host.DisplayName,
				Access:      HostAccess{Mode: mode, HostAlias: host.Access.HostAlias, SocketForwarding: host.Access.SocketForwarding},
				TmuxServer:  host.TmuxServer,
			}
		}
	}
	if len(raw.Tmux.Servers) > 0 {
		out.TmuxServers = map[string]TmuxServer{}
		for key, srv := range raw.Tmux.Servers {
			out.TmuxServers[key] = TmuxServer(srv)
		}
	}
	if len(raw.Providers) > 0 {
		out.Providers = map[string]Provider{}
		for key, prov := range raw.Providers {
			out.Providers[key] = Provider{
				DisplayName: prov.DisplayName, Command: prov.Command,
				Models: prov.Controls.Models, Efforts: prov.Controls.Efforts,
				PermissionModes: prov.Controls.PermissionModes, SandboxModes: prov.Controls.SandboxModes,
				ApprovalPolicies: prov.Controls.ApprovalPolicies,
			}
		}
	}
	if len(raw.Profiles) > 0 {
		out.Profiles = map[string]Profile{}
		for key, p := range raw.Profiles {
			out.Profiles[key] = Profile(p)
		}
	}
	if len(raw.Projects) > 0 {
		out.Projects = map[string]Project{}
		for key, p := range raw.Projects {
			project := Project{
				DisplayName: p.DisplayName, Root: p.Root, DefaultHost: p.DefaultHost,
				DefaultProfile: p.DefaultProfile, BaseRef: p.BaseRef, ManagedWorktreeRoot: p.ManagedWorktreeRoot,
			}
			if repo, ok := p.Repositories["primary"]; ok {
				project.GitCommonDir = repo.GitCommonDir
				project.DefaultBranchRef = repo.DefaultBranchRef
			}
			out.Projects[key] = project
		}
	}
	if policy, ok := raw.Safety.Policies["strict"]; ok {
		out.SafetyPolicy = SafetyPolicy{
			BlockDirtyWorktree: policy.BlockDirtyWorktree, BlockUntrackedFiles: policy.BlockUntrackedFiles,
			BlockIgnoredUserFiles: policy.BlockIgnoredUserFiles, BlockConflicts: policy.BlockConflicts,
			BlockUnpushedCommits: policy.BlockUnpushedCommits, BlockMissingUpstreamProof: policy.BlockMissingUpstreamProof,
			DangerousClaudePermissionModes: policy.DangerousClaudePermissionModes,
			DangerousCodexSandboxModes:     policy.DangerousCodexSandboxModes,
			DangerousCodexFlags:            policy.DangerousCodexFlags,
			DangerRequiresExplicitRequest:  policy.DangerRequiresExplicitRequest,
		}
	}
	if len(raw.Notif.Channels) > 0 {
		out.Notifications = map[string]Notification{}
		for key, ch := range raw.Notif.Channels {
			chType, err := ParseNotificationType(ch.Type)
			if err != nil {
				return Config{}, fmt.Errorf("config source %q notification %q: %w", sourceKey, key, err)
			}
			out.Notifications[key] = Notification{Type: chType, Events: ch.Events}
		}
	}
	return out, nil
}

// expandPaths resolves the [paths] section to absolute, tilde- and
// environment-expanded directories so downstream services never handle a
// relative or ~-prefixed path. Empty fields stay empty (defaults apply later).
func expandPaths(p Paths) (Paths, error) {
	var err error
	if p.StateDir, err = expandPath(p.StateDir); err != nil {
		return Paths{}, err
	}
	if p.RuntimeDir, err = expandPath(p.RuntimeDir); err != nil {
		return Paths{}, err
	}
	if p.LogDir, err = expandPath(p.LogDir); err != nil {
		return Paths{}, err
	}
	return p, nil
}

// expandPath expands a leading ~ to the user home, then $VAR/${VAR} references,
// then makes the result absolute. An empty path is returned unchanged.
func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	path = os.ExpandEnv(path)
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	return path, nil
}

// merge overlays src onto base; set (non-zero) scalar fields and present map
// entries in src win. This realizes higher-priority-wins per key.
func merge(base, src Config) Config {
	out := base
	if src.Version != 0 {
		out.Version = src.Version
	}
	out.Paths = mergePaths(base.Paths, src.Paths)
	out.Defaults = mergeDefaults(base.Defaults, src.Defaults)
	out.UI = mergeUI(base.UI, src.UI, src.declared)
	out.Timing = mergeTiming(base.Timing, src.Timing, src.declared)
	out.Retention = mergeRetention(base.Retention, src.Retention)
	out.Security = mergeSecurity(base.Security, src.Security, src.declared)
	out.declared = base.declared.or(src.declared)
	out.Hosts = mergeHostMap(base.Hosts, src.Hosts)
	out.TmuxServers = mergeTmuxMap(base.TmuxServers, src.TmuxServers)
	out.Providers = mergeProviderMap(base.Providers, src.Providers)
	out.Profiles = mergeProfileMap(base.Profiles, src.Profiles)
	out.Projects = mergeProjectMap(base.Projects, src.Projects)
	if safetyPolicyDeclared(src.SafetyPolicy) {
		out.SafetyPolicy = src.SafetyPolicy
	}
	out.Notifications = mergeNotifMap(base.Notifications, src.Notifications)
	return out
}

func mergePaths(base, src Paths) Paths {
	if src.StateDir != "" {
		base.StateDir = src.StateDir
	}
	if src.RuntimeDir != "" {
		base.RuntimeDir = src.RuntimeDir
	}
	if src.LogDir != "" {
		base.LogDir = src.LogDir
	}
	return base
}

func mergeDefaults(base, src Defaults) Defaults {
	if src.Project != "" {
		base.Project = src.Project
	}
	if src.Host != "" {
		base.Host = src.Host
	}
	if src.Profile != "" {
		base.Profile = src.Profile
	}
	if src.WorktreeMode != "" {
		base.WorktreeMode = src.WorktreeMode
	}
	if src.BaseRef != "" {
		base.BaseRef = src.BaseRef
	}
	return base
}

func mergeUI(base, src UI, decl declaredFlags) UI {
	if src.Theme != "" {
		base.Theme = src.Theme
	}
	if src.RefreshMs != 0 {
		base.RefreshMs = src.RefreshMs
	}
	if decl.showClosed {
		base.ShowClosed = src.ShowClosed
	}
	return base
}

func mergeTiming(base, src Timing, decl declaredFlags) Timing {
	if src.HeartbeatIntervalMs != 0 {
		base.HeartbeatIntervalMs = src.HeartbeatIntervalMs
	}
	if src.HeartbeatTTLMs != 0 {
		base.HeartbeatTTLMs = src.HeartbeatTTLMs
	}
	if src.QuietThresholdMs != 0 {
		base.QuietThresholdMs = src.QuietThresholdMs
	}
	if src.GracefulStopTimeoutMs != 0 {
		base.GracefulStopTimeoutMs = src.GracefulStopTimeoutMs
	}
	if src.ReconcileIntervalMs != 0 {
		base.ReconcileIntervalMs = src.ReconcileIntervalMs
	}
	if decl.suspendResumeReset {
		base.SuspendResumeReset = src.SuspendResumeReset
	}
	return base
}

func mergeRetention(base, src Retention) Retention {
	if src.OutputRetentionDays != 0 {
		base.OutputRetentionDays = src.OutputRetentionDays
	}
	if src.EventRetentionDays != 0 {
		base.EventRetentionDays = src.EventRetentionDays
	}
	if src.StatusSnapshotWindow != 0 {
		base.StatusSnapshotWindow = src.StatusSnapshotWindow
	}
	if src.IdempotencyRetentionDays != 0 {
		base.IdempotencyRetentionDays = src.IdempotencyRetentionDays
	}
	if src.BusyTimeoutMs != 0 {
		base.BusyTimeoutMs = src.BusyTimeoutMs
	}
	if src.JournalSizeLimitBytes != 0 {
		base.JournalSizeLimitBytes = src.JournalSizeLimitBytes
	}
	return base
}

// mergeSecurity overlays only the security toggles the source declared. Each
// toggle defaults true, so a source that omits [security] (decoding to false)
// must not silently disable peer-credential checks or tunnel authentication;
// merge honors an explicit `= false` but never a merely-absent key.
func mergeSecurity(base, src Security, decl declaredFlags) Security {
	if decl.securityPeerCred {
		base.SocketPeerCredentialCheck = src.SocketPeerCredentialCheck
	}
	if decl.securityTunnelToken {
		base.TCPTunnelRequiresToken = src.TCPTunnelRequiresToken
	}
	if decl.securityLoopbackOnly {
		base.TCPTunnelLoopbackOnly = src.TCPTunnelLoopbackOnly
	}
	return base
}

// safetyPolicyDeclared reports whether a parsed source actually declared a
// [safety.policies.strict] block (any field set), so an absent block does not
// overwrite the baseline policy during merge.
func safetyPolicyDeclared(p SafetyPolicy) bool {
	return p.BlockDirtyWorktree || p.BlockUntrackedFiles || p.BlockIgnoredUserFiles ||
		p.BlockConflicts || p.BlockUnpushedCommits || p.BlockMissingUpstreamProof ||
		p.DangerRequiresExplicitRequest ||
		len(p.DangerousClaudePermissionModes) > 0 || len(p.DangerousCodexSandboxModes) > 0 ||
		len(p.DangerousCodexFlags) > 0
}

func mergeHostMap(base, src map[string]Host) map[string]Host {
	if len(src) == 0 {
		return base
	}
	out := map[string]Host{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeTmuxMap(base, src map[string]TmuxServer) map[string]TmuxServer {
	if len(src) == 0 {
		return base
	}
	out := map[string]TmuxServer{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeProviderMap(base, src map[string]Provider) map[string]Provider {
	if len(src) == 0 {
		return base
	}
	out := map[string]Provider{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeProfileMap(base, src map[string]Profile) map[string]Profile {
	if len(src) == 0 {
		return base
	}
	out := map[string]Profile{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeProjectMap(base, src map[string]Project) map[string]Project {
	if len(src) == 0 {
		return base
	}
	out := map[string]Project{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeNotifMap(base, src map[string]Notification) map[string]Notification {
	if len(src) == 0 {
		return base
	}
	out := map[string]Notification{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func validate(cfg Config) error {
	if cfg.Version != 1 {
		return fmt.Errorf("config version %d is not supported (want 1)", cfg.Version)
	}
	if cfg.Timing.HeartbeatTTLMs <= cfg.Timing.HeartbeatIntervalMs {
		return fmt.Errorf("timing.heartbeat_ttl_ms (%d) must exceed heartbeat_interval_ms (%d)", cfg.Timing.HeartbeatTTLMs, cfg.Timing.HeartbeatIntervalMs)
	}
	for _, name := range []string{"heartbeat_interval_ms", "heartbeat_ttl_ms", "quiet_threshold_ms", "graceful_stop_timeout_ms", "reconcile_interval_ms"} {
		if timingValue(cfg.Timing, name) <= 0 {
			return fmt.Errorf("timing.%s must be positive", name)
		}
	}
	for key, profile := range cfg.Profiles {
		if profile.Provider == "" {
			return fmt.Errorf("profile %q has no provider", key)
		}
		if _, ok := cfg.Providers[profile.Provider]; !ok && len(cfg.Providers) > 0 {
			return fmt.Errorf("profile %q references unknown provider %q", key, profile.Provider)
		}
	}
	for key, host := range cfg.Hosts {
		if host.Access.Mode == "" {
			return fmt.Errorf("host %q has no access mode", key)
		}
	}
	return nil
}

func timingValue(t Timing, name string) int {
	switch name {
	case "heartbeat_interval_ms":
		return t.HeartbeatIntervalMs
	case "heartbeat_ttl_ms":
		return t.HeartbeatTTLMs
	case "quiet_threshold_ms":
		return t.QuietThresholdMs
	case "graceful_stop_timeout_ms":
		return t.GracefulStopTimeoutMs
	case "reconcile_interval_ms":
		return t.ReconcileIntervalMs
	default:
		return 0
	}
}

// ParseWorktreeMode converts the ingress worktree mode to the owned enum.
func ParseWorktreeMode(value string) (string, error) {
	switch value {
	case "prompt":
		return "Prompt", nil
	case "always":
		return "Always", nil
	case "never":
		return "Never", nil
	default:
		return "", fmt.Errorf("unknown worktree_mode %q (want prompt, always, or never)", value)
	}
}

// ParseHostAccessMode converts the ingress host access mode to the owned enum.
func ParseHostAccessMode(value string) (string, error) {
	switch value {
	case "local":
		return "Local", nil
	case "ssh":
		return "Ssh", nil
	case "attach_only_mosh":
		return "AttachOnlyMosh", nil
	default:
		return "", fmt.Errorf("unknown host access mode %q (want local, ssh, or attach_only_mosh)", value)
	}
}

// ParseNotificationType converts the ingress channel type to the owned enum.
func ParseNotificationType(value string) (string, error) {
	switch value {
	case "terminal":
		return "Terminal", nil
	case "desktop":
		return "Desktop", nil
	default:
		return "", fmt.Errorf("unknown notification channel type %q (want terminal or desktop)", value)
	}
}

func parseTheme(value string) (string, error) {
	switch value {
	case "system":
		return "System", nil
	case "light":
		return "Light", nil
	case "dark":
		return "Dark", nil
	default:
		return "", fmt.Errorf("unknown ui.theme %q (want system, light, or dark)", value)
	}
}
