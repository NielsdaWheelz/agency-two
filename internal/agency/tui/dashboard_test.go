package tui

import (
	"bytes"
	"strings"
	"testing"

	"agency-two/internal/agency/storage"
	"agency-two/internal/agency/supervisor"
)

func TestRenderDashboardGroupsAndLabelsSessions(t *testing.T) {
	sessions := []storage.SessionSummary{
		{
			Session: "ses_stop", Provider: "codex", Title: "Stopped run", Project: "agency-two", WorkspaceKey: "project_root",
			RunStatus: "Stopped", Git: map[string]any{"summary": "Clean"}, Model: "gpt-5.4", Effort: "high",
			Close: map[string]any{"summary": "Closable"}, LastEvent: "2026-07-01T10:00:00.000Z",
		},
		{
			Session: "ses_live", Provider: "claude", Title: "Live run", Project: "agency-two", WorkspaceKey: "reader-race",
			RunStatus: "Live", Git: map[string]any{"summary": "Dirty"}, Model: "sonnet", Effort: "high",
			Close: map[string]any{"summary": "StopRequired"}, LastEvent: "2026-07-01T11:00:00.000Z",
		},
		{
			Session: "ses_need", Provider: "codex", Title: "Needs input", Project: "agency-two", WorkspaceKey: "auth-fix",
			RunStatus: "NeedsInput", Git: map[string]any{"summary": "Clean"}, Model: "gpt-5.5", Effort: "medium",
			Close: map[string]any{"summary": "StopRequired"}, LastEvent: "2026-07-01T12:00:00.000Z",
		},
	}
	var out bytes.Buffer
	RenderDashboard(&out, Dashboard{Host: "local", Sessions: sessions})
	got := out.String()
	for _, want := range []string{
		"Agency  project: agency-two  host: local  sessions: 1 live, 1 needs input",
		"SESSION",
		"Needs input",
		"Live",
		"Stopped",
		"Dirty",
		"Stop required",
		"project root",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "project_root") {
		t.Fatalf("dashboard leaked workspace key:\n%s", got)
	}
	if strings.Index(got, "ses_need") > strings.Index(got, "ses_live") || strings.Index(got, "ses_live") > strings.Index(got, "ses_stop") {
		t.Fatalf("dashboard order:\n%s", got)
	}
}

func TestRenderDashboardEmptyStateMatchesContentContract(t *testing.T) {
	var out bytes.Buffer
	RenderDashboard(&out, Dashboard{Project: "agency-two", Host: "local"})
	got := out.String()
	for _, want := range []string{
		"No agent sessions in this project.",
		"Start one with agency new codex or agency new claude.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("empty dashboard missing %q:\n%s", want, got)
		}
	}
}

func TestRenderDashboardDoesNotDefaultMissingSafetyDataToSafe(t *testing.T) {
	var out bytes.Buffer
	RenderDashboard(&out, Dashboard{Sessions: []storage.SessionSummary{{
		Session: "ses_bad", Provider: "codex", Title: "Bad payload", Project: "agency-two", WorkspaceKey: "project_root",
		RunStatus: "Live", Model: "gpt-5.4", Effort: "high", LastEvent: "2026-07-01T10:00:00.000Z",
	}}})
	got := out.String()
	for _, want := range []string{"Not a worktree", "Repair required"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Clean") || strings.Contains(got, "Closable") {
		t.Fatalf("dashboard rendered missing safety data as safe:\n%s", got)
	}
}

func TestRenderDashboardMapsEveryRunStatus(t *testing.T) {
	statuses := []string{
		"NeedsInput", "NeedsApproval", "Starting", "Live", "Quiet",
		"Exited", "Stopped", "Killed", "Failed",
		"RepairRequired", "LostTmuxTarget", "LostRunner", "Closed",
	}
	sessions := make([]storage.SessionSummary, 0, len(statuses))
	for _, status := range statuses {
		sessions = append(sessions, storage.SessionSummary{
			Session: "ses_" + strings.ToLower(status), Provider: "codex", Title: status, Project: "agency-two", WorkspaceKey: "project_root",
			RunStatus: status, Git: map[string]any{"summary": "Clean"}, Model: "gpt-5.4", Effort: "high",
			Close: map[string]any{"summary": "Closable"}, LastEvent: "2026-07-01T10:00:00.000Z",
		})
	}
	var out bytes.Buffer
	RenderDashboard(&out, Dashboard{Host: "local", Sessions: sessions})
	got := out.String()
	for _, want := range []string{
		"Needs input", "Needs approval", "Starting", "Live", "Quiet",
		"Exited", "Stopped", "Killed", "Failed",
		"Repair required", "Lost tmux target", "Lost runner",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dashboard missing status label %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ses_closed") {
		t.Fatalf("default dashboard rendered closed session:\n%s", got)
	}

	out.Reset()
	RenderDashboard(&out, Dashboard{Host: "local", ShowClosed: true, Sessions: sessions})
	got = out.String()
	if !strings.Contains(got, "ses_closed") || !strings.Contains(got, "Closed") {
		t.Fatalf("show closed dashboard omitted closed session:\n%s", got)
	}
}

func TestRenderDetailViews(t *testing.T) {
	status := supervisor.SessionStatusResult{
		SessionSummary: storage.SessionSummary{
			Session: "ses_123", Provider: "codex", Title: "Fix race", Workspace: "wks_123", WorkspaceKey: "reader-race", Path: "/repo/wt",
			RunStatus: "NeedsInput", Git: map[string]any{"summary": "Dirty"}, Close: map[string]any{"summary": "StopRequired"},
		},
		WorkspaceKey: "reader-race",
		Path:         "/repo/wt",
		Tmux:         map[string]any{"target": "agency-ses_123"},
		Diff:         map[string]any{"available": true},
		RecentOutput: []storage.OutputChunk{{
			Stream: "Stdout", Bytes: []byte("waiting\n"),
		}},
	}
	var out bytes.Buffer
	RenderSession(&out, status)
	for _, want := range []string{"Agency session", "ses_123", "Needs input", "tmux: agency-ses_123", "Dirty", "waiting"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("session view missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	RenderWorktrees(&out, []supervisor.WorktreeSummary{{
		Workspace: "wks_123", WorkspaceKey: "reader-race", Branch: "agency/reader-race",
		Git: map[string]any{"summary": "Clean"}, Close: map[string]any{"summary": "Closable"},
		Sessions: []string{"ses_123"}, Marker: map[string]any{"matches": true}, Diff: map[string]any{"available": true},
	}})
	for _, want := range []string{"Agency worktrees", "reader-race", "wks_123", "Clean", "Closable", "ok", "available"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("worktree view missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	RenderWorktrees(&out, nil)
	for _, want := range []string{"No managed worktrees in this project.", "Start a worktree session with agency new codex --worktree."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("empty worktree view missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	RenderDoctor(&out, supervisor.DoctorReport{StateDB: "/state/agency.db", Issues: []supervisor.DoctorIssue{{
		Code: "TmuxTargetMissing", Target: map[string]any{"session": "ses_123"}, Summary: "Expected tmux target is missing.", Repair: "lost-tmux-target:ses_123",
	}}})
	for _, want := range []string{"Agency doctor", "TmuxTargetMissing", "ses_123", "repair: lost-tmux-target:ses_123"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor view missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	RenderFooter(&out)
	for _, want := range []string{"new <provider>", "open/attach <session>", "stop/close <session>", "worktree close <workspace>"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("footer missing %q:\n%s", want, out.String())
		}
	}
}
