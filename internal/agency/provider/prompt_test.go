package provider

import (
	"strings"
	"testing"
)

func TestDetectPromptStateReadsBottomRegion(t *testing.T) {
	rendered := "some earlier output\nmore output\n\nWaiting for approval before running command\n"
	hint := DetectPromptState(KeyCodex, rendered)
	if hint.Status != "NeedsApproval" {
		t.Fatalf("hint = %+v, want NeedsApproval", hint)
	}
}

func TestDetectPromptStateInput(t *testing.T) {
	rendered := "assistant produced a plan\nType your message to continue\n"
	hint := DetectPromptState(KeyClaude, rendered)
	if hint.Status != "NeedsInput" {
		t.Fatalf("hint = %+v, want NeedsInput", hint)
	}
}

func TestDetectPromptStateBlankPaneIsUncertain(t *testing.T) {
	// A blank/unavailable pane yields no bottom region: uncertain, so a prior
	// prompt pin is neither asserted nor cleared.
	for _, rendered := range []string{"", "   \n\t\n  \n"} {
		hint := DetectPromptState(KeyCodex, rendered)
		if hint.Status != "" {
			t.Fatalf("rendered %q: hint = %+v, want empty (uncertain)", rendered, hint)
		}
	}
}

func TestDetectPromptStateReadablePaneWithoutPromptIsNone(t *testing.T) {
	// A readable pane showing the agent working is positive evidence of
	// not-prompting (PromptStatusNone), the signal the supervisor uses to clear
	// a stale NeedsInput/NeedsApproval pin — not uncertainty.
	rendered := "running tests...\nall green\ncompiling\n"
	hint := DetectPromptState(KeyCodex, rendered)
	if hint.Status != PromptStatusNone {
		t.Fatalf("hint = %+v, want None", hint)
	}
}

func TestDetectPromptStateDoesNotMatchScrollbackOnly(t *testing.T) {
	// An approval phrase far up in scrollback, with plenty of later output, is
	// outside the bottom region and must not fabricate a current prompt. The
	// readable working pane is None, never a prompt state.
	rendered := "approval required earlier\n"
	for i := 0; i < 40; i++ {
		rendered += "log line after the old prompt\n"
	}
	hint := DetectPromptState(KeyCodex, rendered)
	if hint.Status == "NeedsApproval" || hint.Status == "NeedsInput" {
		t.Fatalf("hint = %+v, want non-prompt (old prompt is scrollback)", hint)
	}
	if hint.Status != PromptStatusNone {
		t.Fatalf("hint = %+v, want None for a readable non-prompting pane", hint)
	}
}

func TestFingerprintKnown(t *testing.T) {
	if !FingerprintKnown(KeyClaude, "Claude Code — esc to interrupt") {
		t.Fatal("expected known claude fingerprint")
	}
	if FingerprintKnown(KeyClaude, "") {
		t.Fatal("empty snapshot must not be a known fingerprint")
	}
}

func TestBuildClaudeEmitsSessionAndSettingsFlags(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{
		ProviderKey: KeyClaude, Model: "sonnet", Effort: "high", PermissionMode: "default",
		Settings: "/tmp/s.json", MCPConfig: "/tmp/mcp.json", SessionID: "sess-1", Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Argv, " ")
	for _, want := range []string{"--settings /tmp/s.json", "--mcp-config /tmp/mcp.json", "--session-id sess-1", "--resume sess-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("claude argv %q missing %q", joined, want)
		}
	}
}

func TestBuildCodexEmitsSearchAndProfile(t *testing.T) {
	plan, err := BuildLaunchPlan(LaunchInput{
		ProviderKey: KeyCodex, Model: "gpt-5.5", Effort: "high", SandboxMode: "workspace-write", ApprovalPolicy: "on-request",
		CodexProfile: "work", Search: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Argv, " ")
	if !strings.Contains(joined, "--profile work") || !strings.Contains(joined, "--search") {
		t.Fatalf("codex argv %q missing profile/search", joined)
	}
}

func TestNewShellArgumentTextRejectsControlChars(t *testing.T) {
	if _, err := NewShellArgumentText("ok-value"); err != nil {
		t.Fatalf("valid argument rejected: %v", err)
	}
	if _, err := NewShellArgumentText("bad\nvalue"); err == nil {
		t.Fatal("newline argument accepted")
	}
	// A dynamic value with a newline must fail plan construction, not concat in.
	if _, err := BuildLaunchPlan(LaunchInput{ProviderKey: KeyClaude, Model: "sonnet\nrm -rf", Effort: "high"}); err == nil {
		t.Fatal("plan accepted a control-char model")
	}
}

func TestContinueArgvIsAdapterOwned(t *testing.T) {
	got := ContinueArgv(KeyClaude, []string{"claude", "--model", "sonnet"})
	if strings.Join(got, " ") != "claude --continue --model sonnet" {
		t.Fatalf("continue argv = %v", got)
	}
	// Idempotent + provider-scoped.
	if again := ContinueArgv(KeyClaude, got); len(again) != len(got) {
		t.Fatalf("continue argv not idempotent: %v", again)
	}
	if codex := ContinueArgv(KeyCodex, []string{"codex", "--model", "gpt-5.5"}); len(codex) != 3 {
		t.Fatalf("codex should not gain --continue: %v", codex)
	}
}
