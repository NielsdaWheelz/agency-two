package tui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"agency-two/internal/agency/content"
	"agency-two/internal/agency/storage"
	"agency-two/internal/agency/supervisor"
)

type Dashboard struct {
	Project    string
	Host       string
	ShowClosed bool
	Sessions   []storage.SessionSummary
}

func RenderDashboard(w io.Writer, dashboard Dashboard) {
	sessions := append([]storage.SessionSummary(nil), dashboard.Sessions...)
	sort.SliceStable(sessions, func(i, j int) bool {
		left := dashboardGroup(sessions[i].RunStatus)
		right := dashboardGroup(sessions[j].RunStatus)
		if left != right {
			return left < right
		}
		return sessions[i].LastEvent > sessions[j].LastEvent
	})

	project := dashboard.Project
	if project == "" {
		project = projectLabel(sessions)
	}
	host := dashboard.Host
	if host == "" {
		host = "local"
	}
	live, needsInput := dashboardCounts(sessions)
	fmt.Fprintf(w, "Agency  project: %s  host: %s  sessions: %d live, %d needs input\n", project, host, live, needsInput)
	if len(sessions) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "No agent sessions in this project.")
		fmt.Fprintln(w, "Start one with agency new codex or agency new claude.")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "SESSION     PROVIDER  TITLE                 PROJECT      WORKSPACE     RUN            GIT     MODEL                 EFFORT  LAST EVENT  CLOSE")
	for _, session := range sessions {
		if session.RunStatus == "Closed" && !dashboard.ShowClosed {
			continue
		}
		fmt.Fprintf(w, "%-10s  %-8s  %-20s  %-11s  %-12s  %-13s  %-6s  %-20s  %-6s  %-10s  %s\n",
			session.Session,
			session.Provider,
			trim(session.Title, 20),
			session.Project,
			content.WorkspaceLabel(session.WorkspaceKey),
			content.RunStatusLabel(session.RunStatus),
			content.GitSummaryLabel(gitSummary(session.Git)),
			trim(session.Model, 20),
			session.Effort,
			shortTime(session.LastEvent),
			content.CloseSummaryLabel(closeSummary(session.Close)),
		)
	}
}

func RenderSession(w io.Writer, status supervisor.SessionStatusResult) {
	fmt.Fprintf(w, "Agency session  %s  %s  %s\n", status.Session, status.Provider, content.RunStatusLabel(status.RunStatus))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "title:", status.Title)
	fmt.Fprintln(w, "workspace:", status.Workspace, "("+content.WorkspaceLabel(status.WorkspaceKey)+")")
	fmt.Fprintln(w, "path:", status.Path)
	fmt.Fprintln(w, "tmux:", mapString(status.Tmux, "target", "unavailable"))
	fmt.Fprintln(w, "git:", content.GitSummaryLabel(gitSummary(status.Git)))
	fmt.Fprintln(w, "model:", status.Model)
	fmt.Fprintln(w, "effort:", status.Effort)
	fmt.Fprintln(w, "close:", content.CloseSummaryLabel(closeSummary(status.Close)))
	if value := mapString(status.Launch, "permissionMode", ""); value != "" {
		fmt.Fprintln(w, "permission-mode:", value)
	}
	if value := mapString(status.Launch, "sandboxMode", ""); value != "" {
		fmt.Fprintln(w, "sandbox:", value)
	}
	if value := mapString(status.Launch, "approvalPolicy", ""); value != "" {
		fmt.Fprintln(w, "approval-policy:", value)
	}
	if available, ok := status.Diff["available"].(bool); ok && available {
		fmt.Fprintln(w, "diff: available")
	} else {
		fmt.Fprintln(w, "diff: unavailable")
	}
	if len(status.Events) > 0 {
		fmt.Fprintln(w, "events:", len(status.Events))
	}
	if len(status.RecentOutput) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "recent output:")
	for _, chunk := range status.RecentOutput {
		fmt.Fprint(w, string(chunk.Bytes))
	}
}

func RenderWorktrees(w io.Writer, worktrees []supervisor.WorktreeSummary) {
	fmt.Fprintln(w, "Agency worktrees")
	if len(worktrees) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "No managed worktrees in this project.")
		fmt.Fprintln(w, "Start a worktree session with agency new codex --worktree.")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "WORKTREE      HANDLE      BRANCH              GIT     CLOSE            SESSIONS  MARKER  DIFF")
	for _, wt := range worktrees {
		marker := "bad"
		if matches, ok := wt.Marker["matches"].(bool); ok && matches {
			marker = "ok"
		}
		diff := "unavailable"
		if available, ok := wt.Diff["available"].(bool); ok && available {
			diff = "available"
		}
		fmt.Fprintf(w, "%-13s %-11s %-19s %-7s %-16s %-9d %-7s %s\n",
			content.WorkspaceLabel(wt.WorkspaceKey),
			wt.Workspace,
			trim(wt.Branch, 19),
			content.GitSummaryLabel(gitSummary(wt.Git)),
			content.CloseSummaryLabel(closeSummary(wt.Close)),
			len(wt.Sessions),
			marker,
			diff,
		)
	}
}

func RenderDoctor(w io.Writer, report supervisor.DoctorReport) {
	fmt.Fprintln(w, "Agency doctor")
	fmt.Fprintln(w, "state:", report.StateDB)
	if len(report.Issues) == 0 {
		fmt.Fprintln(w, "issues: none")
		return
	}
	fmt.Fprintln(w)
	for _, issue := range report.Issues {
		fmt.Fprintln(w, "issue:", issue.Code)
		if session, ok := issue.Target["session"].(string); ok {
			fmt.Fprintln(w, "session:", session)
		}
		if run, ok := issue.Target["run"].(string); ok {
			fmt.Fprintln(w, "run:", run)
		}
		if env, ok := issue.Target["env"].(string); ok {
			fmt.Fprintln(w, "env:", env)
		}
		if provider, ok := issue.Target["provider"].(string); ok {
			fmt.Fprintln(w, "provider:", provider)
		}
		if command, ok := issue.Target["command"].(string); ok {
			fmt.Fprintln(w, "command:", command)
		}
		if workspace, ok := issue.Target["workspace"].(string); ok {
			fmt.Fprintln(w, "workspace:", workspace)
		}
		if project, ok := issue.Target["project"].(string); ok {
			fmt.Fprintln(w, "project:", project)
		}
		if branch, ok := issue.Target["branch"].(string); ok {
			fmt.Fprintln(w, "branch:", branch)
		}
		if path, ok := issue.Target["path"].(string); ok {
			fmt.Fprintln(w, "path:", path)
		}
		fmt.Fprintln(w, "summary:", issue.Summary)
		if issue.Repair != "" {
			fmt.Fprintln(w, "repair:", issue.Repair)
		}
	}
}

// LauncherView is the data for the launcher screen: the controls that will be
// used and the exact command preview, plus any blocking validation messages.
type LauncherView struct {
	Provider       string
	Profile        string
	Model          string
	Effort         string
	WorkspaceMode  string
	BaseRef        string
	PermissionMode string
	SandboxMode    string
	ApprovalPolicy string
	CommandPreview string
	Validation     []string
}

// RenderLauncher renders the launcher view. When validation messages are
// present the launch is blocked, matching the "launch button disabled when
// validation fails" rule.
func RenderLauncher(w io.Writer, view LauncherView) {
	fmt.Fprintln(w, "Agency launcher")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "provider:", view.Provider)
	if view.Profile != "" {
		fmt.Fprintln(w, "profile:", view.Profile)
	}
	fmt.Fprintln(w, "model:", view.Model)
	fmt.Fprintln(w, "effort:", view.Effort)
	if view.WorkspaceMode != "" {
		fmt.Fprintln(w, "workspace:", view.WorkspaceMode)
	}
	if view.BaseRef != "" {
		fmt.Fprintln(w, "base:", view.BaseRef)
	}
	if view.PermissionMode != "" {
		fmt.Fprintln(w, "permission-mode:", view.PermissionMode)
	}
	if view.SandboxMode != "" {
		fmt.Fprintln(w, "sandbox:", view.SandboxMode)
	}
	if view.ApprovalPolicy != "" {
		fmt.Fprintln(w, "approval-policy:", view.ApprovalPolicy)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Command preview")
	fmt.Fprintln(w, "argv:", view.CommandPreview)
	if len(view.Validation) > 0 {
		fmt.Fprintln(w)
		for _, message := range view.Validation {
			fmt.Fprintln(w, message)
		}
		fmt.Fprintln(w, "Launch disabled.")
	}
}

func RenderFooter(w io.Writer) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "q quit | r refresh | b dashboard | s/status <session> | new <provider> | open/attach <session> | stop/close <session> | w worktrees | worktree close <workspace> | d doctor")
}

func dashboardGroup(status string) int {
	switch status {
	case "NeedsInput":
		return 1
	case "NeedsApproval":
		return 2
	case "Live", "Starting", "Quiet":
		return 3
	case "Exited", "Stopped", "Killed", "Failed":
		return 4
	case "RepairRequired", "LostTmuxTarget", "LostRunner":
		return 5
	case "Closed":
		return 6
	default:
		return 7
	}
}

func dashboardCounts(sessions []storage.SessionSummary) (int, int) {
	live := 0
	needsInput := 0
	for _, session := range sessions {
		switch session.RunStatus {
		case "Live", "Starting", "Quiet":
			live++
		case "NeedsInput":
			needsInput++
		}
	}
	return live, needsInput
}

func projectLabel(sessions []storage.SessionSummary) string {
	if len(sessions) == 0 {
		return "all"
	}
	project := sessions[0].Project
	for _, session := range sessions[1:] {
		if session.Project != project {
			return "mixed"
		}
	}
	return project
}

func gitSummary(row map[string]any) string {
	value, ok := row["summary"].(string)
	if !ok || value == "" {
		return "NotAWorktree"
	}
	return value
}

func closeSummary(row map[string]any) string {
	value, ok := row["summary"].(string)
	if !ok || value == "" {
		return "RepairRequired"
	}
	return value
}

func mapString(row map[string]any, key, fallback string) string {
	value, ok := row[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func shortTime(raw string) string {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	return t.Format("15:04:05")
}

func trim(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 1 {
		return s[:limit]
	}
	return s[:limit-1] + "."
}
