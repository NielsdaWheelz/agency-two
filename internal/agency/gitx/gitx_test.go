package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseStatusPorcelainZMultiAxis(t *testing.T) {
	raw := strings.Join([]string{
		"## feature...origin/feature [ahead 2, behind 1]",
		" M tracked.txt",
		"?? new.txt",
		"!! scratch.log",
		"UU conflict.txt",
		"",
	}, "\x00")

	status, err := ParseStatusPorcelainZ([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if status.Presence != PresencePresent {
		t.Fatalf("presence = %q, want %q", status.Presence, PresencePresent)
	}
	if status.Tree != TreeDirty {
		t.Fatalf("tree = %q, want %q", status.Tree, TreeDirty)
	}
	if !status.Untracked || !status.IgnoredUserFiles || !status.Conflicts {
		t.Fatalf("axes not set: %+v", status)
	}
	if status.Upstream != UpstreamDiverged {
		t.Fatalf("upstream = %q, want %q", status.Upstream, UpstreamDiverged)
	}
	if status.Summary != SummaryConflicts {
		t.Fatalf("summary = %q, want %q", status.Summary, SummaryConflicts)
	}
	wantBlockers := []CloseBlocker{
		BlockerWorktreeDirty,
		BlockerWorktreeHasUntrackedFiles,
		BlockerWorktreeHasIgnoredUserFiles,
		BlockerWorktreeHasConflicts,
		BlockerBranchHasUnpushedCommits,
	}
	if got := CloseBlockersForStatus(status, StrictClosePolicy()); !reflect.DeepEqual(got, wantBlockers) {
		t.Fatalf("blockers = %#v, want %#v", got, wantBlockers)
	}
}

func TestParseStatusHeaders(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		upstream UpstreamState
		summary  GitSummary
	}{
		{"current", "## main...origin/main\x00", UpstreamCurrent, SummaryClean},
		{"ahead", "## main...origin/main [ahead 1]\x00", UpstreamAhead, SummaryAhead},
		{"behind", "## main...origin/main [behind 1]\x00", UpstreamBehind, SummaryBehind},
		{"diverged", "## main...origin/main [ahead 1, behind 2]\x00", UpstreamDiverged, SummaryDiverged},
		{"gone", "## main...origin/main [gone]\x00", UpstreamNoUpstream, SummaryClean},
		{"no upstream", "## main\x00", UpstreamNoUpstream, SummaryClean},
		{"detached", "## HEAD (no branch)\x00", UpstreamDetached, SummaryDetachedHead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := ParseStatusPorcelainZ([]byte(tt.header))
			if err != nil {
				t.Fatal(err)
			}
			if status.Upstream != tt.upstream {
				t.Fatalf("upstream = %q, want %q", status.Upstream, tt.upstream)
			}
			if status.Summary != tt.summary {
				t.Fatalf("summary = %q, want %q", status.Summary, tt.summary)
			}
		})
	}
}

func TestParseStatusRenameRecord(t *testing.T) {
	raw := "## main\x00R  new-name.txt\x00old-name.txt\x00"
	status, err := ParseStatusPorcelainZ([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(status.Files))
	}
	file := status.Files[0]
	if file.Code != "R " || file.Path != "new-name.txt" || file.OrigPath != "old-name.txt" {
		t.Fatalf("rename parse = %+v", file)
	}
	if status.Summary != SummaryDirty {
		t.Fatalf("summary = %q, want %q", status.Summary, SummaryDirty)
	}
}

func TestCloseBlockersForMissingStatusAreStructuralOnly(t *testing.T) {
	status := StatusSnapshot{Presence: PresenceMissing, Tree: TreeClean, Upstream: UpstreamNoUpstream}
	got := CloseBlockersForStatus(status, StrictClosePolicy())
	want := []CloseBlocker{BlockerGitWorktreeMissing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers = %#v, want %#v", got, want)
	}
}

func TestParseWorktreeListPorcelainZ(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /repo",
		"HEAD abc123",
		"branch refs/heads/main",
		"",
		"worktree /repo-wt",
		"HEAD def456",
		"branch refs/heads/agency/test",
		"locked user requested",
		"",
		"worktree /repo-detached",
		"HEAD fedcba",
		"detached",
		"prunable missing",
		"",
	}, "\x00")

	worktrees, err := ParseWorktreeListPorcelainZ([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(worktrees) != 3 {
		t.Fatalf("worktrees = %d, want 3", len(worktrees))
	}
	if worktrees[1].BranchName != "agency/test" || !worktrees[1].Locked || worktrees[1].LockReason != "user requested" {
		t.Fatalf("second worktree parse = %+v", worktrees[1])
	}
	if !worktrees[2].Detached || !worktrees[2].Prunable || worktrees[2].PruneReason != "missing" {
		t.Fatalf("detached worktree parse = %+v", worktrees[2])
	}
}

func TestWorktreeGuards(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "repo")
	branchPath := filepath.Join(root, "branch")
	outsidePath := filepath.Join(t.TempDir(), "outside")
	mkdirAll(t, mainPath)
	mkdirAll(t, branchPath)
	mkdirAll(t, outsidePath)

	worktrees := []Worktree{
		{Path: mainPath, BranchName: "main"},
		{Path: branchPath, BranchRef: "refs/heads/agency/test"},
	}

	ok, err := BranchCheckedOutElsewhere(worktrees, mainPath, "agency/test")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected agency/test to be checked out outside main worktree")
	}
	ok, err = BranchCheckedOutElsewhere(worktrees, branchPath, "refs/heads/agency/test")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("did not expect branch to be elsewhere when expected path owns it")
	}
	ok, err = PathWithinRoots(filepath.Join(root, "repo"), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected repo path inside root")
	}
	ok, err = PathWithinRoots(outsidePath, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected outside path to be rejected")
	}
}

func TestGitIntegrationRootStatusDiff(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := newRepo(t)
	mkdirAll(t, filepath.Join(repo, "sub"))
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	runGit(t, repo, "add", "README.md", ".gitignore")
	runGit(t, repo, "commit", "-m", "initial")
	base, err := HeadSHA(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	root, err := Root(ctx, filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if root != repo {
		t.Fatalf("root = %q, want %q", root, repo)
	}

	clean, err := Status(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Summary != SummaryClean || clean.Tree != TreeClean {
		t.Fatalf("clean status = %+v", clean)
	}

	writeFile(t, filepath.Join(repo, "README.md"), "hello\nchanged\n")
	writeFile(t, filepath.Join(repo, "new.txt"), "new\n")
	writeFile(t, filepath.Join(repo, "scratch.log"), "ignored\n")
	dirty, err := Status(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if dirty.Summary != SummaryDirty || dirty.Tree != TreeDirty || !dirty.Untracked || !dirty.IgnoredUserFiles {
		t.Fatalf("dirty status = %+v", dirty)
	}

	diff, err := Diff(ctx, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Stat, "README.md") {
		t.Fatalf("diff stat missing README.md: %q", diff.Stat)
	}
	if !strings.Contains(diff.Patch, "+changed") {
		t.Fatalf("diff patch missing change: %q", diff.Patch)
	}

	missing, err := Status(ctx, filepath.Join(repo, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Presence != PresenceMissing || missing.Summary != SummaryMissingWorktree {
		t.Fatalf("missing status = %+v", missing)
	}

	notRepo := filepath.Join(t.TempDir(), "not-repo")
	mkdirAll(t, notRepo)
	notAWorktree, err := Status(ctx, notRepo)
	if err != nil {
		t.Fatal(err)
	}
	if notAWorktree.Presence != PresenceNotAWorktree || notAWorktree.Summary != SummaryNotAWorktree {
		t.Fatalf("not-a-worktree status = %+v", notAWorktree)
	}
}

func TestGitIntegrationWorktreeAddListRemove(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "agency-test")
	created, err := AddWorktree(ctx, repo, worktreePath, "agency/test-worktree", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if created.BranchName != "agency/test-worktree" {
		t.Fatalf("created branch = %q", created.BranchName)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatal(err)
	}

	worktrees, err := WorktreeList(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FindWorktree(worktrees, worktreePath); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("created worktree was not listed")
	}
	checkedOut, err := BranchCheckedOutElsewhere(worktrees, repo, "agency/test-worktree")
	if err != nil {
		t.Fatal(err)
	}
	if !checkedOut {
		t.Fatal("expected created branch to be checked out outside main repo")
	}

	if err := RemoveWorktree(ctx, repo, worktreePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktreePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree path still exists or unexpected stat error: %v", err)
	}
	worktrees, err = WorktreeList(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FindWorktree(worktrees, worktreePath); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("removed worktree was still listed")
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is not available")
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "agency-tests@example.invalid")
	runGit(t, repo, "config", "user.name", "Agency Tests")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	fullArgs := append([]string{"-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}
