package eventlog

import "testing"

func TestEventTypesAreKnownAndLabeled(t *testing.T) {
	events := []EventType{
		SessionCreated, RunRequested, RunnerHeartbeatAccepted, RunNeedsInput, RunNeedsApproval,
		InputAccepted, ProviderProcessExited, StopRequested, RunStopped, RunKilled, RunFailed,
		CloseAttemptStarted, CloseBlocked, SessionClosed, ManagedWorktreeRemoved,
		DoctorIssueObserved, RepairCompleted, RunnerAdopted, RunnerQuarantined,
		TmuxServerRestarted, WorktreeReconciled,
	}
	for _, eventType := range events {
		if !Valid(string(eventType)) {
			t.Fatalf("event type %s was not valid", eventType)
		}
		if Label(string(eventType)) == "" {
			t.Fatalf("event type %s had no label", eventType)
		}
	}
	if Valid("UnknownEvent") {
		t.Fatal("UnknownEvent was valid")
	}
}
