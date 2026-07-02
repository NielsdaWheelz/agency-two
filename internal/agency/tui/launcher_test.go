package tui

import (
	"bytes"
	"strings"
	"testing"

	"agency-two/internal/agency/content"
)

func TestRenderLauncherShowsPreviewAndControls(t *testing.T) {
	var buf bytes.Buffer
	RenderLauncher(&buf, LauncherView{
		Provider:       "codex",
		Model:          "gpt-5.5",
		Effort:         "high",
		WorkspaceMode:  "managed worktree from origin/main",
		SandboxMode:    "workspace-write",
		ApprovalPolicy: "on-request",
		CommandPreview: `codex --model gpt-5.5 -c model_reasoning_effort=high --sandbox workspace-write --ask-for-approval on-request`,
	})
	out := buf.String()
	for _, want := range []string{"Agency launcher", "provider: codex", "model: gpt-5.5", "Command preview", "argv: codex --model gpt-5.5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("launcher output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Launch disabled") {
		t.Fatalf("valid launcher should not be disabled:\n%s", out)
	}
}

func TestRenderLauncherBlocksOnValidation(t *testing.T) {
	var buf bytes.Buffer
	RenderLauncher(&buf, LauncherView{
		Provider:       "codex",
		Model:          "gpt-5.4-mini",
		Effort:         "max",
		CommandPreview: "codex --model gpt-5.4-mini",
		Validation:     []string{content.LaunchBlockedUnsupportedEffort("codex", "max")},
	})
	out := buf.String()
	if !strings.Contains(out, "Cannot launch. Provider codex does not support effort max.") {
		t.Fatalf("missing validation message:\n%s", out)
	}
	if !strings.Contains(out, "Launch disabled.") {
		t.Fatalf("blocked launcher must show Launch disabled:\n%s", out)
	}
}
