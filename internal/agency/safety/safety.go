package safety

import (
	"context"
	"os"
	"strings"

	"agency-two/internal/agency/gitx"
	"agency-two/internal/agency/storage"
)

type Finding struct {
	Blocker  gitx.CloseBlocker
	Severity string
	Message  string
	Location map[string]any
	Evidence map[string]any
}

type WorktreeCloseInput struct {
	Worktree     storage.ManagedWorktree
	GitStatus    gitx.StatusSnapshot
	Worktrees    []gitx.Worktree
	LiveSessions []storage.SessionDetail
	TargetExists func(context.Context, string) (bool, error)
	// RunnerLost reports whether a run's runner has missed its heartbeat TTL on
	// the supervisor's monotonic clock. Liveness is never derived from a
	// persisted wall-clock delta, so the safety service asks the run owner.
	RunnerLost func(runID string) bool
}

func EvaluateWorktreeClose(ctx context.Context, input WorktreeCloseInput) ([]Finding, error) {
	wt := input.Worktree
	status := gitx.StatusWithoutPaths(input.GitStatus, ".agency-worktree")
	var findings []Finding
	add := func(blocker gitx.CloseBlocker, evidence map[string]any) {
		for _, finding := range findings {
			if finding.Blocker == blocker {
				return
			}
		}
		findings = append(findings, Finding{
			Blocker:  blocker,
			Severity: severity(blocker),
			Message:  message(blocker, wt, evidence),
			Location: map[string]any{"workspace": wt.WorkspaceKey, "path": wt.Path},
			Evidence: evidence,
		})
	}
	for _, blocker := range gitx.CloseBlockersForStatus(status, gitx.StrictClosePolicy()) {
		add(blocker, map[string]any{"gitSummary": string(gitx.SummarizeStatus(status)), "branch": status.Branch, "upstream": string(status.Upstream)})
	}
	rawMarker, err := os.ReadFile(wt.MarkerFilePath)
	if err != nil || strings.TrimSpace(string(rawMarker)) != wt.MarkerFileHash {
		add(gitx.BlockerOwnershipMarkerMismatch, map[string]any{"markerFile": wt.MarkerFilePath})
	}
	inside, err := gitx.PathWithinRoots(wt.Path, []string{wt.ManagedRoot})
	if err != nil {
		return nil, err
	}
	if !inside {
		add(gitx.BlockerPathOutsideWorkspaceRoot, map[string]any{"managedRoot": wt.ManagedRoot})
	}
	found, ok, err := gitx.FindWorktree(input.Worktrees, wt.Path)
	if err != nil {
		return nil, err
	}
	if !ok {
		add(gitx.BlockerGitWorktreeMissing, map[string]any{"expectedPath": wt.Path})
	} else if found.BranchName != wt.Branch && found.BranchRef != "refs/heads/"+wt.Branch {
		add(gitx.BlockerNotExpectedWorktree, map[string]any{"expectedBranch": wt.Branch, "actualBranch": found.BranchRef})
	}
	elsewhere, err := gitx.BranchCheckedOutElsewhere(input.Worktrees, wt.Path, wt.Branch)
	if err != nil {
		return nil, err
	}
	if elsewhere {
		add(gitx.BlockerBranchCheckedOutElsewhere, map[string]any{"branch": wt.Branch})
	}
	if len(input.LiveSessions) > 0 {
		sessions := make([]string, 0, len(input.LiveSessions))
		for _, detail := range input.LiveSessions {
			sessions = append(sessions, detail.Summary.Session)
		}
		add(gitx.BlockerSessionStillRunning, map[string]any{"sessions": sessions})
	}
	if len(input.LiveSessions) > 1 {
		add(gitx.BlockerWorkspaceShared, map[string]any{"liveSessionCount": len(input.LiveSessions)})
	}
	for _, detail := range input.LiveSessions {
		// RunnerHeartbeatExpired is a structural blocker: always enforced,
		// independent of the tmux observation, and evaluated on the monotonic
		// clock via the injected predicate (never a persisted wall-clock delta).
		if input.RunnerLost != nil && detail.Summary.HasActiveBinding && input.RunnerLost(detail.Summary.RunID) {
			add(gitx.BlockerRunnerHeartbeatExpired, map[string]any{"session": detail.Summary.Session})
		}
		if input.TargetExists == nil || detail.RunnerSocket == "" {
			continue
		}
		ok, err := input.TargetExists(ctx, detail.TmuxSessionName)
		if err != nil {
			return nil, err
		}
		if !ok {
			add(gitx.BlockerTmuxTargetMissing, map[string]any{"session": detail.Summary.Session, "tmuxTarget": detail.TmuxSessionName})
		}
	}
	return findings, nil
}

func severity(blocker gitx.CloseBlocker) string {
	switch blocker {
	case gitx.BlockerOwnershipMarkerMismatch, gitx.BlockerPathOutsideWorkspaceRoot, gitx.BlockerGitWorktreeMissing, gitx.BlockerNotExpectedWorktree, gitx.BlockerBranchCheckedOutElsewhere, gitx.BlockerTmuxTargetMissing, gitx.BlockerRunnerHeartbeatExpired:
		return "Defect"
	default:
		return "Blocker"
	}
}

func message(blocker gitx.CloseBlocker, wt storage.ManagedWorktree, evidence map[string]any) string {
	switch blocker {
	case gitx.BlockerSessionStillRunning:
		return "Live sessions use workspace " + wt.WorkspaceKey + "."
	case gitx.BlockerWorkspaceShared:
		return "Workspace " + wt.WorkspaceKey + " is shared by live sessions."
	case gitx.BlockerWorktreeDirty:
		return "Tracked changes block worktree close."
	case gitx.BlockerWorktreeHasUntrackedFiles:
		return "Untracked files block worktree close."
	case gitx.BlockerWorktreeHasIgnoredUserFiles:
		return "Ignored user files block worktree close."
	case gitx.BlockerWorktreeHasConflicts:
		return "Conflicts block worktree close."
	case gitx.BlockerBranchHasUnpushedCommits:
		return "Branch " + wt.Branch + " has unpushed commits."
	case gitx.BlockerNoRemoteTrackingProof:
		return "Branch " + wt.Branch + " has no remote tracking proof."
	case gitx.BlockerOwnershipMarkerMismatch:
		return "Ownership marker does not match workspace " + wt.WorkspaceKey + "."
	case gitx.BlockerPathOutsideWorkspaceRoot:
		return "Worktree path is outside the managed root."
	case gitx.BlockerGitWorktreeMissing:
		return "Expected git worktree is missing."
	case gitx.BlockerNotExpectedWorktree:
		return "Git worktree does not match the expected branch."
	case gitx.BlockerBranchCheckedOutElsewhere:
		return "Branch " + wt.Branch + " is checked out in another worktree."
	case gitx.BlockerTmuxTargetMissing:
		return "Expected tmux target is missing."
	case gitx.BlockerRunnerHeartbeatExpired:
		return "Runner heartbeat expired."
	default:
		return "Worktree close is blocked."
	}
}
