package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSource(t *testing.T, name, body string) Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return Source{Key: name, Path: path, Priority: 10}
}

func TestLoadConvertsIngressVocabularyToOwnedEnums(t *testing.T) {
	src := writeSource(t, "config.toml", `
version = 1

[defaults]
worktree_mode = "always"

[hosts.devbox]
display_name = "Devbox"
access = { mode = "ssh", host_alias = "devbox", socket_forwarding = true }
tmux_server = "default"

[notifications.channels.desk]
type = "desktop"
events = ["RunFailed"]
`)
	cfg, err := Load([]Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.WorktreeMode != "Always" {
		t.Fatalf("worktree_mode = %q, want Always", cfg.Defaults.WorktreeMode)
	}
	host, ok := cfg.Hosts["devbox"]
	if !ok || host.Access.Mode != "Ssh" || !host.Access.SocketForwarding {
		t.Fatalf("devbox host = %+v", host)
	}
	if cfg.Notifications["desk"].Type != "Desktop" {
		t.Fatalf("notification type = %q, want Desktop", cfg.Notifications["desk"].Type)
	}
	// The default local host and default profiles survive the merge.
	if _, ok := cfg.Hosts["local"]; !ok {
		t.Fatal("default local host was dropped")
	}
	if cfg.Profiles["codex_default"].Model != "gpt-5.5" {
		t.Fatalf("codex_default = %+v", cfg.Profiles["codex_default"])
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	src := writeSource(t, "config.toml", `
version = 1

[defaults]
worktree_mode = "prompt"
nonsense_key = "boom"
`)
	if _, err := Load([]Source{src}); err == nil {
		t.Fatal("expected unknown-key error")
	}
}

func TestLoadRejectsUnknownEnumValue(t *testing.T) {
	src := writeSource(t, "config.toml", `
version = 1

[defaults]
worktree_mode = "sometimes"
`)
	if _, err := Load([]Source{src}); err == nil {
		t.Fatal("expected unknown worktree_mode error")
	}
}

func TestLoadHigherPriorityWins(t *testing.T) {
	base := writeSource(t, "base.toml", "version = 1\n[ui]\nrefresh_ms = 1000\n")
	base.Priority = 10
	over := writeSource(t, "override.toml", "version = 1\n[ui]\nrefresh_ms = 250\n")
	over.Priority = 100
	cfg, err := Load([]Source{over, base})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.RefreshMs != 250 {
		t.Fatalf("refresh_ms = %d, want 250 (higher priority wins)", cfg.UI.RefreshMs)
	}
}

func TestMergeDoesNotSilentlyDisableSecurity(t *testing.T) {
	// A higher-priority source that omits [security] must NOT clobber the
	// default-true toggles to false: absent is not "= false". This guards the
	// peer-credential and tunnel-token checks from a silent security downgrade.
	base := writeSource(t, "base.toml", "version = 1\n")
	base.Priority = 10
	over := writeSource(t, "override.toml", "version = 1\n[ui]\nrefresh_ms = 250\n")
	over.Priority = 100
	cfg, err := Load([]Source{over, base})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Security.SocketPeerCredentialCheck || !cfg.Security.TCPTunnelRequiresToken || !cfg.Security.TCPTunnelLoopbackOnly {
		t.Fatalf("security toggles were silently disabled by an unrelated override: %+v", cfg.Security)
	}
}

func TestMergeHonorsExplicitSecurityFalse(t *testing.T) {
	// An explicit `= false` is honored (unlike a merely-absent key). Only the
	// declared toggle changes; the others keep their default-true value.
	base := writeSource(t, "base.toml", "version = 1\n")
	base.Priority = 10
	over := writeSource(t, "override.toml", "version = 1\n[security]\ntcp_tunnel_loopback_only = false\n")
	over.Priority = 100
	cfg, err := Load([]Source{over, base})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.TCPTunnelLoopbackOnly {
		t.Fatal("explicit tcp_tunnel_loopback_only=false was not honored")
	}
	if !cfg.Security.SocketPeerCredentialCheck || !cfg.Security.TCPTunnelRequiresToken {
		t.Fatalf("undeclared security toggles were disturbed: %+v", cfg.Security)
	}
}

func TestMergeHonorsExplicitSuspendResumeResetFalse(t *testing.T) {
	// suspend_resume_reset defaults true and must be overridable to false; it was
	// previously dropped from the timing merge entirely.
	base := writeSource(t, "base.toml", "version = 1\n")
	base.Priority = 10
	over := writeSource(t, "override.toml", "version = 1\n[timing]\nsuspend_resume_reset = false\n")
	over.Priority = 100
	cfg, err := Load([]Source{over, base})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timing.SuspendResumeReset {
		t.Fatal("explicit suspend_resume_reset=false was not honored")
	}
}

func TestLoadExpandsPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	t.Setenv("AGENCY_TEST_LOGROOT", "/var/agency-logs")
	src := writeSource(t, "paths.toml", "version = 1\n[paths]\nstate_dir = \"~/agency-state\"\nlog_dir = \"$AGENCY_TEST_LOGROOT/logs\"\nruntime_dir = \"relative/run\"\n")
	cfg, err := Load([]Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "agency-state"); cfg.Paths.StateDir != want {
		t.Errorf("state_dir = %q, want %q (tilde expanded)", cfg.Paths.StateDir, want)
	}
	if cfg.Paths.LogDir != "/var/agency-logs/logs" {
		t.Errorf("log_dir = %q, want env-expanded /var/agency-logs/logs", cfg.Paths.LogDir)
	}
	if !filepath.IsAbs(cfg.Paths.RuntimeDir) {
		t.Errorf("runtime_dir = %q, want absolutized", cfg.Paths.RuntimeDir)
	}
}

func TestLoadRejectsSamePriorityConflict(t *testing.T) {
	a := writeSource(t, "a.toml", "version = 1\n")
	b := writeSource(t, "b.toml", "version = 1\n")
	a.Priority = 50
	b.Priority = 50
	if _, err := Load([]Source{a, b}); err == nil {
		t.Fatal("expected same-priority conflict error")
	}
}

func TestDefaultIsValid(t *testing.T) {
	if _, err := Load(nil); err != nil {
		t.Fatalf("default config failed validation: %v", err)
	}
	def := Default()
	if def.Timing.HeartbeatTTLMs <= def.Timing.HeartbeatIntervalMs {
		t.Fatal("default heartbeat ttl must exceed interval")
	}
}

func TestLoadRejectsBadTTL(t *testing.T) {
	src := writeSource(t, "config.toml", "version = 1\n[timing]\nheartbeat_interval_ms = 5000\nheartbeat_ttl_ms = 5000\n")
	if _, err := Load([]Source{src}); err == nil {
		t.Fatal("expected ttl<=interval validation error")
	}
}
