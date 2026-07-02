package config

// Default is the built-in effective configuration used when no config source
// overrides a value. It encodes the documented schema defaults and the initial
// catalog (a single local host, the default tmux server, the strict safety
// policy, the two default profiles, and the terminal notification channel). It
// is the baseline every config source is merged over — not a runtime fallback.
func Default() Config {
	return Config{
		Version: 1,
		Defaults: Defaults{
			Host:         "local",
			Profile:      "codex_default",
			WorktreeMode: "Prompt",
			BaseRef:      "origin/main",
		},
		UI: UI{Theme: "System", RefreshMs: 1000, ShowClosed: false},
		Timing: Timing{
			HeartbeatIntervalMs:   2000,
			HeartbeatTTLMs:        10000,
			QuietThresholdMs:      30000,
			GracefulStopTimeoutMs: 10000,
			ReconcileIntervalMs:   15000,
			SuspendResumeReset:    true,
		},
		Retention: Retention{
			OutputRetentionDays:      14,
			EventRetentionDays:       90,
			StatusSnapshotWindow:     50,
			IdempotencyRetentionDays: 7,
			BusyTimeoutMs:            5000,
			JournalSizeLimitBytes:    67108864,
		},
		Security: Security{
			SocketPeerCredentialCheck: true,
			TCPTunnelRequiresToken:    true,
			TCPTunnelLoopbackOnly:     true,
		},
		Hosts: map[string]Host{
			"local": {DisplayName: "Local", Access: HostAccess{Mode: "Local"}, TmuxServer: "default"},
		},
		TmuxServers: map[string]TmuxServer{
			"default": {Socket: "default", SessionPrefix: "agency"},
		},
		Profiles: map[string]Profile{
			"claude_default": {DisplayName: "Claude Default", Provider: "claude", Model: "sonnet", Effort: "high", PermissionMode: "default"},
			"codex_default":  {DisplayName: "Codex Default", Provider: "codex", Model: "gpt-5.5", Effort: "high", SandboxMode: "workspace-write", ApprovalPolicy: "on-request"},
		},
		SafetyPolicy: SafetyPolicy{
			BlockDirtyWorktree:             true,
			BlockUntrackedFiles:            true,
			BlockIgnoredUserFiles:          true,
			BlockConflicts:                 true,
			BlockUnpushedCommits:           true,
			BlockMissingUpstreamProof:      true,
			DangerousClaudePermissionModes: []string{"bypassPermissions"},
			DangerousCodexSandboxModes:     []string{"danger-full-access"},
			DangerousCodexFlags:            []string{"--dangerously-bypass-approvals-and-sandbox", "--yolo"},
			DangerRequiresExplicitRequest:  true,
		},
		Notifications: map[string]Notification{
			"terminal": {Type: "Terminal", Events: []string{"RunNeedsInput", "RunNeedsApproval", "RunStopped", "RunFailed", "CloseBlocked"}},
		},
	}
}
