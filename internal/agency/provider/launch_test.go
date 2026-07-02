package provider

import (
	"errors"
	"reflect"
	"testing"
)

func TestBuildClaudeLaunchPlanUsesSchemaDefaults(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{ProviderKey: KeyClaude})
	if err != nil {
		t.Fatalf("BuildLaunchPlan returned error: %v", err)
	}
	want := []string{"claude", "--model", "sonnet", "--effort", "high", "--permission-mode", "default"}
	if !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv mismatch\nwant: %#v\n got: %#v", want, plan.Argv)
	}
	if plan.Preview != "claude --model sonnet --effort high --permission-mode default" {
		t.Fatalf("preview mismatch: %q", plan.Preview)
	}
}

func TestBuildClaudeLaunchPlanUsesExactTokens(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{
		ProviderKey:    KeyClaude,
		Model:          "opus",
		Effort:         "xhigh",
		PermissionMode: "acceptEdits",
		AddDirs:        []string{"/tmp/with space", "/tmp/plain"},
	})
	if err != nil {
		t.Fatalf("BuildLaunchPlan returned error: %v", err)
	}
	want := []string{
		"claude",
		"--model", "opus",
		"--effort", "xhigh",
		"--permission-mode", "acceptEdits",
		"--add-dir", "/tmp/with space",
		"--add-dir", "/tmp/plain",
	}
	if !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv mismatch\nwant: %#v\n got: %#v", want, plan.Argv)
	}
	if plan.Preview != "claude --model opus --effort xhigh --permission-mode acceptEdits --add-dir '/tmp/with space' --add-dir /tmp/plain" {
		t.Fatalf("preview mismatch: %q", plan.Preview)
	}
}

func TestBuildCodexLaunchPlanUsesSchemaDefaults(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{ProviderKey: KeyCodex})
	if err != nil {
		t.Fatalf("BuildLaunchPlan returned error: %v", err)
	}
	want := []string{"codex", "--model", "gpt-5.5", "-c", "model_reasoning_effort=high", "--sandbox", "workspace-write", "--ask-for-approval", "on-request"}
	if !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv mismatch\nwant: %#v\n got: %#v", want, plan.Argv)
	}
}

func TestBuildCodexLaunchPlanUsesExactTokens(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{
		ProviderKey:    KeyCodex,
		Model:          "gpt-5.4-mini",
		Effort:         "minimal",
		SandboxMode:    "read-only",
		ApprovalPolicy: "never",
		AddDirs:        []string{"/repo/extra"},
	})
	if err != nil {
		t.Fatalf("BuildLaunchPlan returned error: %v", err)
	}
	want := []string{
		"codex",
		"--model", "gpt-5.4-mini",
		"-c", "model_reasoning_effort=minimal",
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
		"--add-dir", "/repo/extra",
	}
	if !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv mismatch\nwant: %#v\n got: %#v", want, plan.Argv)
	}
}

func TestUnsupportedControlsReturnTypedInputErrors(t *testing.T) {
	cases := []struct {
		name  string
		input LaunchInput
		code  string
		field string
	}{
		{"provider", LaunchInput{ProviderKey: "other"}, "ProviderNotFound", "provider"},
		{"claude model", LaunchInput{ProviderKey: KeyClaude, Model: "unknown"}, "UnsupportedModel", "model"},
		{"claude effort", LaunchInput{ProviderKey: KeyClaude, Effort: "minimal"}, "UnsupportedEffort", "effort"},
		{"claude permission", LaunchInput{ProviderKey: KeyClaude, PermissionMode: "ask"}, "UnsupportedControl", "permission_mode"},
		{"claude sandbox", LaunchInput{ProviderKey: KeyClaude, SandboxMode: "workspace-write"}, "UnsupportedControl", "sandbox"},
		{"codex model", LaunchInput{ProviderKey: KeyCodex, Model: "sonnet"}, "UnsupportedModel", "model"},
		{"codex effort", LaunchInput{ProviderKey: KeyCodex, Effort: "max"}, "UnsupportedEffort", "effort"},
		{"codex sandbox", LaunchInput{ProviderKey: KeyCodex, SandboxMode: "none"}, "UnsupportedControl", "sandbox"},
		{"codex approval", LaunchInput{ProviderKey: KeyCodex, ApprovalPolicy: "always"}, "UnsupportedControl", "approval_policy"},
		{"codex permission", LaunchInput{ProviderKey: KeyCodex, PermissionMode: "default"}, "UnsupportedControl", "permission_mode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildLaunchPlan(tc.input)
			var inputErr InputError
			if !errors.As(err, &inputErr) {
				t.Fatalf("error type mismatch: %T %v", err, err)
			}
			if inputErr.Code != tc.code || inputErr.Field != tc.field {
				t.Fatalf("error mismatch: got %s/%s, want %s/%s", inputErr.Code, inputErr.Field, tc.code, tc.field)
			}
		})
	}
}

func TestDangerousLaunchModesRequireExplicitAllow(t *testing.T) {
	cases := []LaunchInput{
		{ProviderKey: KeyClaude, PermissionMode: "bypassPermissions"},
		{ProviderKey: KeyCodex, SandboxMode: "danger-full-access"},
	}
	for _, input := range cases {
		_, err := BuildLaunchPlan(input)
		var inputErr InputError
		if !errors.As(err, &inputErr) {
			t.Fatalf("error type mismatch: %T %v", err, err)
		}
		if inputErr.Code != "DangerousLaunchBlocked" {
			t.Fatalf("error code mismatch: %s", inputErr.Code)
		}
	}
}

func TestDangerousLaunchModesAreAllowedWhenExplicit(t *testing.T) {
	claude, err := BuildLaunchPlan(LaunchInput{
		ProviderKey:    KeyClaude,
		PermissionMode: "bypassPermissions",
		AllowDangerous: true,
	})
	if err != nil {
		t.Fatalf("claude BuildLaunchPlan returned error: %v", err)
	}
	wantClaude := []string{"claude", "--model", "sonnet", "--effort", "high", "--permission-mode", "bypassPermissions"}
	if !reflect.DeepEqual(claude.Argv, wantClaude) {
		t.Fatalf("claude argv mismatch\nwant: %#v\n got: %#v", wantClaude, claude.Argv)
	}

	codex, err := BuildLaunchPlan(LaunchInput{
		ProviderKey:    KeyCodex,
		SandboxMode:    "danger-full-access",
		ApprovalPolicy: "never",
		AllowDangerous: true,
	})
	if err != nil {
		t.Fatalf("codex BuildLaunchPlan returned error: %v", err)
	}
	wantCodex := []string{"codex", "--model", "gpt-5.5", "-c", "model_reasoning_effort=high", "--sandbox", "danger-full-access", "--ask-for-approval", "never"}
	if !reflect.DeepEqual(codex.Argv, wantCodex) {
		t.Fatalf("codex argv mismatch\nwant: %#v\n got: %#v", wantCodex, codex.Argv)
	}
}

func TestShellPreviewQuotesOnlyDisplay(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{
		ProviderKey:     KeyCodex,
		CommandOverride: "/opt/Codex CLI/codex",
		AddDirs:         []string{"/tmp/space dir", "/tmp/it's-here"},
	})
	if err != nil {
		t.Fatalf("BuildLaunchPlan returned error: %v", err)
	}
	if plan.Argv[0] != "/opt/Codex CLI/codex" || plan.Argv[len(plan.Argv)-1] != "/tmp/it's-here" {
		t.Fatalf("argv should keep raw tokens: %#v", plan.Argv)
	}
	want := "'/opt/Codex CLI/codex' --model gpt-5.5 -c model_reasoning_effort=high --sandbox workspace-write --ask-for-approval on-request --add-dir '/tmp/space dir' --add-dir '/tmp/it'\\''s-here'"
	if plan.Preview != want {
		t.Fatalf("preview mismatch\nwant: %q\n got: %q", want, plan.Preview)
	}
}
