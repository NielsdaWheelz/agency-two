package content

import "testing"

func TestRunStatusLabel(t *testing.T) {
	tests := []struct {
		status string
		label  string
	}{
		{"Starting", "Starting"},
		{"Live", "Live"},
		{"NeedsInput", "Needs input"},
		{"NeedsApproval", "Needs approval"},
		{"Quiet", "Quiet"},
		{"Exited", "Exited"},
		{"Stopped", "Stopped"},
		{"Killed", "Killed"},
		{"Failed", "Failed"},
		{"LostTmuxTarget", "Lost tmux target"},
		{"LostRunner", "Lost runner"},
		{"Closed", "Closed"},
		{"RepairRequired", "Repair required"},
	}
	for _, test := range tests {
		if got := RunStatusLabel(test.status); got != test.label {
			t.Fatalf("RunStatusLabel(%q) = %q, want %q", test.status, got, test.label)
		}
	}
}

func TestGitSummaryLabel(t *testing.T) {
	tests := []struct {
		summary string
		label   string
	}{
		{"Clean", "Clean"},
		{"Dirty", "Dirty"},
		{"Untracked", "Untracked"},
		{"IgnoredUserFiles", "Ignored user files"},
		{"Conflicts", "Conflicts"},
		{"Ahead", "Ahead"},
		{"Behind", "Behind"},
		{"Diverged", "Diverged"},
		{"DetachedHead", "Detached head"},
		{"MissingWorktree", "Missing worktree"},
		{"NotAWorktree", "Not a worktree"},
	}
	for _, test := range tests {
		if got := GitSummaryLabel(test.summary); got != test.label {
			t.Fatalf("GitSummaryLabel(%q) = %q, want %q", test.summary, got, test.label)
		}
	}
}

func TestCloseSummaryLabel(t *testing.T) {
	tests := []struct {
		summary string
		label   string
	}{
		{"Closable", "Closable"},
		{"StopRequired", "Stop required"},
		{"SharedWorkspaceBlocked", "Shared workspace blocked"},
		{"DirtyWorktreeBlocked", "Dirty worktree blocked"},
		{"UntrackedFilesBlocked", "Untracked files blocked"},
		{"IgnoredUserFilesBlocked", "Ignored user files blocked"},
		{"ConflictsBlocked", "Conflicts blocked"},
		{"UnpushedCommitsBlocked", "Unpushed commits blocked"},
		{"RepairRequired", "Repair required"},
	}
	for _, test := range tests {
		if got := CloseSummaryLabel(test.summary); got != test.label {
			t.Fatalf("CloseSummaryLabel(%q) = %q, want %q", test.summary, got, test.label)
		}
	}
}

func TestWorkspaceLabel(t *testing.T) {
	if got := WorkspaceLabel("project_root"); got != "project root" {
		t.Fatalf("WorkspaceLabel(project_root) = %q", got)
	}
	if got := WorkspaceLabel("reader-race"); got != "reader-race" {
		t.Fatalf("WorkspaceLabel(reader-race) = %q", got)
	}
}

func TestErrorText(t *testing.T) {
	got := ErrorText(
		"close worktree",
		"Untracked files blocked",
		"wks_7a23c1",
		"3 untracked files are present.",
		"Review the files or remove them, then run agency worktree close wks_7a23c1.",
	)
	want := "Could not close worktree.\n" +
		"reason: Untracked files blocked\n" +
		"target: wks_7a23c1\n" +
		"detail: 3 untracked files are present.\n" +
		"next: Review the files or remove them, then run agency worktree close wks_7a23c1."
	if got != want {
		t.Fatalf("ErrorText() = %q, want %q", got, want)
	}
}
