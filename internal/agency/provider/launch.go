package provider

import (
	"fmt"
	"strings"
)

const (
	KeyClaude = "claude"
	KeyCodex  = "codex"
)

type Capabilities struct {
	ProviderKey       string
	DisplayName       string
	Command           string
	CommandCandidates []string // executables to probe for availability, in order
	Models            []string
	Efforts           []string
	PermissionModes   []string
	SandboxModes      []string
	ApprovalPolicies  []string
	ExtraDirectory    bool // supports additional working directories (--add-dir)
	InitialPrompt     bool // accepts an initial prompt argument
	SessionResume     bool // supports resuming a prior provider session
}

type LaunchInput struct {
	ProviderKey     string
	Model           string
	Effort          string
	PermissionMode  string
	SandboxMode     string
	ApprovalPolicy  string
	AddDirs         []string
	AllowDangerous  bool
	CommandOverride string
	// Provider session management (owned by the run service for additional-run
	// resume, not user CLI flags).
	SessionID string
	Resume    bool
	Continue  bool
	// Profile-configured provider controls.
	Settings     string // Claude --settings
	MCPConfig    string // Claude --mcp-config
	CodexProfile string // Codex --profile
	Search       bool   // Codex --search
}

// ShellArgumentText is an owned, validated argv token. Every dynamic launch value
// is converted into one before it enters an argv, so no argv is ever assembled
// from an unvalidated string (spec Provider Contract).
type ShellArgumentText struct{ text string }

// NewShellArgumentText validates a dynamic value into an owned argv token,
// rejecting control characters that could corrupt argv or a rendered command.
func NewShellArgumentText(value string) (ShellArgumentText, error) {
	if strings.ContainsAny(value, "\x00\n\r") {
		return ShellArgumentText{}, InputError{Code: "UnsupportedControl", Field: "argument", Value: value}
	}
	return ShellArgumentText{text: value}, nil
}

func (s ShellArgumentText) String() string { return s.text }

// argvBuilder assembles a launch argv from trusted literal flag names and
// validated dynamic ShellArgumentText values. The first error short-circuits.
type argvBuilder struct {
	tokens []string
	err    error
}

func newArgvBuilder(command string) *argvBuilder {
	b := &argvBuilder{}
	return b.arg(command)
}

// arg appends a validated dynamic value.
func (b *argvBuilder) arg(value string) *argvBuilder {
	if b.err != nil {
		return b
	}
	token, err := NewShellArgumentText(value)
	if err != nil {
		b.err = err
		return b
	}
	b.tokens = append(b.tokens, token.String())
	return b
}

// literal appends a trusted flag name (compile-time constant).
func (b *argvBuilder) literal(name string) *argvBuilder {
	if b.err != nil {
		return b
	}
	b.tokens = append(b.tokens, name)
	return b
}

// flag appends a trusted flag name and its validated dynamic value.
func (b *argvBuilder) flag(name, value string) *argvBuilder {
	return b.literal(name).arg(value)
}

func (b *argvBuilder) build() ([]string, error) {
	return b.tokens, b.err
}

// ContinueArgv returns the argv for resuming a provider session in an additional
// run. The resume mechanism is owned here (the adapter), not by storage: Claude
// resumes with --continue; other providers resume in-place with no extra flag.
func ContinueArgv(providerKey string, argv []string) []string {
	if providerKey != KeyClaude || len(argv) == 0 {
		return argv
	}
	for _, token := range argv {
		if token == "--continue" {
			return argv
		}
	}
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[0], "--continue")
	return append(out, argv[1:]...)
}

type LaunchPlan struct {
	ProviderKey string
	Argv        []string
	Preview     string
}

type InputError struct {
	Code  string
	Field string
	Value string
}

func (e InputError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Field)
	}
	return fmt.Sprintf("%s: %s=%q", e.Code, e.Field, e.Value)
}

func Catalog() []Capabilities {
	return []Capabilities{
		{
			ProviderKey:       KeyClaude,
			DisplayName:       "Claude Code",
			Command:           "claude",
			CommandCandidates: []string{"claude"},
			Models:            []string{"sonnet", "opus", "opusplan", "haiku", "fable"},
			Efforts:           []string{"low", "medium", "high", "xhigh", "max"},
			PermissionModes:   []string{"default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions"},
			ExtraDirectory:    true,
			InitialPrompt:     true,
			SessionResume:     true,
		},
		{
			ProviderKey:       KeyCodex,
			DisplayName:       "Codex",
			Command:           "codex",
			CommandCandidates: []string{"codex"},
			Models:            []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"},
			Efforts:           []string{"minimal", "low", "medium", "high", "xhigh"},
			SandboxModes:      []string{"read-only", "workspace-write", "danger-full-access"},
			ApprovalPolicies:  []string{"untrusted", "on-request", "never"},
			ExtraDirectory:    true,
			InitialPrompt:     true,
			SessionResume:     true,
		},
	}
}

func BuildLaunchPlan(input LaunchInput) (LaunchPlan, error) {
	switch input.ProviderKey {
	case KeyClaude:
		return buildClaude(input)
	case KeyCodex:
		return buildCodex(input)
	default:
		return LaunchPlan{}, InputError{Code: "ProviderNotFound", Field: "provider", Value: input.ProviderKey}
	}
}

func buildClaude(input LaunchInput) (LaunchPlan, error) {
	cap := Catalog()[0]
	model := defaultString(input.Model, "sonnet")
	effort := defaultString(input.Effort, "high")
	permissionMode := defaultString(input.PermissionMode, "default")

	if !contains(cap.Models, model) {
		return LaunchPlan{}, InputError{Code: "UnsupportedModel", Field: "model", Value: model}
	}
	if !contains(cap.Efforts, effort) {
		return LaunchPlan{}, InputError{Code: "UnsupportedEffort", Field: "effort", Value: effort}
	}
	if !contains(cap.PermissionModes, permissionMode) {
		return LaunchPlan{}, InputError{Code: "UnsupportedControl", Field: "permission_mode", Value: permissionMode}
	}
	if permissionMode == "bypassPermissions" && !input.AllowDangerous {
		return LaunchPlan{}, InputError{Code: "DangerousLaunchBlocked", Field: "permission_mode", Value: permissionMode}
	}
	if input.SandboxMode != "" {
		return LaunchPlan{}, InputError{Code: "UnsupportedControl", Field: "sandbox", Value: input.SandboxMode}
	}
	if input.ApprovalPolicy != "" {
		return LaunchPlan{}, InputError{Code: "UnsupportedControl", Field: "approval_policy", Value: input.ApprovalPolicy}
	}

	b := newArgvBuilder(command(input, cap.Command))
	b.flag("--model", model).flag("--effort", effort).flag("--permission-mode", permissionMode)
	for _, dir := range input.AddDirs {
		b.flag("--add-dir", dir)
	}
	if input.Settings != "" {
		b.flag("--settings", input.Settings)
	}
	if input.MCPConfig != "" {
		b.flag("--mcp-config", input.MCPConfig)
	}
	if input.SessionID != "" {
		b.flag("--session-id", input.SessionID)
	}
	if input.Resume && input.SessionID != "" {
		b.flag("--resume", input.SessionID)
	}
	if input.Continue {
		b.literal("--continue")
	}
	argv, err := b.build()
	if err != nil {
		return LaunchPlan{}, err
	}
	return plan(KeyClaude, argv), nil
}

func buildCodex(input LaunchInput) (LaunchPlan, error) {
	cap := Catalog()[1]
	model := defaultString(input.Model, "gpt-5.5")
	effort := defaultString(input.Effort, "high")
	sandboxMode := defaultString(input.SandboxMode, "workspace-write")
	approvalPolicy := defaultString(input.ApprovalPolicy, "on-request")

	if !contains(cap.Models, model) {
		return LaunchPlan{}, InputError{Code: "UnsupportedModel", Field: "model", Value: model}
	}
	if !contains(cap.Efforts, effort) {
		return LaunchPlan{}, InputError{Code: "UnsupportedEffort", Field: "effort", Value: effort}
	}
	if !contains(cap.SandboxModes, sandboxMode) {
		return LaunchPlan{}, InputError{Code: "UnsupportedControl", Field: "sandbox", Value: sandboxMode}
	}
	if !contains(cap.ApprovalPolicies, approvalPolicy) {
		return LaunchPlan{}, InputError{Code: "UnsupportedControl", Field: "approval_policy", Value: approvalPolicy}
	}
	if codexDangerous(sandboxMode, approvalPolicy) && !input.AllowDangerous {
		return LaunchPlan{}, InputError{Code: "DangerousLaunchBlocked", Field: "sandbox", Value: sandboxMode}
	}
	if input.PermissionMode != "" {
		return LaunchPlan{}, InputError{Code: "UnsupportedControl", Field: "permission_mode", Value: input.PermissionMode}
	}

	b := newArgvBuilder(command(input, cap.Command))
	b.flag("--model", model).flag("-c", "model_reasoning_effort="+effort).flag("--sandbox", sandboxMode).flag("--ask-for-approval", approvalPolicy)
	for _, dir := range input.AddDirs {
		b.flag("--add-dir", dir)
	}
	if input.CodexProfile != "" {
		b.flag("--profile", input.CodexProfile)
	}
	if input.Search {
		b.literal("--search")
	}
	argv, err := b.build()
	if err != nil {
		return LaunchPlan{}, err
	}
	return plan(KeyCodex, argv), nil
}

func codexDangerous(sandboxMode, approvalPolicy string) bool {
	return sandboxMode == "danger-full-access" || sandboxMode == "" && approvalPolicy == "never"
}

func plan(providerKey string, argv []string) LaunchPlan {
	return LaunchPlan{
		ProviderKey: providerKey,
		Argv:        argv,
		Preview:     ShellPreview(argv),
	}
}

func command(input LaunchInput, defaultCommand string) string {
	if input.CommandOverride != "" {
		return input.CommandOverride
	}
	return defaultCommand
}

func defaultString(value, defaultValue string) string {
	if value != "" {
		return value
	}
	return defaultValue
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func ShellPreview(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, token := range argv {
		quoted = append(quoted, shellQuote(token))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(token string) string {
	if token != "" && strings.IndexFunc(token, needsShellQuote) == -1 {
		return token
	}
	return "'" + strings.ReplaceAll(token, "'", "'\\''") + "'"
}

func needsShellQuote(r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return false
	}
	switch r {
	case '@', '%', '_', '+', '=', ':', ',', '.', '/', '-':
		return false
	default:
		return true
	}
}
