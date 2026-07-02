# Agency Content Design Contract

## Status

This document defines user-facing content rules for `agency` CLI and TUI
surfaces. It owns labels, command summaries, status wording, confirmations,
errors, empty states, and event language.

Enum values are owned by [schema.md](schema.md#canonical-enums). This document
maps each owned enum value to its human display label. JSON output uses the
`PascalCase` enum value; presentation surfaces use the label.

## Content Principle

Every label renders a real product concept. Content must not create a friendlier
parallel vocabulary that hides the schema. The primary nouns are:

- Project.
- Host.
- Workspace.
- Managed worktree.
- Agent profile.
- Agent session.
- Agent run.
- Runner.
- Tmux target.
- Safety check.
- Close attempt.
- Close blocker.
- Event.

## Voice

Use concise, factual, operator-grade language.

Rules:

- State what happened or what blocks the action.
- Name the exact target.
- Name the exact consequence.
- Do not use emotional or playful language.
- Do not say an action completed when only a request was accepted.
- Do not say "unknown" when the system has a repair-required or defect state.
- Do not hide provider, path, branch, or model when those facts matter.

## Status Labels

Each table maps the owned enum value (used verbatim in JSON) to its display
label. Definitions match [spec.md Status Model](spec.md#status-model).

### Run Status Labels

| Enum (`RunStatus`) | Label | Meaning |
| --- | --- | --- |
| `Starting` | `Starting` | Run intent exists; runner heartbeat has not been accepted. |
| `Live` | `Live` | Runner heartbeat is current and no terminal outcome exists. |
| `NeedsInput` | `Needs input` | Prompt-state detection hints a user-input prompt. |
| `NeedsApproval` | `Needs approval` | Provider or safety request is waiting for approval. |
| `Quiet` | `Quiet` | Runner is live with no recent output and no detected prompt. |
| `Exited` | `Exited` | Terminal outcome is `ProviderExited`. |
| `Stopped` | `Stopped` | Terminal outcome is `UserStopped`. |
| `Killed` | `Killed` | Terminal outcome is `UserKilled`. |
| `Failed` | `Failed` | Terminal outcome is `RunnerFailed`, `StartFailed`, or `Orphaned`. |
| `LostTmuxTarget` | `Lost tmux target` | Expected tmux target is missing. |
| `LostRunner` | `Lost runner` | Runner heartbeat expired. |
| `Closed` | `Closed` | Session has been closed. |
| `RepairRequired` | `Repair required` | Durable facts contradict each other. |

### Git Status Labels

Git state is multi-axis (see [schema.md](schema.md#gitsummary-and-git-axes)). The
`GitSummary` label is derived for dense display; the structured axes are shown in
detail views and JSON.

| Enum (`GitSummary`) | Label | Meaning |
| --- | --- | --- |
| `Clean` | `Clean` | No tracked, untracked, ignored-user, conflict, or upstream blockers. |
| `Dirty` | `Dirty` | Tracked changes exist. |
| `Untracked` | `Untracked` | Untracked files exist. |
| `IgnoredUserFiles` | `Ignored user files` | Ignored user files are present and not marked disposable. |
| `Conflicts` | `Conflicts` | Merge or rebase conflicts exist. |
| `Ahead` | `Ahead` | Local commits are not proven pushed or merged. |
| `Behind` | `Behind` | Upstream has commits not in the local branch. |
| `Diverged` | `Diverged` | Local and upstream both have unique commits. |
| `DetachedHead` | `Detached head` | Workspace is not on a branch. |
| `MissingWorktree` | `Missing worktree` | Expected worktree path is absent. |
| `NotAWorktree` | `Not a worktree` | Path exists but is not the expected git worktree. |

### Close Eligibility Labels

Close eligibility is a set of blockers with a derived `CloseSummary` label for
dense display (see [schema.md](schema.md#closesummary)). The full blocker set is
always shown in detail views and JSON.

| Enum (`CloseSummary`) | Label | Meaning |
| --- | --- | --- |
| `Closable` | `Closable` | Close checks passed. |
| `StopRequired` | `Stop required` | A live run blocks close. |
| `SharedWorkspaceBlocked` | `Shared workspace blocked` | Another live session uses the workspace. |
| `DirtyWorktreeBlocked` | `Dirty worktree blocked` | Tracked changes block worktree close. |
| `UntrackedFilesBlocked` | `Untracked files blocked` | Untracked files block worktree close. |
| `IgnoredUserFilesBlocked` | `Ignored user files blocked` | Ignored user files block worktree close. |
| `ConflictsBlocked` | `Conflicts blocked` | Merge or rebase conflicts block worktree close. |
| `UnpushedCommitsBlocked` | `Unpushed commits blocked` | Unique local commits or missing upstream proof block worktree close. |
| `RepairRequired` | `Repair required` | Structural state must be repaired before close. |

## Dashboard Content

The dashboard header shows:

```text
Agency  project: agency-two  host: local  sessions: 4 live, 1 needs input
```

Session rows use this order, matching the spec dashboard columns. The `WORKSPACE`
column shows the workspace key (or `project root`); the handle is available in
detail and JSON. Dense information is correct for the target user.

```text
SESSION      PROVIDER  TITLE                 PROJECT      WORKSPACE     RUN          GIT     MODEL      EFFORT  LAST EVENT  CLOSE
ses_f83a91   codex     Fix reader race       agency-two   reader-race   Live         Clean   gpt-5.5    high    12:00:04    Stop required
ses_54bd20   claude    Audit auth callback   agency-two   project root  Needs input  Dirty   sonnet     high    11:58:19    Dirty worktree blocked
```

Rows do not hide the provider or workspace mode.

## Launcher Content

The launcher must show the exact command preview before start. Codex:

```text
Command preview
cwd: /home/niels/src/personal/agency-two
workspace: managed worktree from origin/main
provider: codex
model: gpt-5.5
effort: high
argv: codex --model gpt-5.5 -c model_reasoning_effort="high" --sandbox workspace-write --ask-for-approval on-request
```

Claude:

```text
Command preview
cwd: /home/niels/src/personal/agency-two
workspace: project root
provider: claude
model: sonnet
effort: high
argv: claude --model sonnet --effort high --permission-mode default
```

The `argv` line is a rendering of the stored argv token array; dynamic tokens are
shell-quoted at the display boundary, never concatenated.

Validation failures are inline and blocking:

```text
Cannot launch. Provider codex does not support effort max.
Cannot launch. Base ref origin/main could not be resolved.
Cannot launch. tmux is not available on host local.
```

## Close Prompt Content

### Close Live Session

```text
Close session ses_f83a91?

The run is still live.

Choose one:
1. Detach only
2. Stop run, then close session
3. Cancel
```

Do not offer worktree removal in the first live-session prompt.

### Close Managed Worktree

```text
Close managed worktree wks_7a23c1?

path: /home/niels/src/personal/.agency-worktrees/agency-two/reader-race
branch: agency/ses_f83a91-reader-race
base: origin/main abc123

This removes the git worktree after safety checks pass. It does not delete the
branch.
```

If blocked, every applicable blocker is listed:

```text
Worktree close blocked.

blockers:
- Unpushed commits blocked: branch agency/ses_f83a91-reader-race is ahead 2 commits
- Untracked files blocked: 1 untracked file

No files were removed.
```

## Command Output

### `agency new`

```text
Agent session started
session: ses_f83a91
provider: codex
title: Fix reader race
workspace: wks_7a23c1 (reader-race)
path: /home/niels/src/personal/.agency-worktrees/agency-two/reader-race
tmux: agency:ses_f83a91
model: gpt-5.5
effort: high
```

The `tmux` target renders as `<session_prefix>:<session_handle>`; the pane is
omitted in short display.

### `agency list`

```text
SESSION     PROVIDER  TITLE              RUN    GIT    WORKSPACE
ses_f83a91  codex     Fix reader race    Live   Clean  reader-race
ses_54bd20  claude    Audit callbacks    Quiet  Dirty  project root
```

### `agency stop`

```text
Stop requested
session: ses_f83a91
run: run_1
signal: graceful
```

After terminal outcome:

```text
Run stopped
session: ses_f83a91
run: run_1
```

`run_1` is the session-scoped run handle; it is unambiguous under the named
session.

### `agency close`

```text
Session closed
session: ses_f83a91
workspace: wks_7a23c1 (reader-race) remains active
```

### `agency worktree close`

```text
Managed worktree removed
workspace: wks_7a23c1 (reader-race)
path: /home/niels/src/personal/.agency-worktrees/agency-two/reader-race
branch: agency/ses_f83a91-reader-race remains
```

## Error Content

Errors use this shape:

```text
Could not <action>.
reason: <typed reason>
target: <typed handle or path>
detail: <one concrete detail>
next: <one valid next action>
```

Examples:

```text
Could not close worktree.
reason: Untracked files blocked
target: wks_7a23c1
detail: 3 untracked files are present.
next: Review the files or remove them, then run agency worktree close wks_7a23c1.
```

```text
Could not attach session.
reason: Lost tmux target
target: ses_f83a91
detail: tmux target agency:ses_f83a91 is missing.
next: Run agency doctor.
```

```text
Could not launch Codex.
reason: Unsupported effort
target: profile codex_default
detail: Provider codex does not support effort max for model gpt-5.4-mini.
next: Choose minimal, low, medium, high, or xhigh.
```

## Empty States

Project has no sessions:

```text
No agent sessions in this project.
Start one with agency new codex or agency new claude.
```

No managed worktrees:

```text
No managed worktrees in this project.
Start a worktree session with agency new codex --worktree.
```

No provider profiles:

```text
No agent profiles are configured.
Create one with agency profile create <name> --provider codex.
```

Remote host attach-only:

```text
Host mosh-box is attach-only.
Control-plane operations require SSH access to the remote supervisor.
```

## Event Language

Events are factual and past tense. Each display string renders one `EventType`
value from [schema.md](schema.md#events):

- `Session created` (`SessionCreated`).
- `Run requested` (`RunRequested`).
- `Runner heartbeat accepted` (`RunnerHeartbeatAccepted`).
- `Run needs input` (`RunNeedsInput`).
- `Run needs approval` (`RunNeedsApproval`).
- `Input accepted` (`InputAccepted`).
- `Provider process exited` (`ProviderProcessExited`).
- `Stop requested` (`StopRequested`).
- `Run stopped` (`RunStopped`).
- `Run killed` (`RunKilled`).
- `Run failed` (`RunFailed`).
- `Close attempt started` (`CloseAttemptStarted`).
- `Close blocked` (`CloseBlocked`).
- `Session closed` (`SessionClosed`).
- `Managed worktree removed` (`ManagedWorktreeRemoved`).
- `Runner adopted` (`RunnerAdopted`).
- `Runner quarantined` (`RunnerQuarantined`).
- `Doctor issue observed` (`DoctorIssueObserved`).
- `Repair completed` (`RepairCompleted`).
- `Tmux server restarted` (`TmuxServerRestarted`).
- `Worktree reconciled` (`WorktreeReconciled`).

Events must not infer motivation or summarize provider reasoning.

## Notification Content

Needs input:

```text
Agency: ses_f83a91 needs input
Fix reader race is waiting in reader-race.
```

Needs approval:

```text
Agency: ses_f83a91 needs approval
Codex is waiting for approval in reader-race.
```

Run failed:

```text
Agency: ses_f83a91 failed
Runner failed before Codex exited. Run agency status ses_f83a91.
```

Close blocked:

```text
Agency: worktree close blocked
reader-race has unpushed commits. No files were removed.
```

## JSON Content Rules

JSON output uses schema keys (`camelCase`) and the owned `PascalCase` enum values
from [schema.md](schema.md#canonical-enums). It does not use human display labels
as values. Multi-axis and set-valued state is emitted structurally, not
collapsed to one token.

Example:

```json
{
  "runStatus": "Live",
  "git": { "summary": "Clean", "tree": "Clean", "upstream": "Current" },
  "close": { "closable": false, "summary": "StopRequired", "blockers": ["SessionStillRunning"] }
}
```

Pretty labels are rendered only at CLI/TUI presentation boundaries.

## Copy Review Checklist

Every new CLI/TUI string must pass these checks:

- It names the actual domain object.
- It states whether the operation completed or was only requested.
- It includes exact target identity when the user can act on it.
- It includes exact path or branch for destructive operations.
- It gives one next action for typed errors.
- It does not invent a softer synonym for a schema concept.
- Its JSON form uses the canonical `PascalCase` enum value from the schema.
- It does not hide a blocker behind `failed`, `unknown`, or `unavailable`. A
  genuine `RunFailed` event may say "failed"; a close blocker may not be masked
  as "failed."
- It does not imply merge, commit, push, branch deletion, or PR behavior.
