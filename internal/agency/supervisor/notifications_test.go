package supervisor

import (
	"testing"

	"agency-two/internal/agency/storage"
)

// TestNotificationContentMatchesContentDesign locks the rendered (title, body)
// pairs to the templates in docs/product/agency/content-design.md so a copy
// regression is caught here rather than in a shipped notification.
func TestNotificationContentMatchesContentDesign(t *testing.T) {
	cases := []struct {
		name        string
		notif       storage.PendingNotification
		title, body string
	}{
		{
			name: "needs input names the session title and workspace",
			notif: storage.PendingNotification{
				EventType: "RunNeedsInput", SessionKey: "ses_f83a91",
				SessionTitle: "Fix reader race", Workspace: "reader-race", Provider: "Codex",
			},
			title: "Agency: ses_f83a91 needs input",
			body:  "Fix reader race is waiting in reader-race.",
		},
		{
			name: "root workspace uses the friendly label, not the raw key",
			notif: storage.PendingNotification{
				EventType: "RunNeedsInput", SessionKey: "ses_aa11bb",
				SessionTitle: "Audit logs", Workspace: "project_root", Provider: "Codex",
			},
			title: "Agency: ses_aa11bb needs input",
			body:  "Audit logs is waiting in project root.",
		},
		{
			name: "needs approval names the provider and workspace",
			notif: storage.PendingNotification{
				EventType: "RunNeedsApproval", SessionKey: "ses_f83a91",
				SessionTitle: "Fix reader race", Workspace: "reader-race", Provider: "Codex",
			},
			title: "Agency: ses_f83a91 needs approval",
			body:  "Codex is waiting for approval in reader-race.",
		},
		{
			name: "run failed gives the reason and one next action",
			notif: storage.PendingNotification{
				EventType: "RunFailed", SessionKey: "ses_f83a91",
				Workspace: "reader-race", Provider: "Codex", Payload: `{"outcome":"RunnerFailed"}`,
			},
			title: "Agency: ses_f83a91 failed",
			body:  "Runner failed before Codex exited. Run agency status ses_f83a91.",
		},
		{
			name: "close blocked names the workspace and blocker, not masked as failed",
			notif: storage.PendingNotification{
				EventType: "CloseBlocked", Workspace: "reader-race",
				Payload: `{"target":"Workspace","workspace":"reader-race","blockers":["BranchHasUnpushedCommits"]}`,
			},
			title: "Agency: worktree close blocked",
			body:  "reader-race has unpushed commits. No files were removed.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body := notificationContent(tc.notif)
			if title != tc.title {
				t.Errorf("title = %q, want %q", title, tc.title)
			}
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
		})
	}
}
