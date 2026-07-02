package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agency-two/internal/agency/storage"
	"agency-two/internal/agency/supervisor"
)

func TestProjectInitAndList(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	code := Run([]string{"project", "init", "--project", repo}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Project initialized\nproject:") {
		t.Fatalf("unexpected init output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = Run([]string{"project", "list"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "PROJECT") || !strings.Contains(out.String(), repo) {
		t.Fatalf("unexpected list output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"project", "init", "--project", repo, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("json init code=%d stderr=%s", code, errOut.String())
	}
	for _, blocked := range []string{"projectId", "hostId"} {
		if strings.Contains(out.String(), blocked) {
			t.Fatalf("project json leaked %s:\n%s", blocked, out.String())
		}
	}
	if !strings.Contains(out.String(), `"project": "`) || !strings.Contains(out.String(), `"projectRootWorkspace": "project_root"`) ||
		!strings.Contains(out.String(), `"defaultHost": "local"`) || !strings.Contains(out.String(), `"defaultWorktreeMode": "prompt"`) {
		t.Fatalf("project json missing public fields:\n%s", out.String())
	}
}

func TestProjectAndConfigDeriveReplayKeys(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	t.Chdir(repo)

	for i := 0; i < 2; i++ {
		var out, errOut bytes.Buffer
		if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
			t.Fatalf("init %d code=%d stderr=%s", i, code, errOut.String())
		}
	}
	for i := 0; i < 2; i++ {
		var out, errOut bytes.Buffer
		if code := Run([]string{"config", "set", "defaults.base_ref", "main"}, &out, &errOut); code != 0 {
			t.Fatalf("config %d code=%d stderr=%s", i, code, errOut.String())
		}
	}

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, operation := range []string{"initProject", "setConfig"} {
		var count int
		if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where operation_key=? and state='Completed'`, operation).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s idempotency rows = %d, want 1", operation, count)
		}
	}
}

func TestProjectInitRejectsOutsideGit(t *testing.T) {
	withState(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	code := Run([]string{"project", "init", "--project", t.TempDir()}, &out, &errOut)
	if code != 1 {
		t.Fatalf("code=%d, want 1 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: InvalidProjectRoot") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestProjectCommandsRequireSupervisor(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))

	for _, args := range [][]string{
		{"project", "init", "--project", repo},
		{"project", "list"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 4 {
			t.Fatalf("%v code=%d, want 4 stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
			t.Fatalf("%v stderr:\n%s", args, errOut.String())
		}
	}
}

func TestRemoteHostDispatchesThroughSSH(t *testing.T) {
	withState(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHostAccess(context.Background(), "devbox", "ssh:devbox"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "ssh.args")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsPath+"\nprintf 'remote ok\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"--host", "devbox", "list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("remote code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if strings.TrimSpace(out.String()) != "remote ok" {
		t.Fatalf("remote stdout: %q", out.String())
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "devbox\nagency list --json\n" {
		t.Fatalf("ssh args:\n%s", raw)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--host", "devbox", "send", "ses_abc", "hello; rm -rf /"}, &out, &errOut); code != 0 {
		t.Fatalf("remote quoted code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	raw, err = os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "devbox\nagency send ses_abc 'hello; rm -rf /'\n" {
		t.Fatalf("quoted ssh args:\n%s", raw)
	}
}

func TestMoshHostIsAttachOnly(t *testing.T) {
	withState(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHostAccess(context.Background(), "mosh_box", "mosh:devbox"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "mosh.args")
	if err := os.WriteFile(filepath.Join(dir, "mosh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsPath+"\nprintf 'attached\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"--host", "mosh_box", "list"}, &out, &errOut); code != 1 {
		t.Fatalf("mosh list code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: AttachOnlyMosh") {
		t.Fatalf("mosh list stderr:\n%s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--host", "mosh_box", "attach", "ses_abc123"}, &out, &errOut); code != 0 {
		t.Fatalf("mosh attach code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "devbox\n--\ntmux\nattach-session\n-t\nagency-ses_abc123\n" {
		t.Fatalf("mosh args:\n%s", raw)
	}
}

func TestRemoteHostRejectsUnsafeAliasBeforeExec(t *testing.T) {
	withState(t)
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "ssh.args")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsPath+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"--host", "-oProxyCommand=touch/tmp/pwned", "list"}, &out, &errOut); code != 2 {
		t.Fatalf("unsafe host code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: InvalidHostKey") {
		t.Fatalf("unsafe host stderr:\n%s", errOut.String())
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("ssh was executed or stat failed unexpectedly: %v", err)
	}
}

func TestMoshAttachRejectsUnsafeSessionBeforeExec(t *testing.T) {
	withState(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHostAccess(context.Background(), "mosh_box", "mosh:devbox"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "mosh.args")
	if err := os.WriteFile(filepath.Join(dir, "mosh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsPath+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"--host", "mosh_box", "attach", "ses_bad;tmux"}, &out, &errOut); code != 2 {
		t.Fatalf("unsafe mosh session code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: InvalidSessionHandle") {
		t.Fatalf("unsafe session stderr:\n%s", errOut.String())
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("mosh was executed or stat failed unexpectedly: %v", err)
	}
}

func TestModelAndProfileCommandsUseSupervisor(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	t.Chdir(repo)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"model", "list", "codex", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("model list code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"provider": "codex"`) || !strings.Contains(out.String(), `"key": "gpt-5.4-mini"`) ||
		!strings.Contains(out.String(), `"efforts": [`) || !strings.Contains(out.String(), `"sandboxModes": [`) ||
		!strings.Contains(out.String(), `"approvalPolicies": [`) || !strings.Contains(out.String(), `"availability": "general"`) {
		t.Fatalf("model list output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"model", "list", "codex"}, &out, &errOut); code != 0 {
		t.Fatalf("model list human code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "EFFORTS") || !strings.Contains(out.String(), "AVAILABILITY") || !strings.Contains(out.String(), "minimal,low,medium,high,xhigh") {
		t.Fatalf("model list human output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--json", "--no-color", "model", "list", "codex"}, &out, &errOut); code != 0 {
		t.Fatalf("global json model list code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"provider": "codex"`) || !strings.Contains(out.String(), `"key": "gpt-5.5"`) {
		t.Fatalf("global json model list output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"model", "list", "--json", "codex"}, &out, &errOut); code != 0 {
		t.Fatalf("model list flag-before-provider code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"provider": "codex"`) || !strings.Contains(out.String(), `"key": "gpt-5.4-mini"`) {
		t.Fatalf("model list flag-before-provider output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "create", "fast_codex", "--provider", "codex", "--model", "gpt-5.4-mini", "--effort", "minimal", "--sandbox", "read-only", "--approval-policy", "never"}, &out, &errOut); code != 0 {
		t.Fatalf("profile create code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Agent profile saved\nprofile: fast_codex\nprovider: codex") {
		t.Fatalf("profile create output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--project", repo, "--profile", "fast_codex", "new", "codex", "--command-preview", "--title", "Global preview"}, &out, &errOut); code != 0 {
		t.Fatalf("global project/profile preview code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "cwd: "+repo) || !strings.Contains(out.String(), "model: gpt-5.4-mini") {
		t.Fatalf("global project/profile preview output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "create", "default_codex", "--provider", "codex"}, &out, &errOut); code != 0 {
		t.Fatalf("default profile create code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "create", "review_claude", "--provider", "claude", "--permission-mode", "plan"}, &out, &errOut); code != 0 {
		t.Fatalf("claude profile create code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("profile list code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"key": "fast_codex"`, `"provider": "codex"`, `"model": "gpt-5.4-mini"`, `"effort": "minimal"`, `"sandboxMode": "read-only"`, `"approvalPolicy": "never"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("profile list missing %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{`"key": "review_claude"`, `"provider": "claude"`, `"permissionMode": "plan"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("claude profile list missing %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{`"key": "default_codex"`, `"model": "gpt-5.5"`, `"effort": "high"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("default profile list missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "set", "fast_codex", "--effort", "high"}, &out, &errOut); code != 0 {
		t.Fatalf("profile set code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Agent profile saved\nprofile: fast_codex\nprovider: codex") {
		t.Fatalf("profile set output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("profile list after set code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"key": "fast_codex"`, `"provider": "codex"`, `"model": "gpt-5.4-mini"`, `"effort": "high"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("updated profile list missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "set", "fast_codex", "--model", "gpt-5.4"}, &out, &errOut); code != 0 {
		t.Fatalf("profile set model code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("profile list after model set code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"key": "fast_codex"`, `"model": "gpt-5.4"`, `"effort": "high"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("model-only update missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "set-default", "fast_codex"}, &out, &errOut); code != 0 {
		t.Fatalf("profile set-default code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Default profile set\nproject:") || !strings.Contains(out.String(), "profile: fast_codex") {
		t.Fatalf("profile set-default output:\n%s", out.String())
	}
}

func TestCatalogCommandsRequireSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))

	for _, args := range [][]string{
		{"model", "list"},
		{"profile", "list"},
		{"profile", "create", "fast_codex", "--provider", "codex"},
		{"profile", "set", "fast_codex", "--provider", "codex"},
		{"profile", "set-default", "fast_codex"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 4 {
			t.Fatalf("%v code=%d, want 4 stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
			t.Fatalf("%v stderr:\n%s", args, errOut.String())
		}
	}
}

func TestNewCommandPreview(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	extraDir := filepath.Join(repo, "extra dir")
	code := Run([]string{"new", "codex", "--cwd", repo, "--command-preview", "--model", "gpt-5.4-mini", "--effort", "minimal", "--add-dir", extraDir, "Fix reader race"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"Command preview\n",
		"cwd: " + repo,
		"workspace: project root",
		"provider: codex",
		"model: gpt-5.4-mini",
		"effort: minimal",
		"argv: codex --model gpt-5.4-mini -c model_reasoning_effort=minimal --sandbox workspace-write --ask-for-approval on-request --add-dir '" + extraDir + "'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preview missing %q:\n%s", want, got)
		}
	}
}

func TestNewCommandPreviewDoesNotCreateProject(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	var out, errOut bytes.Buffer
	code := Run([]string{"new", "codex", "--cwd", repo, "--command-preview", "Fix reader race"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("preview code=%d, want 1 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: ProjectNotFound") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projects, err := store.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("preview created projects: %+v", projects)
	}
}

func TestNewDoesNotCreateProject(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	code := Run([]string{"new", "codex", "--cwd", repo, "Fix reader race"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("new code=%d, want 1 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: ProjectNotFound") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projects, err := store.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("new created projects: %+v", projects)
	}
}

func TestNewRequiresSupervisor(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"new", "codex", "--cwd", repo, "Fix reader race"}, &out, &errOut); code != 4 {
		t.Fatalf("new code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestNewAndListHumanAndJSON(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	t.Chdir(repo)

	out.Reset()
	errOut.Reset()
	code := Run([]string{"new", "claude", "--cwd", repo, "--title", "Audit callbacks"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Agent session started\nsession: ses_") {
		t.Fatalf("unexpected new output:\n%s", out.String())
	}
	// The displayed tmux target must be the real session name (agency-<session>),
	// usable directly with tmux. A divergent "agency:<session>" (session:window)
	// form points at a nonexistent session named "agency".
	if !strings.Contains(out.String(), "tmux: agency-ses_") {
		t.Fatalf("new output tmux target is not the real session name:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = Run([]string{"list", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("list json code=%d stderr=%s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{`"runStatus": "Live"`, `"summary": "Clean"`, `"summary": "StopRequired"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("json output missing %q:\n%s", want, got)
		}
	}
	// workspaceKey is a public schema key (WorktreeSummary exposes it too) and the
	// CLI renders the WORKSPACE column from it, so it is intentionally present.
	// path stays out of the session list payload (the working directory is not
	// needed there and is kept minimal).
	if !strings.Contains(got, `"workspaceKey": "project_root"`) {
		t.Fatalf("list json should expose workspaceKey for the WORKSPACE column:\n%s", got)
	}
	if strings.Contains(got, `"path"`) {
		t.Fatalf("list json leaked \"path\":\n%s", got)
	}
}

func TestPublicJSONDoesNotExposePrivateIDs(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	assertNoPrivateJSONIDs(t, out.String())
	t.Chdir(repo)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-public-json", "--title", "Public JSON", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	assertNoPrivateJSONIDs(t, out.String())
	session := firstJSONField(t, out.String(), "session")
	for _, args := range [][]string{
		{"list", "--json"},
		{"status", "--json", session},
		{"events", "--json", session},
		{"worktree", "list", "--json"},
		{"worktree", "status", "--json", "wt-public-json"},
		{"model", "list", "--json"},
		{"profile", "list", "--json"},
		{"project", "list", "--json"},
		{"host", "list", "--json"},
		{"doctor", "--json"},
	} {
		out.Reset()
		errOut.Reset()
		if code := Run(args, &out, &errOut); code != 0 {
			t.Fatalf("%v code=%d stderr=%s", args, code, errOut.String())
		}
		assertNoPrivateJSONIDs(t, out.String())
	}
}

func TestNewSendsInitialPrompt(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	extraDir := filepath.Join(repo, "extra")
	highEntropy := "abc123def456ghi789jkl012mno345pq"
	if code := Run([]string{"new", "codex", "--cwd", repo, "--json", "--add-dir", extraDir, "--env", "AGENCY_SECRET=topsecret", "--env", "AGENCY_MODE=dev", "--env", "SESSION_HINT=" + highEntropy, "Fix reader race"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"--add-dir"`) || !strings.Contains(out.String(), `"`+extraDir+`"`) {
		t.Fatalf("new json missing add-dir argv:\n%s", out.String())
	}
	session := firstJSONField(t, out.String(), "session")

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var rawInput, rawDelivery, rawEnv string
	if err := store.DB.QueryRowContext(context.Background(), `select i.input_ref_json, i.delivery_json, r.launch_env_json
		from run_input_events i
		join agent_runs r on r.run_id=i.run_id
		join agent_sessions s on s.session_id=r.session_id
		where s.session_key=? and i.input_seq=1`, session).Scan(&rawInput, &rawDelivery, &rawEnv); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawEnv, "topsecret") || strings.Contains(rawEnv, highEntropy) ||
		!strings.Contains(rawEnv, "AGENCY_SECRET") || !strings.Contains(rawEnv, "SESSION_HINT") ||
		!strings.Contains(rawEnv, "AGENCY_MODE") || !strings.Contains(rawEnv, "dev") {
		t.Fatalf("launch env was not redacted: %s", rawEnv)
	}
	var ref struct {
		BytesBase64 string `json:"bytesBase64"`
	}
	if err := json.Unmarshal([]byte(rawInput), &ref); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(ref.BytesBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "Fix reader race\n" || !strings.Contains(rawDelivery, `"state":"Accepted"`) {
		t.Fatalf("input=%q delivery=%s", string(decoded), rawDelivery)
	}
}

func TestNewDerivesReplayKey(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Replay launch", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("first new code=%d stderr=%s", code, errOut.String())
	}
	firstSession := firstJSONField(t, out.String(), "session")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Replay launch", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("second new code=%d stderr=%s", code, errOut.String())
	}
	if secondSession := firstJSONField(t, out.String(), "session"); secondSession != firstSession {
		t.Fatalf("second session = %q, want %q", secondSession, firstSession)
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var sessions, keys int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from agent_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where operation_key='startSession' and state='Completed'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || keys != 1 {
		t.Fatalf("sessions=%d keys=%d, want 1/1", sessions, keys)
	}
}

func TestRunDerivesReplayKey(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Replay run", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"run", session}, &out, &errOut); code != 0 {
		t.Fatalf("first run code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "run: run_2") {
		t.Fatalf("first run output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"run", session}, &out, &errOut); code != 0 {
		t.Fatalf("second run code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "run: run_2") {
		t.Fatalf("second run output:\n%s", out.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var runs, keys int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from agent_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where operation_key='startRun' and state='Completed'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if runs != 2 || keys != 1 {
		t.Fatalf("runs=%d keys=%d, want 2/1", runs, keys)
	}
}

func TestNewUsesExplicitProfile(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"profile", "create", "fast_codex", "--provider", "codex", "--model", "gpt-5.4-mini", "--effort", "minimal"}, &out, &errOut); code != 0 {
		t.Fatalf("profile create code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--profile", "fast_codex", "--json", "--title", "Profile launch"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"model": "gpt-5.4-mini"`) || !strings.Contains(out.String(), `"effort": "minimal"`) {
		t.Fatalf("new json did not use explicit profile:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "claude", "--cwd", repo, "--profile", "fast_codex", "--command-preview"}, &out, &errOut); code != 1 {
		t.Fatalf("mismatch code=%d, want 1 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "profile fast_codex is for provider codex, not claude") {
		t.Fatalf("mismatch stderr:\n%s", errOut.String())
	}
}

func TestNewCapturesProviderOutput(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--json", "--title", "Output capture"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := store.DB.QueryContext(context.Background(), `select c.content_ref_json
			from runner_output_chunks c
			join agent_runs r on r.run_id=c.run_id
			join agent_sessions s on s.session_id=r.session_id
			where s.session_key=? order by c.chunk_seq`, session)
		if err == nil {
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var ref struct {
					BytesBase64 string `json:"bytesBase64"`
				}
				if err := json.Unmarshal([]byte(raw), &ref); err != nil {
					t.Fatal(err)
				}
				decoded, err := base64.StdEncoding.DecodeString(ref.BytesBase64)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(decoded), "ready") {
					_ = rows.Close()
					return
				}
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider output was not captured for %s", session)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisorRestartAdoptsRealTmuxRunner(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	socket := filepath.Join(t.TempDir(), "supervisor.sock")
	stopSupervisor := startAppSupervisorAt(t, socket)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Restart adoption", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	if session == "" {
		t.Fatalf("session missing:\n%s", out.String())
	}

	stopSupervisor()
	startAppSupervisorAt(t, socket)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor", "--supervisor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"adoptedRunners": 1`) {
		t.Fatalf("supervisor did not adopt runner after restart:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "--json", session}, &out, &errOut); code != 0 {
		t.Fatalf("status code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"runStatus": "Live"`) {
		t.Fatalf("status after restart:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"stop", session}, &out, &errOut); code != 0 {
		t.Fatalf("stop code=%d stderr=%s", code, errOut.String())
	}
}

func TestListRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, &out, &errOut); code != 4 {
		t.Fatalf("list code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestListShowsObservedDirtyGitStatus(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Dirty status"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"list"}, &out, &errOut); code != 0 {
		t.Fatalf("list code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Dirty") {
		t.Fatalf("list did not show dirty git status:\n%s", out.String())
	}
	for _, want := range []string{"MODEL", "EFFORT", "CLOSE", "gpt-5.5", "high", "Stop required"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list output missing %q:\n%s", want, out.String())
		}
	}
}

func TestListShowsLostTmuxTargetForBoundRun(t *testing.T) {
	withState(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), createCommittedRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Missing tmux", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRunner(context.Background(), run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "missing-runner.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, &out, &errOut); code != 0 {
		t.Fatalf("list code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Lost tmux target") {
		t.Fatalf("list did not show lost tmux target:\n%s", out.String())
	}
}

func TestDoctorReportsAndRepairsLostTmuxTarget(t *testing.T) {
	withState(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), createCommittedRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Repair tmux", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRunner(context.Background(), run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "missing-runner.sock")); err != nil {
		t.Fatal(err)
	}
	jsonRun, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Repair tmux json", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRunner(context.Background(), jsonRun.RunID, jsonRun.TmuxTargetID, filepath.Join(t.TempDir(), "missing-runner-json.sock")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"doctor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, errOut.String())
	}
	repairKey := "lost-tmux-target:" + run.Session
	for _, want := range []string{
		"Doctor observations",
		"state: " + os.Getenv("AGENCY_STATE_DB"),
		"issue: TmuxTargetMissing",
		"session: " + run.Session,
		"summary: Expected tmux target is missing.",
		"repair: " + repairKey,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor json code=%d stderr=%s", code, errOut.String())
	}
	doctor := decodeObject(t, out.String())
	if stringAt(t, doctor, "stateDb") != os.Getenv("AGENCY_STATE_DB") {
		t.Fatalf("doctor stateDb = %v", doctor["stateDb"])
	}
	issue := findDoctorIssue(t, doctor, "TmuxTargetMissing", "session", run.Session)
	target := objectAt(t, issue, "target")
	if stringAt(t, issue, "severity") != "Blocker" || stringAt(t, issue, "repair") != repairKey || stringAt(t, target, "workspace") == "" || stringAt(t, issue, "summary") == "" || stringAt(t, issue, "detail") == "" {
		t.Fatalf("doctor issue shape = %+v", issue)
	}
	for _, leak := range []string{`"label"`, `"sessionId"`, `"runId"`, `"workspaceId"`, "Lost tmux target"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("doctor json leaked %q:\n%s", leak, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"repair", repairKey}, &out, &errOut); code != 0 {
		t.Fatalf("repair code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Repair completed\nrepair: "+repairKey+"\nsession: "+run.Session+"\nstatus: Repaired") {
		t.Fatalf("repair output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	jsonRepairKey := "lost-tmux-target:" + jsonRun.Session
	if code := Run([]string{"--json", "repair", jsonRepairKey}, &out, &errOut); code != 0 {
		t.Fatalf("repair json code=%d stderr=%s", code, errOut.String())
	}
	repair := decodeObject(t, out.String())
	if stringAt(t, repair, "key") != jsonRepairKey || stringAt(t, repair, "session") != jsonRun.Session || stringAt(t, repair, "status") != "Repaired" {
		t.Fatalf("repair json = %+v", repair)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "--json", run.Session}, &out, &errOut); code != 0 {
		t.Fatalf("status code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"runStatus": "Failed"`) {
		t.Fatalf("status output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"events", "--json", run.Session}, &out, &errOut); code != 0 {
		t.Fatalf("events code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"eventType": "RepairCompleted"`) {
		t.Fatalf("events output:\n%s", out.String())
	}
}

func TestUnknownCommandUsesErrorShape(t *testing.T) {
	withState(t)
	var out, errOut bytes.Buffer
	code := Run([]string{"bogus"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	want := "Could not parse command.\nreason: Invalid command input\ntarget: bogus\ndetail: Unknown command.\nnext: Run agency.\n"
	if errOut.String() != want {
		t.Fatalf("stderr mismatch\nwant:\n%s\ngot:\n%s", want, errOut.String())
	}
}

func TestRenameStopCloseLifecycle(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "claude", "--cwd", repo, "--title", "Old title", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"session": "ses_`) || strings.Contains(out.String(), `"Session"`) {
		t.Fatalf("new json casing:\n%s", out.String())
	}
	for _, blocked := range []string{"sessionId", "runId", "tmuxTargetId"} {
		if strings.Contains(out.String(), blocked) {
			t.Fatalf("new json leaked %s:\n%s", blocked, out.String())
		}
	}
	session := firstJSONField(t, out.String(), "session")
	if session == "" {
		t.Fatalf("could not find session in output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"rename", session, "New", "title"}, &out, &errOut); code != 0 {
		t.Fatalf("rename code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Session renamed\nsession: "+session+"\ntitle: New title") {
		t.Fatalf("rename output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"run", session}, &out, &errOut); code != 0 {
		t.Fatalf("run code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Run requested\nsession: "+session+"\nrun: run_2") {
		t.Fatalf("run output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"send", session, "hello", "runner"}, &out, &errOut); code != 0 {
		t.Fatalf("send code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Input accepted\nsession: "+session) {
		t.Fatalf("send output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdinWriter.Write([]byte("stdin runner\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = stdinReader
	code := Run([]string{"send", session}, &out, &errOut)
	os.Stdin = originalStdin
	if err := stdinReader.Close(); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("stdin send code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Input accepted\nsession: "+session) {
		t.Fatalf("stdin send output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"attach", session}, &out, &errOut); code != 0 {
		t.Fatalf("attach code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "tmux") || !strings.Contains(out.String(), "attach-session") {
		t.Fatalf("attach output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"stop", session}, &out, &errOut); code != 0 {
		t.Fatalf("stop code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Stop requested\nsession: "+session+"\nrun: run_2") ||
		!strings.Contains(out.String(), "Run stopped\nsession: "+session+"\nrun: run_2") {
		t.Fatalf("stop output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "claude", "--cwd", repo, "--title", "Kill title", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new kill-session code=%d stderr=%s", code, errOut.String())
	}
	killedSession := firstJSONField(t, out.String(), "session")
	killedRun := firstJSONField(t, out.String(), "run")
	if killedSession == "" || killedRun == "" {
		t.Fatalf("kill-session output missing handles:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"kill", killedSession}, &out, &errOut); code != 0 {
		t.Fatalf("kill code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Run killed\nsession: "+killedSession+"\nrun: "+killedRun) {
		t.Fatalf("kill output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "--json", killedSession}, &out, &errOut); code != 0 {
		t.Fatalf("killed status code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"runStatus": "Killed"`) {
		t.Fatalf("killed status:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"events", "--json", killedSession}, &out, &errOut); code != 0 {
		t.Fatalf("killed events code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"eventType": "RunKilled"`) {
		t.Fatalf("killed events:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"close", session}, &out, &errOut); code != 0 {
		t.Fatalf("close code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Session closed\nsession: "+session) {
		t.Fatalf("close output:\n%s", out.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var completed int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from close_attempts where completed_at is not null`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completed close attempts = %d, want 1", completed)
	}
	for _, operation := range []string{"renameSession", "stopRun", "closeSession"} {
		var keys int
		if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where operation_key=? and state='Completed'`, operation).Scan(&keys); err != nil {
			t.Fatal(err)
		}
		if keys != 1 {
			t.Fatalf("%s completed idempotency keys = %d, want 1", operation, keys)
		}
	}
}

func TestCloseTerminalSessionsDerivesReplayKey(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Terminal close", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(context.Background(), run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"close", "--terminal"}, &out, &errOut); code != 0 {
		t.Fatalf("first close terminal code=%d stderr=%s", code, errOut.String())
	}
	first := out.String()
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"close", "--terminal"}, &out, &errOut); code != 0 {
		t.Fatalf("second close terminal code=%d stderr=%s", code, errOut.String())
	}
	if out.String() != first || !strings.Contains(first, "count: 1") {
		t.Fatalf("close terminal replay mismatch\nfirst:\n%s\nsecond:\n%s", first, out.String())
	}
	store, err = storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var keys int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where operation_key='closeTerminalSessions' and state='Completed'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 1 {
		t.Fatalf("closeTerminalSessions idempotency keys = %d, want 1", keys)
	}
}

func TestSendReplayKeyDoesNotDoubleSend(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Replay send", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"send", "--replay-key", "send-replay-1", session, "hello", "runner"}, &out, &errOut); code != 0 {
		t.Fatalf("send code=%d stderr=%s", code, errOut.String())
	}
	first := out.String()
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"send", "--replay-key", "send-replay-1", session, "hello", "runner"}, &out, &errOut); code != 0 {
		t.Fatalf("send replay code=%d stderr=%s", code, errOut.String())
	}
	if out.String() != first || !strings.Contains(out.String(), "input: 1") {
		t.Fatalf("replay output mismatch\nfirst:\n%s\nsecond:\n%s", first, out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"send", "--replay-key", "send-replay-1", session, "different"}, &out, &errOut); code != 1 {
		t.Fatalf("send conflict code=%d, want 1 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "ReplayKeyConflict") {
		t.Fatalf("conflict stderr:\n%s", errOut.String())
	}

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var inputs, keys int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*)
		from run_input_events i
		join agent_runs r on r.run_id=i.run_id
		join agent_sessions s on s.session_id=r.session_id
		where s.session_key=?`, session).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where replay_key='send-replay-1' and state='Completed'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if inputs != 1 || keys != 1 {
		t.Fatalf("inputs=%d keys=%d, want 1/1", inputs, keys)
	}
}

func TestSendWithoutReplayKeyDeliversEachInput(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Derived send", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"send", session, "hello", "runner"}, &out, &errOut); code != 0 {
		t.Fatalf("send code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "input: 1") {
		t.Fatalf("first send should be input 1:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	// An identical send WITHOUT an explicit --replay-key is a distinct logical
	// action and must be delivered as a new input (seq 2), never swallowed as a
	// content-hash replay. Idempotency is opt-in via --replay-key.
	if code := Run([]string{"send", session, "hello", "runner"}, &out, &errOut); code != 0 {
		t.Fatalf("second send code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "input: 2") {
		t.Fatalf("identical send without replay key should deliver as input 2:\n%s", out.String())
	}
}

func TestRenameRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"rename", "ses_missing", "New title"}, &out, &errOut); code != 4 {
		t.Fatalf("rename code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestCloseLiveSessionPersistsBlocker(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "claude", "--cwd", repo, "--title", "Live close", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"close", session}, &out, &errOut); code != 3 {
		t.Fatalf("close code=%d, want 3 stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "The run is still live.") {
		t.Fatalf("close stderr:\n%s", errOut.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var blockers int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from close_blockers where blocker='SessionStillRunning'`).Scan(&blockers); err != nil {
		t.Fatal(err)
	}
	if blockers != 1 {
		t.Fatalf("SessionStillRunning blockers = %d, want 1", blockers)
	}
}

func TestCloseSessionRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"close", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("close code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestStopRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"stop", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("stop code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestKillRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"kill", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("kill code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestSendRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"send", "ses_missing", "hello"}, &out, &errOut); code != 4 {
		t.Fatalf("send code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestRunRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"run", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("run code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestStatusShowsPersistedRecentOutput(t *testing.T) {
	withState(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	repo := createCommittedRepo(t)
	project, err := store.EnsureProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Output status", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendOutputChunk(context.Background(), run.RunID, 1, "Stdout", []byte("hello from runner\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"status", run.Session}, &out, &errOut); code != 0 {
		t.Fatalf("status code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "recent output:\nhello from runner\n") {
		t.Fatalf("status output:\n%s", out.String())
	}
	for _, want := range []string{"model: gpt-5.5", "effort: high", "close: Stop required"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "--json", run.Session}, &out, &errOut); code != 0 {
		t.Fatalf("status json code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"session": "`+run.Session+`"`) || !strings.Contains(out.String(), `"runStatus": "Starting"`) {
		t.Fatalf("status json output:\n%s", out.String())
	}
	status := decodeObject(t, out.String())
	git := objectAt(t, status, "git")
	closeState := objectAt(t, status, "close")
	if stringAt(t, git, "summary") != "Clean" || stringAt(t, git, "presence") != "Present" || stringAt(t, git, "tree") != "Clean" || stringAt(t, git, "upstream") != "NoUpstream" {
		t.Fatalf("status git = %+v", git)
	}
	if boolAt(t, git, "untracked") || boolAt(t, git, "ignoredUserFiles") || boolAt(t, git, "conflicts") {
		t.Fatalf("status git booleans = %+v", git)
	}
	if boolAt(t, closeState, "closable") || stringAt(t, closeState, "summary") != "StopRequired" || !arrayContainsString(arrayAt(t, closeState, "blockers"), "SessionStillRunning") {
		t.Fatalf("status close = %+v", closeState)
	}
	if len(arrayAt(t, status, "recentOutput")) != 1 || len(arrayAt(t, status, "events")) == 0 || !boolAt(t, objectAt(t, status, "diff"), "available") {
		t.Fatalf("status detail shape = %+v", status)
	}
	if stringAt(t, objectAt(t, status, "tmux"), "target") == "" || stringAt(t, objectAt(t, status, "launch"), "provider") != "codex" {
		t.Fatalf("status tmux/launch = %+v %+v", status["tmux"], status["launch"])
	}
	if strings.Contains(out.String(), "Stop required") {
		t.Fatalf("status json used human label:\n%s", out.String())
	}
}

func TestEventsShowsPersistedLifecycle(t *testing.T) {
	withState(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	repo := createCommittedRepo(t)
	project, err := store.EnsureProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Event status", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	seq, err := store.AddInputEvent(context.Background(), run.RunID, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInputAccepted(context.Background(), run.RunID, seq); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTerminalOutcome(context.Background(), run.RunID, "ProviderExited", `{"outcome":"ProviderExited","exitCode":0}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"events", run.Session}, &out, &errOut); code != 0 {
		t.Fatalf("events code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"TIME", "Session created", "Run requested", "Input accepted", "Provider process exited"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("events output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"events", "--json", run.Session}, &out, &errOut); code != 0 {
		t.Fatalf("events json code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"eventType": "InputAccepted"`, `"kind": "Run"`, `"session": "` + run.Session + `"`, `"run": "run_1"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("events json missing %q:\n%s", want, out.String())
		}
	}
	for _, blocked := range []string{`"label"`, `"eventId"`, `"runId"`, `"sessionId"`} {
		if strings.Contains(out.String(), blocked) {
			t.Fatalf("events json leaked %q:\n%s", blocked, out.String())
		}
	}
}

func TestStatusRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("status code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestDiffRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"diff", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("diff code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestAttachRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"attach", "ses_missing"}, &out, &errOut); code != 4 {
		t.Fatalf("attach code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestNewManagedWorktreeLaunch(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-test", "--title", "Worktree test"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("new worktree code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "workspace: wks_") || !strings.Contains(out.String(), "(wt-test)") {
		t.Fatalf("new output missing worktree:\n%s", out.String())
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-test")
	if _, err := os.Stat(filepath.Join(worktreePath, ".agency-worktree")); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("original checkout changed or missing: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "list", "--json", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("worktree list json code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"workspace": "wks_`, `"workspaceKey": "wt-test"`, `"managedWorktree": true`, `"summary": "StopRequired"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("worktree list json missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), `"workspace": "wt-test"`) {
		t.Fatalf("worktree list json used key as handle:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "list", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("worktree list human code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"WORKTREE", "HANDLE", "GIT", "CLOSE", "SESSIONS", "MARKER", "DIFF", "wt-test", "Stop required", "ok", "available"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("worktree list human missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "status", "--json", "wt-test"}, &out, &errOut); code != 0 {
		t.Fatalf("worktree status code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"workspace": "wks_`, `"workspaceKey": "wt-test"`, `"project": "`, `"path": "` + worktreePath + `"`, `"baseSha": "`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("worktree status missing %q:\n%s", want, out.String())
		}
	}
	worktreeStatus := decodeObject(t, out.String())
	git := objectAt(t, worktreeStatus, "git")
	closeState := objectAt(t, worktreeStatus, "close")
	if stringAt(t, git, "summary") != "Clean" || stringAt(t, git, "presence") != "Present" || stringAt(t, git, "tree") != "Clean" || stringAt(t, git, "upstream") != "NoUpstream" {
		t.Fatalf("worktree git = %+v", git)
	}
	if boolAt(t, closeState, "closable") || stringAt(t, closeState, "summary") != "StopRequired" || !arrayContainsString(arrayAt(t, closeState, "blockers"), "SessionStillRunning") {
		t.Fatalf("worktree close = %+v", closeState)
	}
	if len(arrayAt(t, worktreeStatus, "sessions")) != 1 || !boolAt(t, objectAt(t, worktreeStatus, "marker"), "matches") || !boolAt(t, objectAt(t, worktreeStatus, "diff"), "available") {
		t.Fatalf("worktree status shape = %+v", worktreeStatus)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "status", "wt-test"}, &out, &errOut); code != 0 {
		t.Fatalf("worktree status human code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"worktree: wks_", "path: " + worktreePath, "branch: agency/wt-test", "git: Clean", "close: Stop required", "marker: true", "diff: available"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("worktree status human missing %q:\n%s", want, out.String())
		}
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var safetyRuns int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from safety_check_runs`).Scan(&safetyRuns); err != nil {
		t.Fatal(err)
	}
	if safetyRuns != 0 {
		t.Fatalf("worktree read commands recorded %d safety checks", safetyRuns)
	}
}

func TestDefaultWorktreeModeLaunch(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	t.Chdir(repo)
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "defaults.worktree_mode", "always"}, &out, &errOut); code != 0 {
		t.Fatalf("config worktree mode code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "get", "defaults.worktree_mode"}, &out, &errOut); code != 0 {
		t.Fatalf("config get worktree mode code=%d stderr=%s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "always" {
		t.Fatalf("worktree mode output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--worktree-name", "wt-default", "--title", "Default worktree", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new default worktree code=%d stderr=%s", code, errOut.String())
	}
	started := decodeObject(t, out.String())
	if stringAt(t, started, "workspaceKey") != "wt-default" {
		t.Fatalf("default worktree launch = %+v", started)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-default", ".agency-worktree")); err != nil {
		t.Fatalf("default worktree marker missing: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "--json", stringAt(t, started, "session")}, &out, &errOut); code != 0 {
		t.Fatalf("status default worktree code=%d stderr=%s", code, errOut.String())
	}
	launch := objectAt(t, decodeObject(t, out.String()), "launch")
	if stringAt(t, launch, "sandboxMode") != "workspace-write" || stringAt(t, launch, "approvalPolicy") != "on-request" {
		t.Fatalf("default launch controls = %+v", launch)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", stringAt(t, started, "session")}, &out, &errOut); code != 0 {
		t.Fatalf("status default worktree human code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"model: gpt-5.5", "effort: high", "sandbox: workspace-write", "approval-policy: on-request"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status human missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"stop", stringAt(t, started, "session")}, &out, &errOut); code != 0 {
		t.Fatalf("stop default worktree code=%d stderr=%s", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--no-worktree", "--title", "Project root override", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new no-worktree override code=%d stderr=%s", code, errOut.String())
	}
	override := decodeObject(t, out.String())
	if stringAt(t, override, "workspaceKey") != "project_root" || stringAt(t, override, "path") != repo {
		t.Fatalf("no-worktree override launch = %+v", override)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"stop", stringAt(t, override, "session")}, &out, &errOut); code != 0 {
		t.Fatalf("stop override code=%d stderr=%s", code, errOut.String())
	}
}

func TestNewManagedWorktreeRejectsUnsafeName(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "../escape", "--title", "Bad worktree"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("new unsafe worktree code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "InvalidWorktreeName") {
		t.Fatalf("stderr missing InvalidWorktreeName:\n%s", errOut.String())
	}
	escaped := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "..", "escape")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("unsafe worktree path exists or stat failed unexpectedly: %v", err)
	}
}

func TestNewManagedWorktreeValidatesLaunchBeforePublishing(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-invalid", "--model", "bad-model", "--title", "Invalid model"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("new invalid model code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "UnsupportedModel") {
		t.Fatalf("stderr missing UnsupportedModel:\n%s", errOut.String())
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-invalid")
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("invalid launch created worktree path or stat failed unexpectedly: %v", err)
	}

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var workspaces, managed, active int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from workspaces where workspace_key='wt-invalid'`).Scan(&workspaces); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from managed_worktrees mw join workspaces w on w.workspace_id=mw.workspace_id where w.workspace_key='wt-invalid'`).Scan(&managed); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from active_worktrees aw join managed_worktrees mw on mw.managed_worktree_id=aw.managed_worktree_id join workspaces w on w.workspace_id=mw.workspace_id where w.workspace_key='wt-invalid'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if workspaces != 0 || managed != 0 || active != 0 {
		t.Fatalf("invalid launch published workspace state: workspaces=%d managed=%d active=%d", workspaces, managed, active)
	}
}

func TestWorktreeCloseRejectsNonManagedPath(t *testing.T) {
	withState(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "agency-tests@example.invalid")
	runGit(t, repo, "config", "user.name", "Agency Tests")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	externalPath := filepath.Join(t.TempDir(), "external-worktree")
	runGit(t, repo, "worktree", "add", "-b", "external", externalPath, "HEAD")
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "close", externalPath}, &out, &errOut); code != 1 {
		t.Fatalf("close code=%d, want 1 stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: ManagedWorktreeNotFound") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(externalPath, "README.md")); err != nil {
		t.Fatalf("external worktree was removed: %v", err)
	}
}

func TestManagedWorktreeCloseBlocksAndPersistsBlockers(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-blocked", "--title", "Blocked close"}, &out, &errOut); code != 0 {
		t.Fatalf("new worktree code=%d stderr=%s", code, errOut.String())
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-blocked")
	if err := os.WriteFile(filepath.Join(worktreePath, "scratch.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "close", "wt-blocked"}, &out, &errOut); code != 3 {
		t.Fatalf("close code=%d, want 3 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"Worktree close blocked.", "Untracked files blocked", "Stop required", "No files were removed."} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("blocked output missing %q:\n%s", want, errOut.String())
		}
	}
	for _, raw := range []string{"[WorktreeHasUntrackedFiles]", "[SessionStillRunning]"} {
		if strings.Contains(errOut.String(), raw) {
			t.Fatalf("blocked output leaked %q:\n%s", raw, errOut.String())
		}
	}
	if _, err := os.Stat(filepath.Join(worktreePath, "scratch.txt")); err != nil {
		t.Fatalf("blocked close removed file: %v", err)
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var attempts, blockers, safetyRuns, safetyFindings int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from close_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from close_blockers where blocker in ('WorktreeHasUntrackedFiles', 'SessionStillRunning')`).Scan(&blockers); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from safety_check_runs where check_kind='WorktreeClose' and completed_at is not null`).Scan(&safetyRuns); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from safety_check_findings where blocker in ('WorktreeHasUntrackedFiles', 'SessionStillRunning')`).Scan(&safetyFindings); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || blockers != 2 || safetyRuns != 1 || safetyFindings != 2 {
		t.Fatalf("attempts=%d blockers=%d safetyRuns=%d safetyFindings=%d", attempts, blockers, safetyRuns, safetyFindings)
	}
	var workspaceID, targetJSON, evidenceJSON, payloadJSON string
	if err := store.DB.QueryRowContext(context.Background(), `select workspace_id from workspaces where workspace_key='wt-blocked'`).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	workspace := storage.Handle("wks_", workspaceID)
	if err := store.DB.QueryRowContext(context.Background(), `select target_json from close_attempts`).Scan(&targetJSON); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select evidence_json from close_blockers where blocker='WorktreeHasUntrackedFiles'`).Scan(&evidenceJSON); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select payload_json from events where event_type='CloseBlocked'`).Scan(&payloadJSON); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"target": targetJSON, "event": payloadJSON} {
		if !strings.Contains(raw, workspace) || strings.Contains(raw, `"workspace":"wt-blocked"`) {
			t.Fatalf("%s JSON uses wrong workspace shape: %s", name, raw)
		}
	}
	if !strings.Contains(evidenceJSON, `"gitSummary":"Untracked"`) || !strings.Contains(evidenceJSON, `"branch":"agency/wt-blocked"`) {
		t.Fatalf("close blocker evidence did not persist safety evidence: %s", evidenceJSON)
	}
}

func TestWorktreeCloseJSONUsesSchemaShape(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-json", "--title", "JSON close"}, &out, &errOut); code != 0 {
		t.Fatalf("new worktree code=%d stderr=%s", code, errOut.String())
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-json")
	if err := os.WriteFile(filepath.Join(worktreePath, "scratch.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "close", "--json", "wt-json"}, &out, &errOut); code != 3 {
		t.Fatalf("close code=%d, want 3 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{
		`"closeAttempt": "cls_`,
		`"target": {`,
		`"workspace": "wks_`,
		`"workspaceKey": "wt-json"`,
		`"closed": false`,
		`"blocker": "WorktreeHasUntrackedFiles"`,
		`"evidence": {`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("close json missing %q:\n%s", want, out.String())
		}
	}
}

func TestManagedWorktreeCloseBlocksGitDataLossScenarios(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "ignore logs")

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
	makeWorktree := func(name string, upstream bool) string {
		path := filepath.Join(project.ManagedWorktreeRoot, name)
		if err := os.MkdirAll(project.ManagedWorktreeRoot, 0755); err != nil {
			t.Fatal(err)
		}
		branch := "agency/" + name
		runGit(t, repo, "worktree", "add", "-b", branch, path, "HEAD")
		if upstream {
			runGit(t, path, "branch", "--set-upstream-to", "main", branch)
		}
		headSHA := strings.TrimSpace(runGitOutput(t, path, "rev-parse", "HEAD"))
		workspaceID, err := store.AddManagedWorkspace(context.Background(), project.ID, name, path, branch, "HEAD", baseSHA, headSHA)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".agency-worktree"), []byte(workspaceID+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishManagedWorkspace(context.Background(), workspaceID); err != nil {
			t.Fatal(err)
		}
		return path
	}
	dirtyPath := makeWorktree("wt-dirty", true)
	if err := os.WriteFile(filepath.Join(dirtyPath, "README.md"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ignoredPath := makeWorktree("wt-ignored", true)
	if err := os.WriteFile(filepath.Join(ignoredPath, "scratch.log"), []byte("ignored\n"), 0644); err != nil {
		t.Fatal(err)
	}
	noUpstreamPath := makeWorktree("wt-no-upstream", false)
	unpushedPath := makeWorktree("wt-unpushed", true)
	if err := os.WriteFile(filepath.Join(unpushedPath, "agent.txt"), []byte("agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, unpushedPath, "add", "agent.txt")
	runGit(t, unpushedPath, "commit", "-m", "agent commit")
	conflictPath := makeWorktree("wt-conflict", true)
	if err := os.WriteFile(filepath.Join(conflictPath, "README.md"), []byte("worktree\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, conflictPath, "commit", "-am", "worktree edit")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "commit", "-am", "main edit")
	cmd := exec.Command("git", "merge", "main")
	cmd.Dir = conflictPath
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("merge unexpectedly succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	startAppSupervisor(t)
	for _, tc := range []struct {
		workspace string
		path      string
		blocker   string
		label     string
	}{
		{"wt-dirty", dirtyPath, "WorktreeDirty", "Dirty worktree blocked"},
		{"wt-ignored", ignoredPath, "WorktreeHasIgnoredUserFiles", "Ignored user files blocked"},
		{"wt-no-upstream", noUpstreamPath, "NoRemoteTrackingProof", "Unpushed commits blocked"},
		{"wt-unpushed", unpushedPath, "BranchHasUnpushedCommits", "Unpushed commits blocked"},
		{"wt-conflict", conflictPath, "WorktreeHasConflicts", "Conflicts blocked"},
	} {
		var out, errOut bytes.Buffer
		if code := Run([]string{"worktree", "close", tc.workspace}, &out, &errOut); code != 3 {
			t.Fatalf("%s close code=%d, want 3 stdout=%s stderr=%s", tc.workspace, code, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), tc.label) || strings.Contains(errOut.String(), "["+tc.blocker+"]") || !strings.Contains(errOut.String(), "No files were removed.") {
			t.Fatalf("%s close stderr:\n%s", tc.workspace, errOut.String())
		}
		if _, err := os.Stat(tc.path); err != nil {
			t.Fatalf("%s close removed or lost path: %v", tc.workspace, err)
		}
	}
	store, err = storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, blocker := range []string{"WorktreeDirty", "WorktreeHasIgnoredUserFiles", "NoRemoteTrackingProof", "BranchHasUnpushedCommits", "WorktreeHasConflicts"} {
		var count int
		if err := store.DB.QueryRowContext(context.Background(), `select count(*) from close_blockers where blocker=?`, blocker).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatalf("close blocker %s was not recorded", blocker)
		}
	}
}

func TestManagedWorktreeCloseBlocksStartingRun(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(project.ManagedWorktreeRoot, "wt-starting")
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0700); err != nil {
		t.Fatal(err)
	}
	branch := "agency/wt-starting"
	runGit(t, repo, "worktree", "add", "-b", branch, worktreePath, "HEAD")
	base := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
	workspaceID, err := store.AddManagedWorkspace(context.Background(), project.ID, "wt_starting", worktreePath, branch, "HEAD", base, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".agency-worktree"), []byte(workspaceID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(context.Background(), workspaceID); err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: workspaceID, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Starting run", WorkingDirectory: worktreePath, Argv: []string{"codex"}, Env: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"worktree", "close", "wt_starting"}, &out, &errOut); code != 3 {
		t.Fatalf("close code=%d, want 3 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "Stop required") || strings.Contains(errOut.String(), "[SessionStillRunning]") {
		t.Fatalf("blocked output did not include starting run:\n%s", errOut.String())
	}
	store, err = storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var blockers, findings int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from close_blockers where blocker='SessionStillRunning'`).Scan(&blockers); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from safety_check_findings where blocker='SessionStillRunning'`).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if blockers != 1 || findings != 1 {
		t.Fatalf("SessionStillRunning blockers=%d findings=%d, want 1/1", blockers, findings)
	}
}

func TestManagedWorktreeCloseRemovesCleanStoppedOwnedWorktree(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-clean", "--title", "Clean close", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new worktree code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	if session == "" {
		t.Fatalf("session missing from output:\n%s", out.String())
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-clean")
	runGit(t, worktreePath, "branch", "--set-upstream-to", "main", "agency/wt-clean")

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"stop", session}, &out, &errOut); code != 0 {
		t.Fatalf("stop code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"close", session}, &out, &errOut); code != 0 {
		t.Fatalf("session close code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "close", "wt-clean"}, &out, &errOut); code != 0 {
		t.Fatalf("worktree close code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	firstClose := out.String()
	if !strings.Contains(out.String(), "Managed worktree removed\nworkspace: wks_") || !strings.Contains(out.String(), "(wt-clean)") || !strings.Contains(out.String(), "branch: agency/wt-clean remains") {
		t.Fatalf("close output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "close", "wt-clean"}, &out, &errOut); code != 0 {
		t.Fatalf("worktree close replay code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if out.String() != firstClose {
		t.Fatalf("worktree close replay mismatch\nfirst:\n%s\nsecond:\n%s", firstClose, out.String())
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists or stat failed with unexpected error: %v", err)
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var removed, active int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from removed_worktrees`).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from active_worktrees`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if removed != 1 || active != 0 {
		t.Fatalf("removed=%d active=%d", removed, active)
	}
	var keys int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from idempotency_keys where operation_key='closeWorktree' and state='Completed'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 1 {
		t.Fatalf("closeWorktree idempotency keys = %d, want 1", keys)
	}
	var workspaceID, targetJSON, payloadJSON string
	if err := store.DB.QueryRowContext(context.Background(), `select workspace_id from workspaces where workspace_key='wt-clean'`).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	workspace := storage.Handle("wks_", workspaceID)
	if err := store.DB.QueryRowContext(context.Background(), `select target_json from close_attempts where target_json like '%"Workspace"%'`).Scan(&targetJSON); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select payload_json from events where event_type='ManagedWorktreeRemoved'`).Scan(&payloadJSON); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"target": targetJSON, "event": payloadJSON} {
		if !strings.Contains(raw, workspace) || strings.Contains(raw, "managedWorktreeId") || strings.Contains(raw, `"workspace":"wt-clean"`) {
			t.Fatalf("%s JSON uses wrong workspace shape: %s", name, raw)
		}
	}
}

func TestSupervisorResumesClosingWorktreeOnStartup(t *testing.T) {
	withState(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := createCommittedRepo(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	name := "wt-replay"
	path := filepath.Join(project.ManagedWorktreeRoot, name)
	if err := os.MkdirAll(project.ManagedWorktreeRoot, 0755); err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
	runGit(t, repo, "worktree", "add", "-b", "agency/"+name, path, "HEAD")
	runGit(t, path, "branch", "--set-upstream-to", "main", "agency/"+name)
	headSHA := strings.TrimSpace(runGitOutput(t, path, "rev-parse", "HEAD"))
	workspaceID, err := store.AddManagedWorkspace(context.Background(), project.ID, name, path, "agency/"+name, "HEAD", baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".agency-worktree"), []byte(workspaceID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(context.Background(), workspaceID); err != nil {
		t.Fatal(err)
	}
	wt, err := store.ActiveManagedWorktree(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWorktreeRemoval(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	startAppSupervisor(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("closing worktree path still exists or stat failed: %v", err)
	}
	store, err = storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var closing, removed int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from closing_worktrees`).Scan(&closing); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from removed_worktrees`).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if closing != 0 || removed != 1 {
		t.Fatalf("closing=%d removed=%d, want closing=0 removed=1", closing, removed)
	}
}

func TestManagedWorktreeCloseBlocksStructuralDamage(t *testing.T) {
	withState(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectForPath(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	makeWorktree := func(name string) string {
		path := filepath.Join(project.ManagedWorktreeRoot, name)
		if err := os.MkdirAll(project.ManagedWorktreeRoot, 0755); err != nil {
			t.Fatal(err)
		}
		baseSHA := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
		runGit(t, repo, "worktree", "add", "-b", "agency/"+name, path, "HEAD")
		runGit(t, path, "branch", "--set-upstream-to", "main", "agency/"+name)
		headSHA := strings.TrimSpace(runGitOutput(t, path, "rev-parse", "HEAD"))
		workspaceID, err := store.AddManagedWorkspace(context.Background(), project.ID, name, path, "agency/"+name, "HEAD", baseSHA, headSHA)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".agency-worktree"), []byte(workspaceID+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishManagedWorkspace(context.Background(), workspaceID); err != nil {
			t.Fatal(err)
		}
		return path
	}
	markerPath := makeWorktree("wt-marker-damage")
	if err := os.WriteFile(filepath.Join(markerPath, ".agency-worktree"), []byte("wrong\n"), 0600); err != nil {
		t.Fatal(err)
	}
	missingPath := makeWorktree("wt-missing-damage")
	runGit(t, repo, "worktree", "remove", "--force", missingPath)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor json code=%d stderr=%s", code, errOut.String())
	}
	doctor := decodeObject(t, out.String())
	markerIssue := findDoctorIssue(t, doctor, "OwnershipMarkerMismatch", "workspaceKey", "wt-marker-damage")
	missingIssue := findDoctorIssue(t, doctor, "GitWorktreeMissing", "workspaceKey", "wt-missing-damage")
	for _, issue := range []map[string]any{markerIssue, missingIssue} {
		if stringAt(t, issue, "severity") != "Blocker" || stringAt(t, issue, "summary") == "" || stringAt(t, issue, "detail") == "" || stringAt(t, objectAt(t, issue, "target"), "workspace") == "" {
			t.Fatalf("doctor issue shape = %+v", issue)
		}
	}
	if strings.Contains(out.String(), `"workspaceId"`) || strings.Contains(out.String(), `"managedWorktreeId"`) {
		t.Fatalf("doctor json leaked private ids:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{
		"Doctor observations",
		"issue: OwnershipMarkerMismatch",
		"workspace: " + stringAt(t, objectAt(t, markerIssue, "target"), "workspace"),
		"path: " + filepath.Join(markerPath, ".agency-worktree"),
		"summary: Managed worktree ownership marker does not match storage.",
		"issue: GitWorktreeMissing",
		"path: " + missingPath,
		"summary: Expected git worktree is missing.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out.String())
		}
	}

	for _, tc := range []struct {
		workspace string
		blocker   string
	}{
		{"wt-marker-damage", "OwnershipMarkerMismatch"},
		{"wt-missing-damage", "GitWorktreeMissing"},
	} {
		out.Reset()
		errOut.Reset()
		if code := Run([]string{"worktree", "close", tc.workspace}, &out, &errOut); code != 3 {
			t.Fatalf("%s close code=%d, want 3 stdout=%s stderr=%s", tc.workspace, code, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "Repair required") || strings.Contains(errOut.String(), "["+tc.blocker+"]") || !strings.Contains(errOut.String(), "No files were removed.") {
			t.Fatalf("%s close stderr:\n%s", tc.workspace, errOut.String())
		}
	}
}

func TestManagedWorktreeCloseBlocksMissingTmuxTarget(t *testing.T) {
	withState(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	repo := createCommittedRepo(t)
	// A runner the supervisor never proved live this process lifetime is declared
	// heartbeat-expired once the monotonic TTL elapses; a tiny TTL makes that
	// deterministic instead of waiting the production default.
	t.Setenv("AGENCY_HEARTBEAT_TTL_MS", "1")
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectForPath(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	name := "wt-missing-tmux"
	path := filepath.Join(project.ManagedWorktreeRoot, name)
	if err := os.MkdirAll(project.ManagedWorktreeRoot, 0755); err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
	runGit(t, repo, "worktree", "add", "-b", "agency/"+name, path, "HEAD")
	runGit(t, path, "branch", "--set-upstream-to", "main", "agency/"+name)
	headSHA := strings.TrimSpace(runGitOutput(t, path, "rev-parse", "HEAD"))
	workspaceID, err := store.AddManagedWorkspace(context.Background(), project.ID, name, path, "agency/"+name, "HEAD", baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".agency-worktree"), []byte(workspaceID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishManagedWorkspace(context.Background(), workspaceID); err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: workspaceID, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Missing tmux target", WorkingDirectory: path, Argv: []string{"codex"}, Env: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRunner(context.Background(), run.RunID, run.TmuxTargetID, filepath.Join(t.TempDir(), "missing-runner.sock")); err != nil {
		t.Fatal(err)
	}
	// The supervisor never adopts this bound runner (its socket does not exist),
	// so with the tiny monotonic TTL it is heartbeat-expired by close time.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"worktree", "close", name}, &out, &errOut); code != 3 {
		t.Fatalf("close code=%d, want 3 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"Stop required", "Repair required", "Runner heartbeat expired.", "Expected tmux target is missing.", "No files were removed."} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("close output missing %q:\n%s", want, errOut.String())
		}
	}
	for _, raw := range []string{"[SessionStillRunning]", "[TmuxTargetMissing]", "[RunnerHeartbeatExpired]"} {
		if strings.Contains(errOut.String(), raw) {
			t.Fatalf("close output leaked %q:\n%s", raw, errOut.String())
		}
	}
}

func TestSessionDiffUsesManagedWorktreeBaseSHA(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--worktree", "--worktree-name", "wt-diff", "--title", "Diff base", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new worktree code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	if session == "" {
		t.Fatalf("session missing from output:\n%s", out.String())
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-diff")
	if err := os.WriteFile(filepath.Join(worktreePath, "agent.txt"), []byte("committed by agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktreePath, "add", "agent.txt")
	runGit(t, worktreePath, "commit", "-m", "agent change")

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"diff", session}, &out, &errOut); code != 0 {
		t.Fatalf("diff code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "agent.txt") || !strings.Contains(out.String(), "committed by agent") {
		t.Fatalf("diff did not include committed agent change:\n%s", out.String())
	}
}

func TestSessionDiffUsesProjectBaseRef(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	base := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "agent.txt"), []byte("committed after base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "agent.txt")
	runGit(t, repo, "commit", "-m", "agent change")
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	t.Chdir(repo)
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "defaults.base_ref", base}, &out, &errOut); code != 0 {
		t.Fatalf("base config code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Project diff", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"diff", session}, &out, &errOut); code != 0 {
		t.Fatalf("diff code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "agent.txt") || !strings.Contains(out.String(), "committed after base") {
		t.Fatalf("project diff did not use configured base ref:\n%s", out.String())
	}
}

func TestWorktreeCloseRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"worktree", "close", "wt-missing"}, &out, &errOut); code != 4 {
		t.Fatalf("worktree close code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestWorktreeReadCommandsRequireSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	for _, args := range [][]string{
		{"worktree", "list"},
		{"worktree", "status", "wt-missing"},
		{"worktree", "diff", "wt-missing"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 4 {
			t.Fatalf("%v code=%d, want 4 stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
			t.Fatalf("%v stderr:\n%s", args, errOut.String())
		}
	}
}

func TestMaintenanceCommands(t *testing.T) {
	withState(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	t.Chdir(repo)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "defaults.profile", "claude_default"}, &out, &errOut); code != 0 {
		t.Fatalf("config set code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Config value set\nkey: defaults.profile\nvalue: claude_default") {
		t.Fatalf("config output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "get", "defaults.profile"}, &out, &errOut); code != 0 {
		t.Fatalf("config get profile code=%d stderr=%s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "claude_default" {
		t.Fatalf("config get profile output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "defaults.base_ref", "main"}, &out, &errOut); code != 0 {
		t.Fatalf("config set base ref code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Config value set\nkey: defaults.base_ref\nvalue: main") {
		t.Fatalf("config base output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "get", "defaults.base_ref"}, &out, &errOut); code != 0 {
		t.Fatalf("config get base ref code=%d stderr=%s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "main" {
		t.Fatalf("config get base ref output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "hosts.devbox.access", "ssh:devbox"}, &out, &errOut); code != 0 {
		t.Fatalf("host config set code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Config value set\nkey: hosts.devbox.access\nvalue: ssh:devbox") {
		t.Fatalf("host config output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "get", "hosts.devbox.access"}, &out, &errOut); code != 0 {
		t.Fatalf("host config get code=%d stderr=%s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "ssh:devbox" {
		t.Fatalf("host config get output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "defaults.host", "devbox"}, &out, &errOut); code != 0 {
		t.Fatalf("default host set code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Config value set\nkey: defaults.host\nvalue: devbox") {
		t.Fatalf("default host output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "get", "defaults.host"}, &out, &errOut); code != 0 {
		t.Fatalf("default host get code=%d stderr=%s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "devbox" {
		t.Fatalf("default host get output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "set", "defaults.worktree_mode", "never"}, &out, &errOut); code != 0 {
		t.Fatalf("config set worktree mode code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Config value set\nkey: defaults.worktree_mode\nvalue: never") {
		t.Fatalf("worktree mode config output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"config", "get", "defaults.worktree_mode"}, &out, &errOut); code != 0 {
		t.Fatalf("config get worktree mode code=%d stderr=%s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "never" {
		t.Fatalf("worktree mode config get output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"host", "list"}, &out, &errOut); code != 0 {
		t.Fatalf("host list code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "devbox") || !strings.Contains(out.String(), "Ssh") {
		t.Fatalf("host list output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"host", "install-unit"}, &out, &errOut); code != 0 {
		t.Fatalf("host install-unit code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ExecStart=") || !strings.Contains(out.String(), "WantedBy=default.target") {
		t.Fatalf("install-unit output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"host", "install-unit", "--launchd"}, &out, &errOut); code != 0 {
		t.Fatalf("host install-unit --launchd code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "com.agency.supervisor") {
		t.Fatalf("install-unit launchd output:\n%s", out.String())
	}

	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var sources, revisions int
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from config_sources where source_key='cli' and path='agency config set'`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRowContext(context.Background(), `select count(*) from config_revisions`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || revisions < 6 {
		t.Fatalf("config sources=%d revisions=%d, want 1/>=6", sources, revisions)
	}
	effective, err := store.LatestEffectiveConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defaults := objectAt(t, effective, "defaults")
	if stringAt(t, defaults, "profile") != "claude_default" || stringAt(t, defaults, "baseRef") != "main" ||
		stringAt(t, defaults, "host") != "devbox" || stringAt(t, defaults, "worktreeMode") != "Never" {
		t.Fatalf("effective defaults = %+v", defaults)
	}
	devbox := objectAt(t, objectAt(t, effective, "hosts"), "devbox")
	access := objectAt(t, devbox, "access")
	if stringAt(t, access, "mode") != "Ssh" || stringAt(t, access, "hostAlias") != "devbox" {
		t.Fatalf("effective devbox access = %+v", access)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"host", "list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("host list json code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{`"host": "devbox"`, `"mode": "Ssh"`, `"hostAlias": "devbox"`, `"socketForwarding": true`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("host json missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"prune"}, &out, &errOut); code != 0 {
		t.Fatalf("prune code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"State pruned", "idempotency keys:", "output chunks:", "status snapshots:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("prune output missing %q:\n%s", want, out.String())
		}
	}

	exportPath := filepath.Join(t.TempDir(), "agency-export.db")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"export", exportPath}, &out, &errOut); code != 0 {
		t.Fatalf("export code=%d stderr=%s", code, errOut.String())
	}
	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("export missing: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"repair", "missing"}, &out, &errOut); code != 1 {
		t.Fatalf("repair code=%d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "reason: Repair failed") {
		t.Fatalf("repair stderr:\n%s", errOut.String())
	}
}

func TestMaintenanceCommandsRequireSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))

	for _, args := range [][]string{
		{"config", "set", "defaults.profile", "claude_default"},
		{"host", "list"},
		{"prune"},
		{"export", filepath.Join(t.TempDir(), "agency-export.db")},
		{"repair", "lost-tmux-target:ses_missing"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 4 {
			t.Fatalf("%v code=%d, want 4 stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
			t.Fatalf("%v stderr:\n%s", args, errOut.String())
		}
	}
}

func TestDefaultCommandRendersSupervisorDashboard(t *testing.T) {
	withState(t)
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.EnsureProject(context.Background(), createCommittedRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _, _, _, err := store.DefaultProfileRevision(context.Background(), project.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunIntent(context.Background(), storage.LaunchIntent{
		ProjectID: project.ID, WorkspaceID: project.ProjectRootWorkspace, ProfileRevisionID: rev, HostID: project.HostID,
		Title: "Dashboard session", WorkingDirectory: project.RootPath, Argv: []string{"codex"}, Env: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run(nil, &out, &errOut); code != 0 {
		t.Fatalf("default command code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"Agency  project:", "SESSION", "Dashboard session", "Starting", "codex"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, out.String())
		}
	}
}

func TestTUIActionDispatcherUsesCommandOwners(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	t.Chdir(repo)

	state := tuiState{view: "dashboard"}
	action := dispatchTUIAction(context.Background(), defaultSupervisorSocket(), "new codex --cwd "+repo+" --worktree --worktree-name wt-tui --title TUI --json", &state)
	if action.quit || len(action.attachArgv) != 0 {
		t.Fatalf("new action = %+v", action)
	}
	session := firstJSONField(t, state.notice, "session")
	if session == "" {
		t.Fatalf("new action did not start a session:\n%s", state.notice)
	}
	worktreePath := filepath.Join(filepath.Dir(repo), ".agency-worktrees", filepath.Base(repo), "wt-tui")
	runGit(t, worktreePath, "branch", "--set-upstream-to", "main", "agency/wt-tui")

	action = dispatchTUIAction(context.Background(), defaultSupervisorSocket(), "open "+session, &state)
	if len(action.attachArgv) == 0 || action.attachArgv[0] != "tmux" {
		t.Fatalf("open action = %+v notice=%s", action, state.notice)
	}

	dispatchTUIAction(context.Background(), defaultSupervisorSocket(), "status "+session, &state)
	if state.view != "session" || state.selectedSession != session {
		t.Fatalf("status action state = %+v", state)
	}

	dispatchTUIAction(context.Background(), defaultSupervisorSocket(), "stop "+session, &state)
	if !strings.Contains(state.notice, "Run stopped") {
		t.Fatalf("stop action notice:\n%s", state.notice)
	}
	dispatchTUIAction(context.Background(), defaultSupervisorSocket(), "close "+session, &state)
	if !strings.Contains(state.notice, "Session closed") {
		t.Fatalf("close action notice:\n%s", state.notice)
	}
	dispatchTUIAction(context.Background(), defaultSupervisorSocket(), "worktree close wt-tui", &state)
	if !strings.Contains(state.notice, "Managed worktree removed") {
		t.Fatalf("worktree close action notice:\n%s", state.notice)
	}
}

func TestDefaultCommandRequiresSupervisor(t *testing.T) {
	withState(t)
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	var out, errOut bytes.Buffer
	if code := Run(nil, &out, &errOut); code != 4 {
		t.Fatalf("default command code=%d, want 4 stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "reason: RemoteSupervisorUnavailable") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestDoctorSupervisorHealth(t *testing.T) {
	withState(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"doctor", "--supervisor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor --supervisor code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"status": "Live"`) || !strings.Contains(out.String(), `"socketPath": "`) {
		t.Fatalf("doctor supervisor output:\n%s", out.String())
	}
}

func TestDoctorReportsNoIssues(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"doctor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Doctor observations\nstate: "+os.Getenv("AGENCY_STATE_DB")+"\nissues: none") {
		t.Fatalf("doctor output:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor json code=%d stderr=%s", code, errOut.String())
	}
	if issues := arrayAt(t, decodeObject(t, out.String()), "issues"); len(issues) != 0 {
		t.Fatalf("doctor json issues = %+v", issues)
	}
}

func TestDoctorReportsOrphanedAgencyBranch(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	runGit(t, repo, "branch", "agency/orphan")
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, errOut.String())
	}
	issue := findDoctorIssue(t, decodeObject(t, out.String()), "OrphanedAgencyBranch", "branch", "agency/orphan")
	target := objectAt(t, issue, "target")
	if stringAt(t, target, "project") == "" {
		t.Fatalf("orphan branch target = %+v", target)
	}
	if !strings.Contains(runGitOutput(t, repo, "branch", "--list", "agency/orphan"), "agency/orphan") {
		t.Fatal("doctor removed orphaned agency branch")
	}
}

func TestDoctorReportsStoredLaunchEnvSecret(t *testing.T) {
	withState(t)
	withLaunchTools(t)
	repo := createCommittedRepo(t)
	startAppSupervisor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"project", "init", "--project", repo}, &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"new", "codex", "--cwd", repo, "--title", "Env doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("new code=%d stderr=%s", code, errOut.String())
	}
	session := firstJSONField(t, out.String(), "session")
	store, err := storage.Open(context.Background(), os.Getenv("AGENCY_STATE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(context.Background(), `update agent_runs set launch_env_json='{"set":{"OPENAI_API_KEY":{"value":"sk-abc123def456ghi789jkl012mno345pq","source":"command"}},"inheritedNames":[],"redacted":[]}'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, errOut.String())
	}
	issue := findDoctorIssue(t, decodeObject(t, out.String()), "LaunchEnvSecretStored", "env", "OPENAI_API_KEY")
	target := objectAt(t, issue, "target")
	if stringAt(t, target, "session") != session || stringAt(t, target, "run") != "run_1" {
		t.Fatalf("env issue target = %+v", target)
	}
}

func startAppSupervisor(t *testing.T) func() {
	t.Helper()
	return startAppSupervisorAt(t, filepath.Join(t.TempDir(), "supervisor.sock"))
}

func startAppSupervisorAt(t *testing.T, socket string) func() {
	t.Helper()
	t.Setenv("AGENCY_SUPERVISOR_SOCKET", socket)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	cfg := supervisor.Config{StateDB: os.Getenv("AGENCY_STATE_DB"), SocketPath: socket}
	// Tests may shrink the monotonic heartbeat TTL so an unproven runner is
	// declared lost promptly; the config service owns this in production.
	if raw := os.Getenv("AGENCY_HEARTBEAT_TTL_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil {
			cfg.HeartbeatTTL = time.Duration(ms) * time.Millisecond
		}
	}
	go func() {
		errs <- supervisor.Serve(ctx, cfg)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("supervisor exit: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("supervisor did not stop")
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := supervisor.HealthCheck(context.Background(), socket); err == nil {
			return stop
		}
		select {
		case err := <-errs:
			t.Fatalf("supervisor exited before it was ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor did not become ready")
	return stop
}

func firstJSONField(t *testing.T, body, field string) string {
	t.Helper()
	needle := `"` + field + `": "`
	start := strings.Index(body, needle)
	if start < 0 {
		return ""
	}
	start += len(needle)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatalf("unterminated field %s in %s", field, body)
	}
	return body[start : start+end]
}

func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("decode json: %v\n%s", err, body)
	}
	return data
}

func objectAt(t *testing.T, row map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := row[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in %+v", key, row)
	}
	return value
}

func arrayAt(t *testing.T, row map[string]any, key string) []any {
	t.Helper()
	value, ok := row[key].([]any)
	if !ok {
		t.Fatalf("%s is not an array in %+v", key, row)
	}
	return value
}

func stringAt(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	value, ok := row[key].(string)
	if !ok {
		t.Fatalf("%s is not a string in %+v", key, row)
	}
	return value
}

func boolAt(t *testing.T, row map[string]any, key string) bool {
	t.Helper()
	value, ok := row[key].(bool)
	if !ok {
		t.Fatalf("%s is not a bool in %+v", key, row)
	}
	return value
}

func assertNoPrivateJSONIDs(t *testing.T, body string) {
	t.Helper()
	for _, blocked := range []string{
		`"projectId"`, `"repositoryId"`, `"hostId"`, `"providerId"`, `"providerModelId"`,
		`"profileId"`, `"profileRevisionId"`, `"workspaceId"`, `"managedWorktreeId"`,
		`"tmuxServerId"`, `"tmuxTargetId"`, `"sessionId"`, `"runId"`, `"runnerBindingId"`,
		`"eventId"`, `"statusSubjectId"`, `"configSourceId"`, `"configRevisionId"`,
	} {
		if strings.Contains(body, blocked) {
			t.Fatalf("public JSON leaked private id key %s:\n%s", blocked, body)
		}
	}
}

func findDoctorIssue(t *testing.T, report map[string]any, code, targetKey, targetValue string) map[string]any {
	t.Helper()
	for _, raw := range arrayAt(t, report, "issues") {
		issue, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("doctor issue is not an object: %+v", raw)
		}
		if stringAt(t, issue, "code") != code {
			continue
		}
		target := objectAt(t, issue, "target")
		if stringAt(t, target, targetKey) == targetValue {
			return issue
		}
	}
	t.Fatalf("doctor issue %s for %s=%s not found in %+v", code, targetKey, targetValue, report)
	return nil
}

func arrayContainsString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func withState(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENCY_STATE_DB", filepath.Join(dir, "agency.db"))
	// Isolate config discovery so the supervisor loads the built-in default
	// config, not any config.toml on the developer's machine.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
}

func withLaunchTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	runnerBin := filepath.Join(dir, "agency-runner")
	build := exec.Command("go", "build", "-o", runnerBin, "./cmd/agency-runner")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agency-runner: %v\n%s", err, string(out))
	}
	for _, name := range []string{"claude", "codex"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'ready\\n'\nsleep 60\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENCY_RUNNER_BIN", runnerBin)
	socket := filepath.Join(dir, "tmux.sock")
	t.Setenv("AGENCY_TMUX_SOCKET", socket)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run()
	})
}

func createCommittedRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "agency-tests@example.invalid")
	runGit(t, repo, "config", "user.name", "Agency Tests")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return string(out)
}
