package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"agency-two/internal/agency/content"
	"agency-two/internal/agency/eventlog"
	"agency-two/internal/agency/storage"
)

const (
	// notificationInterval is how often the worker scans for pending deliveries.
	notificationInterval = 2 * time.Second
	// maxNotificationAttempts bounds retries before a delivery is failed.
	maxNotificationAttempts = 3
)

// notificationLoop is the notification service worker. It runs off the canonical
// state (append-only events + notification_deliveries), delivers selected events
// to their channels with bounded retry, and records each attempt. Delivery
// failures never touch run state — the worker is fully decoupled from the
// event-writing transaction.
func (s *Server) notificationLoop(ctx context.Context) {
	ticker := time.NewTicker(notificationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.deliverPendingNotifications(ctx)
		}
	}
}

func (s *Server) deliverPendingNotifications(ctx context.Context) {
	pending, err := s.store.PendingNotifications(ctx, maxNotificationAttempts)
	if err != nil {
		s.log.Error("notification scan failed", "error", err.Error())
		return
	}
	for _, n := range pending {
		title, body := notificationContent(n)
		if deliverErr := s.deliverNotification(n.ChannelType, title, body); deliverErr != nil {
			terminal := n.AttemptSeq >= maxNotificationAttempts
			failure := `{"code":"DeliveryFailed","detail":` + strconv.Quote(deliverErr.Error()) + `}`
			if err := s.store.RecordNotificationFailed(ctx, n.ChannelID, n.EventID, n.AttemptSeq, failure, terminal); err != nil {
				s.log.Error("record notification failure failed", "error", err.Error())
			}
			if terminal {
				s.log.Warn("notification delivery exhausted retries", "channel", n.ChannelKey, "event", n.EventType, "error", deliverErr.Error())
			}
			continue
		}
		if err := s.store.RecordNotificationDelivered(ctx, n.ChannelID, n.EventID, n.AttemptSeq); err != nil {
			s.log.Error("record notification delivery failed", "error", err.Error())
		}
	}
}

// deliverNotification performs the real side effect for a channel type. Terminal
// notifications append to a spool file a terminal client tails; desktop
// notifications shell out to the host notifier.
func (s *Server) deliverNotification(channelType, title, body string) error {
	switch channelType {
	case "Terminal":
		return s.appendNotificationSpool(title, body)
	case "Desktop":
		return deliverDesktopNotification(title, body)
	default:
		return fmt.Errorf("unsupported notification channel type %q", channelType)
	}
}

func (s *Server) appendNotificationSpool(title, body string) error {
	if err := os.MkdirAll(s.logDir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.logDir, "notifications.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s\t%s\t%s\n", storage.Now(), title, body)
	return err
}

func deliverDesktopNotification(title, body string) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("desktop notifier unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, title, body).Run()
}

// notificationContent renders the operator-facing title and body for one pending
// notification from the owned event vocabulary.
// notificationContent renders the (title, body) pair per content-design.md's
// Notification Content section: it names the domain object, states waiting vs
// failed, includes the actionable target identity, names the exact workspace for
// a blocked close, and gives one next action for a failure. It never masks a
// close blocker as "failed".
func notificationContent(n storage.PendingNotification) (string, string) {
	subject := n.SessionKey
	if subject == "" {
		subject = "agency"
	}
	workspace := content.WorkspaceLabel(n.Workspace)
	if workspace == "" {
		workspace = "its workspace"
	}
	provider := n.Provider
	if provider == "" {
		provider = "The agent"
	}
	switch n.EventType {
	case string(eventlog.RunNeedsInput):
		actor := n.SessionTitle
		if actor == "" {
			actor = provider
		}
		return "Agency: " + subject + " needs input", actor + " is waiting in " + workspace + "."
	case string(eventlog.RunNeedsApproval):
		return "Agency: " + subject + " needs approval", provider + " is waiting for approval in " + workspace + "."
	case string(eventlog.RunFailed):
		return "Agency: " + subject + " failed", failureReason(n.Payload, provider) + " Run agency status " + subject + "."
	case string(eventlog.RunStopped):
		return "Agency: " + subject + " stopped", provider + " was stopped in " + workspace + "."
	case string(eventlog.CloseBlocked):
		return "Agency: worktree close blocked", workspace + " has " + blockerPhrase(n.Payload) + ". No files were removed."
	default:
		return "Agency: " + subject, eventlog.Label(n.EventType)
	}
}

// failureReason renders a RunFailed body fragment from the terminal outcome in
// the event payload, naming the provider for the runner-failed case to match
// content-design ("Runner failed before Codex exited.").
func failureReason(payload, provider string) string {
	var p struct {
		Outcome string `json:"outcome"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	switch p.Outcome {
	case "RunnerFailed":
		return "Runner failed before " + provider + " exited."
	case "StartFailed":
		return "Run failed to start."
	case "Orphaned":
		return "Runner was orphaned."
	default:
		return "Run failed."
	}
}

// blockerPhrase renders the first close blocker as a human phrase, so a blocked
// close is never masked behind a generic word. The blocker set is emitted
// structurally in the event; the notification surfaces the leading cause.
func blockerPhrase(payload string) string {
	var p struct {
		Blockers []string `json:"blockers"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	if len(p.Blockers) == 0 {
		return "open close blockers"
	}
	return blockerText(p.Blockers[0])
}

func blockerText(blocker string) string {
	switch blocker {
	case "SessionStillRunning":
		return "a running session"
	case "WorkspaceShared":
		return "a shared workspace"
	case "WorktreeDirty":
		return "uncommitted changes"
	case "WorktreeHasUntrackedFiles":
		return "untracked files"
	case "WorktreeHasIgnoredUserFiles":
		return "ignored user files"
	case "WorktreeHasConflicts":
		return "merge conflicts"
	case "BranchHasUnpushedCommits":
		return "unpushed commits"
	case "NoRemoteTrackingProof":
		return "no remote-tracking proof"
	case "OwnershipMarkerMismatch":
		return "an ownership marker mismatch"
	case "PathOutsideWorkspaceRoot":
		return "a path outside its workspace root"
	case "GitWorktreeMissing":
		return "a missing git worktree"
	case "NotExpectedWorktree":
		return "an unexpected worktree"
	case "BranchCheckedOutElsewhere":
		return "its branch checked out elsewhere"
	case "TmuxTargetMissing":
		return "a missing tmux target"
	case "RunnerHeartbeatExpired":
		return "an expired runner heartbeat"
	default:
		return "open close blockers"
	}
}
