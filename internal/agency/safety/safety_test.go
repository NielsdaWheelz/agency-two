package safety

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agency-two/internal/agency/gitx"
	"agency-two/internal/agency/storage"
)

func TestEvaluateWorktreeCloseCombinesGitStructureAndLiveSessionFindings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "repo", "worktree")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, ".agency-worktree")
	if err := os.WriteFile(marker, []byte("wrong\n"), 0600); err != nil {
		t.Fatal(err)
	}
	findings, err := EvaluateWorktreeClose(context.Background(), WorktreeCloseInput{
		Worktree: storage.ManagedWorktree{
			WorkspaceID: "workspace-id", WorkspaceKey: "reader_race", Path: path,
			Branch: "agency/reader-race", MarkerFilePath: marker, MarkerFileHash: "expected", ManagedRoot: filepath.Join(root, "repo"),
		},
		GitStatus: gitx.StatusSnapshot{
			Presence: gitx.PresencePresent, Tree: gitx.TreeDirty, Upstream: gitx.UpstreamAhead, Branch: "agency/reader-race",
			Files: []gitx.FileStatus{{Code: " M", Path: "README.md"}, {Code: "??", Path: ".agency-worktree"}},
		},
		Worktrees: []gitx.Worktree{{Path: path, BranchName: "agency/reader-race"}},
		LiveSessions: []storage.SessionDetail{{
			Summary:      storage.SessionSummary{Session: "ses_live"},
			RunnerSocket: "/tmp/runner.sock", TmuxSessionName: "agency-ses_live",
		}},
		TargetExists: func(context.Context, string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []gitx.CloseBlocker{
		gitx.BlockerWorktreeDirty,
		gitx.BlockerBranchHasUnpushedCommits,
		gitx.BlockerOwnershipMarkerMismatch,
		gitx.BlockerSessionStillRunning,
		gitx.BlockerTmuxTargetMissing,
	}
	for _, blocker := range want {
		if !hasFinding(findings, blocker) {
			t.Fatalf("missing finding %s in %+v", blocker, findings)
		}
	}
	if hasFinding(findings, gitx.BlockerWorktreeHasUntrackedFiles) {
		t.Fatalf(".agency-worktree marker should not create an untracked blocker: %+v", findings)
	}
}

func hasFinding(findings []Finding, blocker gitx.CloseBlocker) bool {
	for _, finding := range findings {
		if finding.Blocker == blocker {
			return true
		}
	}
	return false
}
