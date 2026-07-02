package eventlog

type EventType string

const (
	SessionCreated          EventType = "SessionCreated"
	RunRequested            EventType = "RunRequested"
	RunnerHeartbeatAccepted EventType = "RunnerHeartbeatAccepted"
	RunNeedsInput           EventType = "RunNeedsInput"
	RunNeedsApproval        EventType = "RunNeedsApproval"
	InputAccepted           EventType = "InputAccepted"
	ProviderProcessExited   EventType = "ProviderProcessExited"
	StopRequested           EventType = "StopRequested"
	RunStopped              EventType = "RunStopped"
	RunKilled               EventType = "RunKilled"
	RunFailed               EventType = "RunFailed"
	CloseAttemptStarted     EventType = "CloseAttemptStarted"
	CloseBlocked            EventType = "CloseBlocked"
	SessionClosed           EventType = "SessionClosed"
	ManagedWorktreeRemoved  EventType = "ManagedWorktreeRemoved"
	DoctorIssueObserved     EventType = "DoctorIssueObserved"
	RepairCompleted         EventType = "RepairCompleted"
	RunnerAdopted           EventType = "RunnerAdopted"
	RunnerQuarantined       EventType = "RunnerQuarantined"
	TmuxServerRestarted     EventType = "TmuxServerRestarted"
	WorktreeReconciled      EventType = "WorktreeReconciled"
)

func Valid(value string) bool {
	switch EventType(value) {
	case SessionCreated, RunRequested, RunnerHeartbeatAccepted, RunNeedsInput, RunNeedsApproval,
		InputAccepted, ProviderProcessExited, StopRequested, RunStopped, RunKilled, RunFailed,
		CloseAttemptStarted, CloseBlocked, SessionClosed, ManagedWorktreeRemoved,
		DoctorIssueObserved, RepairCompleted, RunnerAdopted, RunnerQuarantined,
		TmuxServerRestarted, WorktreeReconciled:
		return true
	default:
		return false
	}
}

func Label(value string) string {
	switch EventType(value) {
	case SessionCreated:
		return "Session created"
	case RunRequested:
		return "Run requested"
	case RunnerHeartbeatAccepted:
		return "Runner heartbeat accepted"
	case RunNeedsInput:
		return "Run needs input"
	case RunNeedsApproval:
		return "Run needs approval"
	case InputAccepted:
		return "Input accepted"
	case ProviderProcessExited:
		return "Provider process exited"
	case StopRequested:
		return "Stop requested"
	case RunStopped:
		return "Run stopped"
	case RunKilled:
		return "Run killed"
	case RunFailed:
		return "Run failed"
	case CloseAttemptStarted:
		return "Close attempt started"
	case CloseBlocked:
		return "Close blocked"
	case SessionClosed:
		return "Session closed"
	case ManagedWorktreeRemoved:
		return "Managed worktree removed"
	case DoctorIssueObserved:
		return "Doctor issue observed"
	case RepairCompleted:
		return "Repair completed"
	case RunnerAdopted:
		return "Runner adopted"
	case RunnerQuarantined:
		return "Runner quarantined"
	case TmuxServerRestarted:
		return "Tmux server restarted"
	case WorktreeReconciled:
		return "Worktree reconciled"
	default:
		panic("unknown EventType: " + value)
	}
}
