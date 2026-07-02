package content

func RunStatusLabel(status string) string {
	switch status {
	case "Starting":
		return "Starting"
	case "Live":
		return "Live"
	case "NeedsInput":
		return "Needs input"
	case "NeedsApproval":
		return "Needs approval"
	case "Quiet":
		return "Quiet"
	case "Exited":
		return "Exited"
	case "Stopped":
		return "Stopped"
	case "Killed":
		return "Killed"
	case "Failed":
		return "Failed"
	case "LostTmuxTarget":
		return "Lost tmux target"
	case "LostRunner":
		return "Lost runner"
	case "Closed":
		return "Closed"
	case "RepairRequired":
		return "Repair required"
	default:
		panic("unknown RunStatus: " + status)
	}
}

func GitSummaryLabel(summary string) string {
	switch summary {
	case "Clean":
		return "Clean"
	case "Dirty":
		return "Dirty"
	case "Untracked":
		return "Untracked"
	case "IgnoredUserFiles":
		return "Ignored user files"
	case "Conflicts":
		return "Conflicts"
	case "Ahead":
		return "Ahead"
	case "Behind":
		return "Behind"
	case "Diverged":
		return "Diverged"
	case "DetachedHead":
		return "Detached head"
	case "MissingWorktree":
		return "Missing worktree"
	case "NotAWorktree":
		return "Not a worktree"
	default:
		panic("unknown GitSummary: " + summary)
	}
}

func CloseSummaryLabel(summary string) string {
	switch summary {
	case "Closable":
		return "Closable"
	case "StopRequired":
		return "Stop required"
	case "SharedWorkspaceBlocked":
		return "Shared workspace blocked"
	case "DirtyWorktreeBlocked":
		return "Dirty worktree blocked"
	case "UntrackedFilesBlocked":
		return "Untracked files blocked"
	case "IgnoredUserFilesBlocked":
		return "Ignored user files blocked"
	case "ConflictsBlocked":
		return "Conflicts blocked"
	case "UnpushedCommitsBlocked":
		return "Unpushed commits blocked"
	case "RepairRequired":
		return "Repair required"
	default:
		panic("unknown CloseSummary: " + summary)
	}
}

func WorkspaceLabel(workspaceKey string) string {
	if workspaceKey == "project_root" {
		return "project root"
	}
	return workspaceKey
}

func ErrorText(action, reason, target, detail, next string) string {
	return "Could not " + action + ".\n" +
		"reason: " + reason + "\n" +
		"target: " + target + "\n" +
		"detail: " + detail + "\n" +
		"next: " + next
}
