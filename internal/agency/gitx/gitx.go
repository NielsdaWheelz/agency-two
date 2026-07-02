package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrGitMissing = errors.New("git executable not found")

type Client struct {
	GitPath string
}

type CommandError struct {
	Dir    string
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("git %s failed: %s", strings.Join(e.Args, " "), strings.TrimSpace(e.Stderr))
	}
	return fmt.Sprintf("git %s failed: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

type Presence string

const (
	PresencePresent      Presence = "Present"
	PresenceMissing      Presence = "Missing"
	PresenceNotAWorktree Presence = "NotAWorktree"
)

type TreeState string

const (
	TreeClean TreeState = "Clean"
	TreeDirty TreeState = "Dirty"
)

type UpstreamState string

const (
	UpstreamCurrent    UpstreamState = "Current"
	UpstreamAhead      UpstreamState = "Ahead"
	UpstreamBehind     UpstreamState = "Behind"
	UpstreamDiverged   UpstreamState = "Diverged"
	UpstreamNoUpstream UpstreamState = "NoUpstream"
	UpstreamDetached   UpstreamState = "Detached"
)

type GitSummary string

const (
	SummaryNotAWorktree     GitSummary = "NotAWorktree"
	SummaryMissingWorktree  GitSummary = "MissingWorktree"
	SummaryDetachedHead     GitSummary = "DetachedHead"
	SummaryConflicts        GitSummary = "Conflicts"
	SummaryDirty            GitSummary = "Dirty"
	SummaryUntracked        GitSummary = "Untracked"
	SummaryIgnoredUserFiles GitSummary = "IgnoredUserFiles"
	SummaryDiverged         GitSummary = "Diverged"
	SummaryBehind           GitSummary = "Behind"
	SummaryAhead            GitSummary = "Ahead"
	SummaryClean            GitSummary = "Clean"
)

type StatusSnapshot struct {
	Summary          GitSummary
	Presence         Presence
	Tree             TreeState
	Untracked        bool
	IgnoredUserFiles bool
	Conflicts        bool
	Upstream         UpstreamState
	Branch           string
	UpstreamRef      string
	Files            []FileStatus
}

type FileStatus struct {
	Code     string
	Path     string
	OrigPath string
}

type DiffResult struct {
	Base  string
	Stat  string
	Patch string
}

type Worktree struct {
	Path        string
	HeadSHA     string
	BranchRef   string
	BranchName  string
	Detached    bool
	Bare        bool
	Locked      bool
	LockReason  string
	Prunable    bool
	PruneReason string
}

func Root(ctx context.Context, dir string) (string, error) {
	return Client{}.Root(ctx, dir)
}

func HeadSHA(ctx context.Context, dir string) (string, error) {
	return Client{}.HeadSHA(ctx, dir)
}

func ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	return Client{}.ResolveRef(ctx, dir, ref)
}

func Status(ctx context.Context, dir string) (StatusSnapshot, error) {
	return Client{}.Status(ctx, dir)
}

func Diff(ctx context.Context, dir, base string) (DiffResult, error) {
	return Client{}.Diff(ctx, dir, base)
}

func AddWorktree(ctx context.Context, repoDir, path, branch, baseRef string) (Worktree, error) {
	return Client{}.AddWorktree(ctx, repoDir, path, branch, baseRef)
}

func RemoveWorktree(ctx context.Context, repoDir, path string) error {
	return Client{}.RemoveWorktree(ctx, repoDir, path)
}

func WorktreeList(ctx context.Context, repoDir string) ([]Worktree, error) {
	return Client{}.WorktreeList(ctx, repoDir)
}

func LocalBranches(ctx context.Context, repoDir string) ([]string, error) {
	return Client{}.LocalBranches(ctx, repoDir)
}

func (c Client) Root(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", errors.New("git root was empty")
	}
	return filepath.Clean(root), nil
}

func (c Client) HeadSHA(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("git HEAD sha was empty")
	}
	return sha, nil
}

func (c Client) ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", errors.New("git ref is required")
	}
	out, err := c.run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("git ref sha was empty")
	}
	return sha, nil
}

func (c Client) Status(ctx context.Context, dir string) (StatusSnapshot, error) {
	if dir == "" {
		return StatusSnapshot{}, errors.New("git status dir is required")
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return statusWithPresence(PresenceMissing), nil
		}
		return StatusSnapshot{}, err
	}
	out, err := c.run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if isNotRepository(err) {
			return statusWithPresence(PresenceNotAWorktree), nil
		}
		return StatusSnapshot{}, err
	}
	if strings.TrimSpace(string(out)) != "true" {
		return statusWithPresence(PresenceNotAWorktree), nil
	}
	out, err = c.run(ctx, dir, "status", "--porcelain=v1", "-z", "--branch", "--ignored=matching")
	if err != nil {
		return StatusSnapshot{}, err
	}
	return ParseStatusPorcelainZ(out)
}

func (c Client) Diff(ctx context.Context, dir, base string) (DiffResult, error) {
	if strings.TrimSpace(base) == "" {
		return DiffResult{}, errors.New("git diff base is required")
	}
	stat, err := c.run(ctx, dir, "diff", "--stat", "--no-ext-diff", "--no-color", base, "--")
	if err != nil {
		return DiffResult{}, err
	}
	patch, err := c.run(ctx, dir, "diff", "--no-ext-diff", "--no-color", base, "--")
	if err != nil {
		return DiffResult{}, err
	}
	return DiffResult{Base: base, Stat: string(stat), Patch: string(patch)}, nil
}

func (c Client) AddWorktree(ctx context.Context, repoDir, path, branch, baseRef string) (Worktree, error) {
	if strings.TrimSpace(path) == "" {
		return Worktree{}, errors.New("worktree path is required")
	}
	if strings.TrimSpace(branch) == "" {
		return Worktree{}, errors.New("worktree branch is required")
	}
	if strings.TrimSpace(baseRef) == "" {
		return Worktree{}, errors.New("worktree base ref is required")
	}
	if _, err := c.run(ctx, repoDir, "worktree", "add", "-b", branch, path, baseRef); err != nil {
		return Worktree{}, err
	}
	worktrees, err := c.WorktreeList(ctx, repoDir)
	if err != nil {
		return Worktree{}, err
	}
	wt, ok, err := FindWorktree(worktrees, path)
	if err != nil {
		return Worktree{}, err
	}
	if !ok {
		return Worktree{}, fmt.Errorf("created worktree %q was not listed by git", path)
	}
	return wt, nil
}

func (c Client) RemoveWorktree(ctx context.Context, repoDir, path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("worktree path is required")
	}
	_, err := c.run(ctx, repoDir, "worktree", "remove", path)
	return err
}

func (c Client) WorktreeList(ctx context.Context, repoDir string) ([]Worktree, error) {
	out, err := c.run(ctx, repoDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return ParseWorktreeListPorcelainZ(out)
}

func (c Client) LocalBranches(ctx context.Context, repoDir string) ([]string, error) {
	out, err := c.run(ctx, repoDir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		branch := strings.TrimSpace(line)
		if branch != "" {
			branches = append(branches, branch)
		}
	}
	return branches, nil
}

func ParseStatusPorcelainZ(out []byte) (StatusSnapshot, error) {
	status := StatusSnapshot{
		Presence: PresencePresent,
		Tree:     TreeClean,
		Upstream: UpstreamNoUpstream,
	}
	fields := splitNUL(out)
	for i := 0; i < len(fields); i++ {
		record := fields[i]
		if record == "" {
			continue
		}
		if strings.HasPrefix(record, "## ") {
			parseBranchHeader(&status, strings.TrimPrefix(record, "## "))
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return StatusSnapshot{}, fmt.Errorf("invalid status record %q", record)
		}
		code := record[:2]
		file := FileStatus{Code: code, Path: record[3:]}
		if code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C' {
			if i+1 >= len(fields) || fields[i+1] == "" {
				return StatusSnapshot{}, fmt.Errorf("rename/copy status %q missing original path", record)
			}
			file.OrigPath = fields[i+1]
			i++
		}
		status.Files = append(status.Files, file)
		switch code {
		case "??":
			status.Untracked = true
		case "!!":
			status.IgnoredUserFiles = true
		default:
			status.Tree = TreeDirty
			if isConflictCode(code) {
				status.Conflicts = true
			}
		}
	}
	status.Summary = SummarizeStatus(status)
	return status, nil
}

func ParseWorktreeListPorcelainZ(out []byte) ([]Worktree, error) {
	fields := splitNUL(out)
	var worktrees []Worktree
	var current *Worktree
	finish := func() {
		if current != nil {
			worktrees = append(worktrees, *current)
			current = nil
		}
	}
	for _, field := range fields {
		if field == "" {
			finish()
			continue
		}
		key, value, hasValue := strings.Cut(field, " ")
		if key == "worktree" {
			finish()
			if !hasValue || value == "" {
				current = &Worktree{}
				continue
			}
			current = &Worktree{Path: filepath.Clean(value)}
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("worktree field %q appeared before a worktree path", field)
		}
		switch key {
		case "HEAD":
			current.HeadSHA = value
		case "branch":
			current.BranchRef = value
			current.BranchName = shortBranchName(value)
		case "detached":
			current.Detached = true
		case "bare":
			current.Bare = true
		case "locked":
			current.Locked = true
			current.LockReason = value
		case "prunable":
			current.Prunable = true
			current.PruneReason = value
		}
	}
	finish()
	return worktrees, nil
}

func StatusWithoutPaths(status StatusSnapshot, paths ...string) StatusSnapshot {
	ignored := map[string]bool{}
	for _, path := range paths {
		ignored[path] = true
	}
	next := StatusSnapshot{
		Summary:     status.Summary,
		Presence:    status.Presence,
		Tree:        TreeClean,
		Upstream:    status.Upstream,
		Branch:      status.Branch,
		UpstreamRef: status.UpstreamRef,
	}
	if status.Presence != PresencePresent {
		return status
	}
	for _, file := range status.Files {
		if ignored[file.Path] {
			continue
		}
		next.Files = append(next.Files, file)
		switch file.Code {
		case "??":
			next.Untracked = true
		case "!!":
			next.IgnoredUserFiles = true
		default:
			next.Tree = TreeDirty
			if isConflictCode(file.Code) {
				next.Conflicts = true
			}
		}
	}
	next.Summary = SummarizeStatus(next)
	return next
}

func (c Client) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	git := c.gitPath()
	if _, err := exec.LookPath(git); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrGitMissing, git)
	}
	cmd := exec.CommandContext(ctx, git, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), &CommandError{
			Dir:    dir,
			Args:   append([]string(nil), args...),
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}
	return stdout.Bytes(), nil
}

func (c Client) gitPath() string {
	if c.GitPath == "" {
		return "git"
	}
	return c.GitPath
}

func statusWithPresence(presence Presence) StatusSnapshot {
	status := StatusSnapshot{
		Presence: presence,
		Tree:     TreeClean,
		Upstream: UpstreamNoUpstream,
	}
	status.Summary = SummarizeStatus(status)
	return status
}

func parseBranchHeader(status *StatusSnapshot, header string) {
	if strings.HasPrefix(header, "HEAD ") || header == "HEAD" {
		status.Upstream = UpstreamDetached
		return
	}
	if strings.HasPrefix(header, "No commits yet on ") {
		status.Branch = strings.TrimPrefix(header, "No commits yet on ")
		status.Upstream = UpstreamNoUpstream
		return
	}
	branch, rest, hasUpstream := strings.Cut(header, "...")
	status.Branch = branch
	if !hasUpstream {
		status.Upstream = UpstreamNoUpstream
		return
	}
	upstream := rest
	flags := ""
	if idx := strings.Index(rest, " ["); idx >= 0 {
		upstream = rest[:idx]
		flags = strings.TrimSuffix(strings.TrimPrefix(rest[idx:], " ["), "]")
	}
	status.UpstreamRef = upstream
	ahead := strings.Contains(flags, "ahead ")
	behind := strings.Contains(flags, "behind ")
	switch {
	case strings.Contains(flags, "gone"):
		status.Upstream = UpstreamNoUpstream
	case ahead && behind:
		status.Upstream = UpstreamDiverged
	case ahead:
		status.Upstream = UpstreamAhead
	case behind:
		status.Upstream = UpstreamBehind
	default:
		status.Upstream = UpstreamCurrent
	}
}

func isConflictCode(code string) bool {
	switch code {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return strings.Contains(code, "U")
	}
}

func isNotRepository(err error) bool {
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	text := cmdErr.Stderr + "\n" + cmdErr.Stdout
	return strings.Contains(text, "not a git repository") ||
		strings.Contains(text, "not in a git directory") ||
		strings.Contains(text, "not a gitdir")
}

func splitNUL(out []byte) []string {
	if len(out) == 0 {
		return nil
	}
	raw := bytes.Split(out, []byte{0})
	fields := make([]string, 0, len(raw))
	for _, field := range raw {
		fields = append(fields, string(field))
	}
	return fields
}

func shortBranchName(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}
