package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	requireTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	capability, err := Probe(ctx)
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if !capability.Available {
		t.Fatal("Probe reported tmux unavailable")
	}
	if capability.BinaryPath == "" {
		t.Fatal("Probe returned empty binary path")
	}
	if capability.Version == "" {
		t.Fatal("Probe returned empty version")
	}
}

func TestCreateRunnerTargetLifecycle(t *testing.T) {
	adapter := newTestAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessionName := uniqueSessionName(t)

	target, err := adapter.CreateRunnerTarget(ctx, sessionName, "sleep 300")
	if err != nil {
		t.Fatalf("CreateRunnerTarget returned error: %v", err)
	}
	if target.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", target.SessionName, sessionName)
	}
	if target.SessionID == "" {
		t.Fatal("SessionID is empty")
	}
	if target.WindowName != runnerWindow {
		t.Fatalf("WindowName = %q, want %q", target.WindowName, runnerWindow)
	}
	if target.WindowID == "" {
		t.Fatal("WindowID is empty")
	}
	if target.PaneID == "" {
		t.Fatal("PaneID is empty")
	}

	exists, err := adapter.TargetExists(ctx, sessionName)
	if err != nil {
		t.Fatalf("TargetExists returned error: %v", err)
	}
	if !exists {
		t.Fatal("TargetExists returned false for live session")
	}

	identity, err := adapter.ServerIdentity(ctx)
	if err != nil {
		t.Fatalf("ServerIdentity returned error: %v", err)
	}
	if identity.PID <= 0 {
		t.Fatalf("ServerIdentity PID = %d, want positive", identity.PID)
	}
	if identity.StartedAt <= 0 {
		t.Fatalf("ServerIdentity StartedAt = %d, want positive", identity.StartedAt)
	}

	attach, err := adapter.AttachCommand(sessionName)
	if err != nil {
		t.Fatalf("AttachCommand returned error: %v", err)
	}
	wantAttach := []string{"tmux", "-f", "/dev/null", "-S", adapter.SocketPath, "attach-session", "-t", sessionName}
	if !reflect.DeepEqual(attach, wantAttach) {
		t.Fatalf("AttachCommand = %#v, want %#v", attach, wantAttach)
	}

	if err := adapter.KillTarget(ctx, sessionName); err != nil {
		t.Fatalf("KillTarget returned error: %v", err)
	}
	exists, err = adapter.TargetExists(ctx, sessionName)
	if err != nil {
		t.Fatalf("TargetExists after KillTarget returned error: %v", err)
	}
	if exists {
		t.Fatal("TargetExists returned true after KillTarget")
	}
}

func TestCreateRunnerTargetRejectsDuplicateSession(t *testing.T) {
	adapter := newTestAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessionName := uniqueSessionName(t)

	if _, err := adapter.CreateRunnerTarget(ctx, sessionName, "sleep 300"); err != nil {
		t.Fatalf("first CreateRunnerTarget returned error: %v", err)
	}
	if _, err := adapter.CreateRunnerTarget(ctx, sessionName, "sleep 300"); err == nil {
		t.Fatal("second CreateRunnerTarget returned nil error")
	}
}

func TestTargetExistsDoesNotRequireServer(t *testing.T) {
	adapter := newTestAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exists, err := adapter.TargetExists(ctx, uniqueSessionName(t))
	if err != nil {
		t.Fatalf("TargetExists returned error: %v", err)
	}
	if exists {
		t.Fatal("TargetExists returned true without a server")
	}

	_, err = adapter.ServerIdentity(ctx)
	if !errors.Is(err, ErrServerMissing) {
		t.Fatalf("ServerIdentity error = %v, want ErrServerMissing", err)
	}
}

func TestKillTargetReportsMissingTarget(t *testing.T) {
	adapter := newTestAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := adapter.KillTarget(ctx, uniqueSessionName(t))
	if !errors.Is(err, ErrTargetMissing) {
		t.Fatalf("KillTarget error = %v, want ErrTargetMissing", err)
	}
}

func TestSessionNameValidation(t *testing.T) {
	ctx := context.Background()
	adapter := Adapter{}

	for _, name := range []string{"", "bad:name", "bad name", "bad/name"} {
		if _, err := adapter.CreateRunnerTarget(ctx, name, "sleep 1"); err == nil {
			t.Fatalf("CreateRunnerTarget accepted invalid session name %q", name)
		}
		if _, err := adapter.AttachCommand(name); err == nil {
			t.Fatalf("AttachCommand accepted invalid session name %q", name)
		}
		if _, err := adapter.TargetExists(ctx, name); err == nil {
			t.Fatalf("TargetExists accepted invalid session name %q", name)
		}
		if err := adapter.KillTarget(ctx, name); err == nil {
			t.Fatalf("KillTarget accepted invalid session name %q", name)
		}
	}

	if _, err := adapter.CreateRunnerTarget(ctx, "valid-name_1.2", "  "); err == nil {
		t.Fatal("CreateRunnerTarget accepted an empty command")
	}
}

func newTestAdapter(t *testing.T) Adapter {
	t.Helper()
	tmuxPath := requireTmux(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "tmux.sock")

	adapter := Adapter{
		Binary:     "tmux",
		SocketPath: socketPath,
		ConfigPath: "/dev/null",
	}
	t.Cleanup(func() {
		cmd := exec.Command(tmuxPath, "-f", "/dev/null", "-S", socketPath, "kill-server")
		_ = cmd.Run()
	})
	return adapter
}

func requireTmux(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	return path
}

func uniqueSessionName(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	return "agency_test_" + name + "_" + strconv.Itoa(os.Getpid())
}
