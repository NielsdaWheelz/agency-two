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
	ProviderKey      string
	DisplayName      string
	Command          string
	Models           []string
	Efforts          []string
	PermissionModes  []string
	SandboxModes     []string
	ApprovalPolicies []string
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
			ProviderKey:     KeyClaude,
			DisplayName:     "Claude Code",
			Command:         "claude",
			Models:          []string{"sonnet", "opus", "opusplan", "haiku", "fable"},
			Efforts:         []string{"low", "medium", "high", "xhigh", "max"},
			PermissionModes: []string{"default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions"},
		},
		{
			ProviderKey:      KeyCodex,
			DisplayName:      "Codex",
			Command:          "codex",
			Models:           []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"},
			Efforts:          []string{"minimal", "low", "medium", "high", "xhigh"},
			SandboxModes:     []string{"read-only", "workspace-write", "danger-full-access"},
			ApprovalPolicies: []string{"untrusted", "on-request", "never"},
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
		return LaunchPlan{}, InputError{Code: "UnsupportedPermissionMode", Field: "permission_mode", Value: permissionMode}
	}
	if permissionMode == "bypassPermissions" && !input.AllowDangerous {
		return LaunchPlan{}, InputError{Code: "DangerousLaunchBlocked", Field: "permission_mode", Value: permissionMode}
	}
	if input.SandboxMode != "" {
		return LaunchPlan{}, InputError{Code: "UnsupportedProviderControl", Field: "sandbox", Value: input.SandboxMode}
	}
	if input.ApprovalPolicy != "" {
		return LaunchPlan{}, InputError{Code: "UnsupportedProviderControl", Field: "approval_policy", Value: input.ApprovalPolicy}
	}

	argv := []string{
		command(input, cap.Command),
		"--model", model,
		"--effort", effort,
		"--permission-mode", permissionMode,
	}
	for _, dir := range input.AddDirs {
		argv = append(argv, "--add-dir", dir)
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
		return LaunchPlan{}, InputError{Code: "UnsupportedSandboxMode", Field: "sandbox", Value: sandboxMode}
	}
	if !contains(cap.ApprovalPolicies, approvalPolicy) {
		return LaunchPlan{}, InputError{Code: "UnsupportedApprovalPolicy", Field: "approval_policy", Value: approvalPolicy}
	}
	if codexDangerous(sandboxMode, approvalPolicy) && !input.AllowDangerous {
		return LaunchPlan{}, InputError{Code: "DangerousLaunchBlocked", Field: "sandbox", Value: sandboxMode}
	}
	if input.PermissionMode != "" {
		return LaunchPlan{}, InputError{Code: "UnsupportedProviderControl", Field: "permission_mode", Value: input.PermissionMode}
	}

	argv := []string{
		command(input, cap.Command),
		"--model", model,
		"-c", "model_reasoning_effort=" + effort,
		"--sandbox", sandboxMode,
		"--ask-for-approval", approvalPolicy,
	}
	for _, dir := range input.AddDirs {
		argv = append(argv, "--add-dir", dir)
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
