package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type CloseBlocker string

const (
	BlockerSessionStillRunning         CloseBlocker = "SessionStillRunning"
	BlockerWorkspaceShared             CloseBlocker = "WorkspaceShared"
	BlockerWorktreeDirty               CloseBlocker = "WorktreeDirty"
	BlockerWorktreeHasUntrackedFiles   CloseBlocker = "WorktreeHasUntrackedFiles"
	BlockerWorktreeHasIgnoredUserFiles CloseBlocker = "WorktreeHasIgnoredUserFiles"
	BlockerWorktreeHasConflicts        CloseBlocker = "WorktreeHasConflicts"
	BlockerBranchHasUnpushedCommits    CloseBlocker = "BranchHasUnpushedCommits"
	BlockerNoRemoteTrackingProof       CloseBlocker = "NoRemoteTrackingProof"
	BlockerOwnershipMarkerMismatch     CloseBlocker = "OwnershipMarkerMismatch"
	BlockerPathOutsideWorkspaceRoot    CloseBlocker = "PathOutsideWorkspaceRoot"
	BlockerGitWorktreeMissing          CloseBlocker = "GitWorktreeMissing"
	BlockerNotExpectedWorktree         CloseBlocker = "NotExpectedWorktree"
	BlockerBranchCheckedOutElsewhere   CloseBlocker = "BranchCheckedOutElsewhere"
	BlockerTmuxTargetMissing           CloseBlocker = "TmuxTargetMissing"
	BlockerRunnerHeartbeatExpired      CloseBlocker = "RunnerHeartbeatExpired"
)

type CloseSummary string

const (
	CloseSummaryClosable                CloseSummary = "Closable"
	CloseSummaryStopRequired            CloseSummary = "StopRequired"
	CloseSummarySharedWorkspaceBlocked  CloseSummary = "SharedWorkspaceBlocked"
	CloseSummaryDirtyWorktreeBlocked    CloseSummary = "DirtyWorktreeBlocked"
	CloseSummaryUntrackedFilesBlocked   CloseSummary = "UntrackedFilesBlocked"
	CloseSummaryIgnoredUserFilesBlocked CloseSummary = "IgnoredUserFilesBlocked"
	CloseSummaryConflictsBlocked        CloseSummary = "ConflictsBlocked"
	CloseSummaryUnpushedCommitsBlocked  CloseSummary = "UnpushedCommitsBlocked"
	CloseSummaryRepairRequired          CloseSummary = "RepairRequired"
)

type ClosePolicy struct {
	BlockDirtyWorktree        bool
	BlockUntrackedFiles       bool
	BlockIgnoredUserFiles     bool
	BlockConflicts            bool
	BlockUnpushedCommits      bool
	BlockMissingUpstreamProof bool
}

func StrictClosePolicy() ClosePolicy {
	return ClosePolicy{
		BlockDirtyWorktree:        true,
		BlockUntrackedFiles:       true,
		BlockIgnoredUserFiles:     true,
		BlockConflicts:            true,
		BlockUnpushedCommits:      true,
		BlockMissingUpstreamProof: true,
	}
}

func SummarizeStatus(status StatusSnapshot) GitSummary {
	switch status.Presence {
	case PresenceNotAWorktree:
		return SummaryNotAWorktree
	case PresenceMissing:
		return SummaryMissingWorktree
	}
	if status.Upstream == UpstreamDetached {
		return SummaryDetachedHead
	}
	if status.Conflicts {
		return SummaryConflicts
	}
	if status.Tree == TreeDirty {
		return SummaryDirty
	}
	if status.Untracked {
		return SummaryUntracked
	}
	if status.IgnoredUserFiles {
		return SummaryIgnoredUserFiles
	}
	switch status.Upstream {
	case UpstreamDiverged:
		return SummaryDiverged
	case UpstreamBehind:
		return SummaryBehind
	case UpstreamAhead:
		return SummaryAhead
	default:
		return SummaryClean
	}
}

func CloseBlockersForStatus(status StatusSnapshot, policy ClosePolicy) []CloseBlocker {
	var blockers []CloseBlocker
	switch status.Presence {
	case PresenceMissing:
		blockers = append(blockers, BlockerGitWorktreeMissing)
	case PresenceNotAWorktree:
		blockers = append(blockers, BlockerNotExpectedWorktree)
	}
	if status.Presence != PresencePresent {
		return blockers
	}
	if policy.BlockDirtyWorktree && status.Tree == TreeDirty {
		blockers = append(blockers, BlockerWorktreeDirty)
	}
	if policy.BlockUntrackedFiles && status.Untracked {
		blockers = append(blockers, BlockerWorktreeHasUntrackedFiles)
	}
	if policy.BlockIgnoredUserFiles && status.IgnoredUserFiles {
		blockers = append(blockers, BlockerWorktreeHasIgnoredUserFiles)
	}
	if policy.BlockConflicts && status.Conflicts {
		blockers = append(blockers, BlockerWorktreeHasConflicts)
	}
	if policy.BlockUnpushedCommits && (status.Upstream == UpstreamAhead || status.Upstream == UpstreamDiverged) {
		blockers = append(blockers, BlockerBranchHasUnpushedCommits)
	}
	if policy.BlockMissingUpstreamProof && (status.Upstream == UpstreamNoUpstream || status.Upstream == UpstreamDetached) {
		blockers = append(blockers, BlockerNoRemoteTrackingProof)
	}
	return blockers
}

func SummarizeClose(blockers []CloseBlocker) CloseSummary {
	if len(blockers) == 0 {
		return CloseSummaryClosable
	}
	has := func(targets ...CloseBlocker) bool {
		for _, target := range targets {
			for _, blocker := range blockers {
				if blocker == target {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(BlockerSessionStillRunning):
		return CloseSummaryStopRequired
	case has(BlockerWorkspaceShared):
		return CloseSummarySharedWorkspaceBlocked
	case has(BlockerWorktreeDirty):
		return CloseSummaryDirtyWorktreeBlocked
	case has(BlockerWorktreeHasUntrackedFiles):
		return CloseSummaryUntrackedFilesBlocked
	case has(BlockerWorktreeHasIgnoredUserFiles):
		return CloseSummaryIgnoredUserFilesBlocked
	case has(BlockerWorktreeHasConflicts):
		return CloseSummaryConflictsBlocked
	case has(BlockerBranchHasUnpushedCommits, BlockerNoRemoteTrackingProof):
		return CloseSummaryUnpushedCommitsBlocked
	default:
		return CloseSummaryRepairRequired
	}
}

func FindWorktree(worktrees []Worktree, path string) (Worktree, bool, error) {
	target, err := comparablePath(path)
	if err != nil {
		return Worktree{}, false, err
	}
	for _, worktree := range worktrees {
		current, err := comparablePath(worktree.Path)
		if err != nil {
			return Worktree{}, false, err
		}
		if current == target {
			return worktree, true, nil
		}
	}
	return Worktree{}, false, nil
}

func BranchCheckedOutElsewhere(worktrees []Worktree, expectedPath, branch string) (bool, error) {
	if strings.TrimSpace(branch) == "" {
		return false, nil
	}
	expected, err := comparablePath(expectedPath)
	if err != nil {
		return false, err
	}
	for _, worktree := range worktrees {
		current, err := comparablePath(worktree.Path)
		if err != nil {
			return false, err
		}
		if current == expected || worktree.Detached || worktree.Bare {
			continue
		}
		if branchMatches(worktree, branch) {
			return true, nil
		}
	}
	return false, nil
}

func PathWithinRoots(path string, roots []string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("path is required")
	}
	if len(roots) == 0 {
		return false, nil
	}
	resolvedPath, err := realAbs(path)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		resolvedRoot, err := realAbs(root)
		if err != nil {
			return false, err
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedPath)
		if err != nil {
			return false, err
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return true, nil
		}
	}
	return false, nil
}

func branchMatches(worktree Worktree, branch string) bool {
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if worktree.BranchName == branch {
		return true
	}
	return strings.TrimPrefix(worktree.BranchRef, "refs/heads/") == branch
}

func comparablePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	return realAbs(path)
}

func realAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Clean(abs), nil
		}
		return "", err
	}
	return filepath.Clean(resolved), nil
}
