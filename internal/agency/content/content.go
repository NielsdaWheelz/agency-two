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

// CannotLaunch renders a blocking launcher validation message. Launcher
// validation is inline and blocking per the content design.
func CannotLaunch(detail string) string {
	return "Cannot launch. " + detail
}

// LaunchBlockedUnsupportedEffort is the launcher message for an unsupported
// effort control.
func LaunchBlockedUnsupportedEffort(providerKey, effort string) string {
	return CannotLaunch("Provider " + providerKey + " does not support effort " + effort + ".")
}

// LaunchBlockedBaseRef is the launcher message for an unresolvable base ref.
func LaunchBlockedBaseRef(ref string) string {
	return CannotLaunch("Base ref " + ref + " could not be resolved.")
}

// LaunchBlockedTmux is the launcher message for an unavailable tmux host.
func LaunchBlockedTmux(hostKey string) string {
	return CannotLaunch("tmux is not available on host " + hostKey + ".")
}
