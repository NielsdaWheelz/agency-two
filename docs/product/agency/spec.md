# Agency Product And Architecture Spec

## Status

This is the target-state specification for `agency`, a one-user CLI/TUI for
managing local and devbox-hosted coding-agent sessions. It is a hard-cutover
specification: there is no legacy surface, no compatibility mode, and no
fallback behavior for old data or old command shapes.

This document owns product behavior, subsystem architecture, operation
contracts, scope, and acceptance criteria. The storage and configuration shape,
including every persisted enum, lives in [schema.md](schema.md). CLI/TUI wording
and content rules live in [content-design.md](content-design.md).

## Product Summary

`agency` manages terminal coding agents, starting with Claude Code and Codex,
from one CLI/TUI. It creates and tracks agent sessions, starts agents inside
tmux, optionally creates git worktrees, exposes session and worktree status,
shows read-only diffs of agent work, opens sessions for direct interaction,
stops sessions, and closes sessions or managed worktrees only after explicit
safety checks pass.

`agency` does not replace Claude Code, Codex, git, or tmux. It provides a
durable control plane over them.

## Source Principles

The product follows the repository standards in `docs/rules`:

- Product docs live outside `docs/rules`; `docs/rules` remains shared
  engineering standards.
- Transport does not own long-lived work. CLI and TUI clients may disconnect
  without interrupting sessions.
- Services own state and behavior end to end. Entry points parse input and call
  services.
- Mutating operations state their category, replay identity, linearization
  strategy, and side-effect behavior.
- Destructive cleanup removes user-visible ownership before tearing down support
  resources.
- Persisted rows model durable facts. Runtime labels are projections from facts
  and observations.
- Trusted stored state is not authority for destructive work. Destructive
  operations re-observe filesystem, git, process, and tmux state before acting.
- The database schema is hard-cutover, but the `agency-runner` wire protocol is
  independently versioned and forward-compatible, because runners outlive
  supervisor upgrades.
- `agency` stores no long-lived credentials. Provider auth is sourced from each
  provider's own mechanism on the host where the runner runs.

## External Product References

Provider-specific launch controls in this spec are grounded in the public
provider documentation current on 2026-07-01:

- Claude Code CLI reference:
  <https://code.claude.com/docs/en/cli-reference>
- Claude Code model configuration:
  <https://code.claude.com/docs/en/model-config>
- Codex CLI command reference:
  <https://developers.openai.com/codex/cli/reference>
- Codex configuration reference:
  <https://developers.openai.com/codex/config-reference>
- Codex app worktrees:
  <https://developers.openai.com/codex/app/worktrees>

These references inform adapter contracts. They do not authorize `agency` to
depend on provider-private state, private transcript formats, or provider-owned
session databases.

## Target User

The target user is one expert developer operating local repositories and
remote devboxes. The user runs multiple Claude Code and Codex sessions in
parallel, expects direct terminal access, wants tmux resilience, wants to be
told when a session needs attention, and wants hard guards against losing work.

The product is not a team SaaS, CI bot, hosted build platform, PR workflow
manager, or agent-planning framework.

## Goals

- Start Claude Code and Codex sessions from a single command and TUI.
- Run sessions in tmux so they survive client detach, SSH disconnect, and TUI
  exit.
- Support optional managed git worktrees for isolated parallel agent work.
- Store enough durable state to recover and explain sessions after process or
  client restarts, including adopting still-running runners after a supervisor
  upgrade.
- Display accurate status from live observations and durable terminal facts.
- Show a read-only diff of a workspace against its base so the user can review
  agent work without leaving `agency`.
- Notify the user when a parallel session needs input, needs approval, stops, or
  fails, so many sessions can be supervised at once.
- Make model, effort, permission, sandbox, and provider defaults explicit.
- Prevent data loss during session and worktree close operations.
- Make remote/devbox operation first-class by running the supervisor where work
  runs, and auto-start it across reboots.
- Provide machine-readable CLI output for scripting without making scripts parse
  the TUI or tmux.
- Keep every capability owned by one subsystem with one public contract.
- Store no long-lived provider credentials at rest.

## Non-Goals

- No merge automation.
- No commit automation.
- No push automation.
- No pull request creation.
- No branch deletion in the first target state. Guarded deletion of provably
  merged-and-pushed branches is a named [Deferred Decision](#deferred-decisions),
  not a permanent exclusion; the tool mints branches, so their cleanup is
  acknowledged rather than ignored.
- No cloud-hosted multi-user service.
- No custom terminal emulator.
- No replacement for Claude Code agent view or Codex app worktrees.
- No transcript parsing as a correctness source. Reading the rendered tmux/PTY
  output for best-effort prompt hints is permitted and bounded (see
  [Prompt-State Detection](#prompt-state-detection)); reading provider-private
  transcripts, JSONL, or session databases is not.
- No direct modification of provider-owned session databases.
- No raw directory deletion for managed worktrees.
- No automatic cleanup of dirty, untracked, ignored, or unpushed work.
- No mosh-backed control plane.
- No hidden compatibility branches for old schemas, command names, or state
  layouts.

## Systems And Subsystems

The product decomposes into subsystems by kind. Kind matters: services own
state and self-wire their private dependencies; adapters translate at external
boundaries; process layers host services and background work; helpers are pure
edge rendering; out-of-band concerns are build/test infrastructure, not runtime
subsystems.

Process layers and binaries:

1. CLI client (`cmd/agency`).
2. TUI client (`cmd/agency`, default subcommand).
3. Supervisor process (`cmd/agency-supervisor`, logic in
   `internal/agency/supervisor`).
4. Runner harness (`cmd/agency-runner`, logic in `internal/agency/runner`).

Boundary adapters:

5. IPC/API server.
6. Git adapter.
7. Tmux adapter.
8. Claude provider adapter.
9. Codex provider adapter.

Services (own durable state and behavior):

10. Configuration service.
11. Storage service.
12. Project service.
13. Workspace service.
14. Run service (owns runs, runner supervision, heartbeats, input routing and
    acknowledgement, stop, and terminal outcomes).
15. Status service.
16. Safety service.
17. Close workflow service.
18. Doctor service.
19. Remote host service.
20. Notification service.
21. Event log service.

Helpers and out-of-band concerns (not runtime services):

22. Provider catalog: a static two-entry registry helper inside the provider
    module.
23. Content and presentation: label-rendering helpers owned by `cli/` and
    `tui/`, derived from the schema enums.
24. Runner wire protocol (`internal/agency/runnerproto`): shared, versioned
    message types used by both the runner and the run service.
25. Test harness: build/test infrastructure.
26. Packaging and install: build/release infrastructure.

There is no standalone input subsystem; sending and acknowledging input is a run
service operation.

## System Responsibility Map

### CLI Client

The CLI owns command parsing, deriving the replay key for each mutating
invocation from the request payload, supervisor connection, and formatting of
command responses. It does not own session lifecycle, process lifecycle, git
decisions, tmux decisions, or safety policy.

### TUI Client

The TUI owns terminal rendering, keyboard handling, local selection state, and
calls to the supervisor API. Exiting the TUI detaches from the control surface
only. TUI exit never stops agent runs, kills tmux panes, or removes worktrees.

### Supervisor Process

The supervisor is the process layer. It holds the singleton lock on the state
directory, owns host-owned resources (the SQLite handle, the API socket), builds
and hosts the service layers, runs the keyed supervised task registry, performs
startup reconciliation, and runs background compaction. It does not own
run-domain or workspace-domain logic; those live in their services. Exactly one
supervisor owns one state database on one host, enforced as described in
[Supervisor Singleton](#supervisor-singleton).

### Runner Harness

`agency-runner` runs inside the tmux pane and launches the selected provider
CLI as its child process. The runner is the live source and authority of process
facts while running: it owns the child process, the PTY bridge, output capture
and spooling, heartbeat emission, input acknowledgement, and exit reporting for
one agent run. It serves a control socket (see [Runner Protocol And
Adoption](#runner-protocol-and-adoption)). The run service is the durable owner
that records those facts; the runner does not write the database.

tmux is the attachable display and process container. tmux target ids are
observations and routing targets, not lifecycle authority.

### IPC/API Server

The IPC/API server exposes the supervisor API to CLI and TUI clients over a Unix
socket, or to remote clients through an SSH-forwarded socket or an
authenticated private TCP tunnel. It authenticates every connection before
dispatch (see [Security](#security)). The transport can reconnect without
changing session lifecycle.

### Configuration Service

The configuration service parses TOML files and environment-derived settings
once at the boundary, converts ingress vocabulary into owned enums, validates
them into owned types, merges sources by precedence, and persists effective
configuration revisions. Runtime services read typed configuration, not raw
TOML.

### Storage Service

The storage service owns SQLite schema, migrations, connection pragmas,
transactions, sequence allocation, and row conversion. It exposes
operation-shaped repository APIs. Other subsystems do not issue ad hoc SQL.

### Project Service

The project service owns known repositories, project roots, project defaults,
and project display names. It does not own worktree creation or run lifecycle.

### Workspace Service

The workspace service owns session workspaces: project-root workspaces, managed
worktree workspaces, and adopted external worktrees. It owns worktree
allocation, path checks, marker files, publication, and the `git worktree
remove` mutation. It does not decide whether a close is safe; it asks the safety
service for that verdict.

### Git Adapter

The git adapter is a boundary adapter over the git CLI. It parses stable machine
output such as `git worktree list --porcelain -z`, `git status --porcelain -z`,
and `git diff` into owned Git snapshots. It never decides whether a git mutation
is safe. Services make domain decisions from parsed snapshots.

### Tmux Adapter

The tmux adapter is a boundary adapter over the tmux CLI and control mode. It
creates sessions, windows, and panes, starts runner commands, attaches clients,
captures target observations including server instance identity, renders the
rendered-pane snapshot for prompt hints, and tears down owned targets when the
run service authorizes teardown. It does not decide cleanup eligibility.

### Claude Provider Adapter

The Claude adapter owns Claude Code command construction and supported controls
(see [Claude Adapter Rules](#claude-adapter-rules)). It treats Claude's own agent
view and background tasks as external behavior, not agency-owned state.

### Codex Provider Adapter

The Codex adapter owns Codex command construction and supported controls (see
[Codex Adapter Rules](#codex-adapter-rules)). It treats Codex app, app-server,
and managed worktrees as external surfaces, not agency-owned state.

### Run Service

The run service owns agent sessions, agent runs, per-run supervision tasks,
runner heartbeats, input routing and acknowledgement, stop requests, and
terminal outcomes. It records what the runner reports; it does not itself hold
the live child process.

### Status Service

The status service owns read-only projections from durable facts and fresh
observations: session status, run status, git status, worktree close
eligibility, provider prompt hints, and TUI summaries. It computes close
eligibility by calling the safety service; it never mutates. Status projections
are rebuildable.

### Safety Service

The safety service owns every policy decision that can block or require explicit
approval. It is the predicate: it evaluates launch requests, git mutations,
process stop or kill requests, worktree removal, remote commands, and provider
permission modes, and returns typed findings and close blockers. It does not
perform the mutation it gates.

### Close Workflow Service

The close workflow is a thin durable orchestrator for guarded close of sessions
and managed worktrees. It calls the safety service for the verdict and the
workspace service for the mutation, records the attempt and any blockers,
removes agency visibility before tearing down support state, and verifies the
teardown. It does not itself own removal, preconditions, or policy.

### Doctor Service

Doctor observes configured hosts, projects, tmux targets, worktrees, runners,
branches, and provider CLIs, and enumerates typed issues. Doctor never mutates
state: each repair is a named operation on the owning domain service, dispatched
explicitly with its own safety check. Doctor is the manual, deep variant of the
same engine that runs automatic [Startup
Reconciliation](#startup-reconciliation).

### Remote Host Service

The remote host service owns devbox registration, SSH bootstrap checks, remote
supervisor discovery, and attach command construction. The remote supervisor
owns remote session lifecycle.

### Notification Service

The notification service delivers selected events to terminal or desktop targets
with bounded retry. Notification delivery state is not canonical session state.

### Event Log Service

The event log service owns event semantics over storage-owned tables (no ad hoc
SQL). Events are append-only and support audit, status projection, notification
fan-out, and debugging. Each writing service contributes events; the event log
aggregates them.

## Final State

The final state is a Go repository with:

- One primary runtime: Go.
- One package manager: Go modules.
- One authoritative product store: SQLite.
- One terminal orchestration backend: tmux.
- One git implementation boundary: git CLI.
- One long-lived supervisor per host state directory.
- Two provider adapters: Claude and Codex.
- One CLI and one TUI using the same supervisor API.

Expected future file layout:

```text
cmd/agency/main.go
cmd/agency-supervisor/main.go
cmd/agency-runner/main.go
internal/agency/api/
internal/agency/supervisor/
internal/agency/runner/
internal/agency/runnerproto/
internal/agency/config/
internal/agency/storage/
internal/agency/project/
internal/agency/workspace/
internal/agency/git/
internal/agency/tmux/
internal/agency/provider/
internal/agency/provider/claude/
internal/agency/provider/codex/
internal/agency/run/
internal/agency/status/
internal/agency/safety/
internal/agency/close/
internal/agency/doctor/
internal/agency/remote/
internal/agency/notification/
internal/agency/eventlog/
internal/agency/tui/
internal/agency/cli/
```

`internal/agency/runnerproto` holds the shared, versioned wire types so both the
runner and the run service depend on one definition rather than duplicating it.
No implementation file is added before the product spec, schema, and test
acceptance contract are accepted.

## Capability Contract

### Project Capabilities

- Register the current repository as a project.
- Resolve a project from a cwd.
- Set default provider, profile, model, effort, worktree mode, and base ref.
- List project sessions and workspaces.
- Archive a project record without touching files.

### Session Capabilities

- Create an agent session and start its first run.
- Start an additional run for an existing session in the same workspace.
- Attach to a live tmux target.
- Send input to a live runner.
- Show a read-only diff of the session's workspace against its base.
- Stop a live run.
- Rename a session title.
- Close a session after guards pass.
- List sessions; bulk-close terminal sessions.
- Get one session's full status.
- Show session event history.

### Workspace Capabilities

- Use the project root as a workspace.
- Create a managed worktree workspace.
- Adopt an existing worktree workspace.
- List workspaces.
- Show git status and read-only diff for a workspace.
- Close a managed worktree after guards pass.

### Provider Capabilities

- Validate requested model and effort controls.
- Build exact provider launch argv as a token array.
- Display provider-specific command preview.
- Detect provider CLI availability.
- Report unsupported controls as typed errors before launch.

### Profile Capabilities

- Create or update a named provider profile, producing a new revision.
- Set the default profile.
- List profiles.

### Safety Capabilities

- Block dangerous launch modes unless explicitly requested.
- Block closing live sessions unless a stop action is chosen first.
- Block worktree removal when any close blocker applies.
- Record close attempts and the full set of blockers.
- Explain every blocker with exact evidence.

### Remote Capabilities

- Check SSH access to a devbox.
- Start or discover a remote supervisor; auto-start it on boot.
- Connect local CLI/TUI to a remote supervisor over an authenticated transport.
- Attach to remote tmux through SSH.
- Treat mosh as attach-only.

### Maintenance Capabilities

- Prune retained output, events, and expired coordination rows.
- Export a consistent backup of the state database.
- Report orphaned agency-owned branches and worktrees.

## Provider Contract

Every provider adapter implements:

```text
ProviderCapabilities
  provider_key
  display_name
  command_candidates
  model_control
  effort_control
  permission_controls
  sandbox_controls
  extra_directory_control
  initial_prompt_control

BuildLaunchPlan({ input, executionPolicy }) -> LaunchPlan | ProviderInputError
DetectPromptState(renderedSnapshot) -> ProviderPromptHint
ParseOutput(chunk) -> ProviderEvent[]
```

`BuildLaunchPlan` takes a single object parameter. It returns a `LaunchPlan`
whose argv is a validated token array; every dynamic token is an owned
`ShellArgumentText`, so no argv is ever built by string concatenation.

`DetectPromptState` and `ParseOutput` read only the rendered tmux/PTY snapshot,
never a provider-private transcript. Their output is a best-effort hint (see
[Prompt-State Detection](#prompt-state-detection)).

Provider adapters do not:

- Create worktrees.
- Create tmux sessions.
- Decide safety policy.
- Parse private provider transcript databases.
- Modify provider global config unless the command is explicitly a config
  operation.

## Claude Adapter Rules

- Launch Claude Code interactively (no `--print`/`-p`), so the agent runs
  supervised in tmux. Do not use `--bg`/`--background`; agency owns the tmux
  container.
- Use launch-time flags for model (`--model`), effort (`--effort`, values
  `low`, `medium`, `high`, `xhigh`, `max`), permission mode
  (`--permission-mode`, values `default`, `acceptEdits`, `plan`, `auto`,
  `dontAsk`, `bypassPermissions`), additional directories (`--add-dir`), session
  settings (`--settings`), and MCP config (`--mcp-config`).
- Session-scoped plugins use `--plugin-dir` and `--plugin-url`; there is no
  single `--plugin` control, and persistent plugin installs are out of scope.
- Support durable session management via `--session-id`, `--resume`, and
  `--continue` so an additional run can resume a prior Claude session.
- Store the exact argv token array and the redacted environment snapshot for
  every run.
- Do not rely on Claude's internal JSONL transcript shape.
- Do not depend on Claude agent view state for agency status.
- Treat `bypassPermissions` (and its `--dangerously-skip-permissions` alias) as a
  dangerous mode that cannot be a default.
- Treat Claude worktree flags as provider-native convenience only. Managed
  agency worktrees are created by the workspace service.

## Codex Adapter Rules

- Launch Codex interactively (bare `codex`, which opens the terminal UI), not
  the headless `codex exec` subcommand.
- Use launch-time `--model` for the model, the dedicated `--sandbox` flag for
  sandbox mode, and the dedicated `--ask-for-approval` (alias `-a`) flag for
  approval policy. Use `-c model_reasoning_effort=<effort>` for reasoning
  effort, which has no dedicated flag. Additional directories use `--add-dir`,
  profile uses `--profile`, and web search uses `--search`.
- Store the exact argv token array and the redacted environment snapshot for
  every run.
- Do not depend on Codex app worktree state for agency status.
- Do not expose `--yolo` (the alias of `--dangerously-bypass-approvals-and-sandbox`)
  as a default.
- Treat Codex app-server remote mode as an external transport surface, not as
  agency's primary control plane.

## Process Model

For a local run:

```text
CLI/TUI
  -> supervisor API (authenticated)
  -> run service
  -> safety service
  -> workspace service
  -> tmux adapter
  -> tmux pane
  -> agency-runner (serves control socket)
  -> provider CLI child process
```

The run service reconnects to the runner through the runner's control socket at
a deterministic path, so a supervisor restart re-dials without disturbing the
child process.

For a remote/devbox run:

```text
local CLI/TUI
  -> SSH-forwarded socket or authenticated tunnel
  -> remote supervisor API
  -> remote run service
  -> remote tmux
  -> remote agency-runner
  -> remote provider CLI child process
```

The provider process runs where the repository and tools live, using the
provider credentials present on that host.

## Supervisor Singleton

Exactly one supervisor may own one state directory. Enforcement:

- At startup the supervisor acquires an OS advisory lock (`flock`/`fcntl`) held
  for process lifetime on `<state dir>/agency.db.lock`. The kernel releases it on
  crash, so there is no stale-lock problem. The lockfile records pid, start
  epoch, and supervisor version for diagnostics only.
- If the lock is already held, startup fails with `SupervisorAlreadyRunning`
  naming the holding pid.
- On API socket bind, if the socket path exists the supervisor attempts to
  connect: a live peer means another supervisor is running and startup refuses;
  a refused connection means a stale socket, which is unlinked and rebound.
- SQLite locking is not the singleton guard. It protects SQLite integrity, not
  the application invariants that assume a single writer.

## Runner Protocol And Adoption

`agency-runner` outlives supervisor and client restarts and supervisor upgrades.
The runner wire protocol is therefore versioned and forward-compatible, the
opposite policy from the hard-cutover database schema.

- Each runner serves a per-run Unix domain socket at
  `$XDG_RUNTIME_DIR/agency/runners/<run_id>.sock`. The runner is the server and
  the supervisor is the client, so a supervisor restart is a re-dial while the
  runner keeps serving.
- The first frame is a stable, never-changing preamble: magic, `runner_protocol_version`,
  `runner_binary_version`, and `run_id`. Version detection itself can never
  break.
- Messages are length-prefixed: `Hello(version)`, `Subscribe(output,
  fromChunkSeq)`, `Heartbeat`, `SendInput(seq, bytes) -> Ack(seq) | Nack`,
  `RequestStop(graceful)`, `RequestKill`, `Exit(termination)`.
- The runner spools output to a file and serves it from `fromChunkSeq`, so a
  supervisor-down window loses no output; chunks are idempotent on
  `(run_id, chunk_seq)`.
- On startup or upgrade the supervisor dials each `active_runner_bindings`
  endpoint and handshakes:
  - Compatible version (current or supported N-1): adopt. Resume the output
    stream, heartbeat, and control channel. Emit `RunnerAdopted`.
  - Incompatible version: quarantine. Mark the run `RepairRequired`, keep the
    child process running, disable programmatic input and stop, and expose only
    tmux attach plus an explicit "stop via attach" repair. Emit
    `RunnerQuarantined`. Never kill on version mismatch.
- Installing a new runner binary affects only new runs; in-place upgrade never
  signals existing runners.

## Startup Reconciliation

Before serving IPC, and on `reconcile_interval_ms`, the supervisor runs an
idempotent reconciliation pass (Doctor is its manual, deeper form):

1. Re-dial every `active_runner_bindings` endpoint: adopt live compatible
   runners, quarantine incompatible ones, mark past-TTL-unreachable runners
   `Orphaned`, leave within-TTL unreachable runners pending.
2. Read tmux server instance identity per `tmux_servers`. A changed or absent
   identity means a tmux server restart (for example host reboot): bulk-transition
   all targets and bindings under it to orphaned and emit `TmuxServerRestarted`.
3. Cross-check `managed_worktrees` against `git worktree list --porcelain` and
   marker files: publish consistent ones, quarantine inconsistent ones.
4. Detect on-disk agency-marked worktrees with no `managed_worktrees` row and
   record a repair-required adoptable-orphan; never auto-delete user work.
5. Detect runs with a tmux target but no binding or outcome: re-observe the pane
   and mark `StartFailed` or `Orphaned`, or rediscover the runner.

Reconciliation emits an event per transition. It never deletes user work; it
surfaces repair-required state for explicit operator action.

## Time And Clocks

- Heartbeat liveness is computed on the supervisor's monotonic clock, not
  wall-clock. The runner and supervisor are co-located, so there is no
  cross-host skew. `active_runner_bindings.last_heartbeat_at` is kept in
  wall-clock only for display and audit.
- Across a supervisor restart, every binding is treated as unproven and requires
  one fresh heartbeat within `heartbeat_ttl_ms` before it can be declared lost.
  A binding is never declared lost from a wall-clock delta that spans a restart.
- A large wall-clock jump (laptop suspend/resume, NTP step) resets the liveness
  baseline instead of expiring runners, when `suspend_resume_reset` is set.
- All timing parameters (`heartbeat_interval_ms`, `heartbeat_ttl_ms`,
  `quiet_threshold_ms`, `graceful_stop_timeout_ms`, `reconcile_interval_ms`) are
  named configuration constants owned by the config service, not inline
  literals. TTLs are right-open intervals: active while `now < expires_at`.

## Prompt-State Detection

`NeedsInput`, `NeedsApproval`, and `Quiet` are derived from the rendered tmux/PTY
snapshot, the only parsing surface `agency` uses; provider-private transcripts,
JSONL, and session databases remain off-limits.

- Prompt hints are best-effort signals. They may drive dashboard sorting,
  notifications, and hints, but must never gate a destructive or data-loss
  decision.
- Uncertainty degrades to `Live` or `Quiet`; detection never fabricates
  `NeedsInput`.
- The adapter fingerprints the provider TUI version. When heuristics no longer
  match a known fingerprint, Doctor raises a defect so breakage is observable,
  not silent.
- The user can manually mark a session as needing input or approval.

## Security

The supervisor API can launch processes and write to a PTY, so it is an
RCE-equivalent surface and every connection is authenticated before dispatch:

- The default transport is a Unix socket. The server verifies peer credentials
  (`SO_PEERCRED`) against the owning uid.
- Remote access prefers an SSH-forwarded Unix socket, delegating authentication
  to SSH.
- A private TCP tunnel is allowed only with a per-supervisor bearer token
  (minted at start, stored `0600` in the runtime dir) and bound to loopback.
  "Private" network placement is not itself access control.
- Unauthenticated frames are rejected before any dispatch.

## Observability

The supervisor is a long-lived daemon and must be operable, especially remotely:

- Structured, leveled JSON logs to `logs/` with rotation and retention, carrying
  the event `correlation` and `causation` ids.
- A `health` API method returning uptime, supervisor and protocol versions, DB
  path, WAL size, adopted/quarantined/orphaned runner counts, last reconcile
  time, and clock baseline, surfaced by `agency doctor --supervisor`.
- A guarded goroutine-dump endpoint on the Unix socket for debugging hangs.

The event log is domain audit, not operational telemetry; the two are separate.

## Lifecycle Flows

### Create Session Without Worktree

1. CLI/TUI validates command shape and derives the replay key.
2. Supervisor resolves project from cwd.
3. Provider adapter validates requested controls.
4. Safety service evaluates launch policy.
5. Run service inserts session intent, first run intent, and launch event.
6. Tmux adapter creates an owned target.
7. Runner starts in the project-root workspace and begins serving its control
   socket.
8. Runner launches provider CLI.
9. Runner reports accepted start; run service records the binding and first
   heartbeat.
10. Status projection shows the session as live.

### Create Session With Managed Worktree

1. Resolve project and repository.
2. Parse and validate base ref.
3. Reserve a managed-workspace allocation (support state; not yet visible).
4. Run `git worktree add` for the branch from the base ref (external effect).
5. Write the agency marker file into the created worktree (external effect).
6. Publish workspace visibility by inserting the `active_worktrees` row (final
   visibility step).
7. Start the first run in the managed worktree.
8. Display worktree path, branch, base, and session link.

External effects precede the publication row, so no observer can see the
workspace before the worktree exists. A crash before step 6 leaves an on-disk
worktree with no active row, which [Startup
Reconciliation](#startup-reconciliation) records as an adoptable orphan. The
project root remains untouched except for git metadata changes required by
`git worktree`.

### Attach

1. Resolve session handle or short key.
2. Re-observe runner and tmux target.
3. If a live tmux target exists, execute attach command.
4. If runner is live but the tmux target is missing, offer a repair action.
5. If the run has a terminal outcome, show status instead of attaching.

### Send Input

1. Resolve live run and allocate the input sequence number.
2. Send input to the runner over its control socket.
3. Runner writes to the provider PTY.
4. Runner acknowledges byte delivery; the run service records `Accepted`.
5. Event log records input accepted.

The PTY write is memoized by `(run_id, input_seq)`, so a replay reconciles the
existing acknowledgement instead of re-sending keystrokes.

### Show Diff

1. Resolve session or workspace.
2. Git adapter computes `git diff` (and `--stat`) against the pinned `base_sha`.
3. Return a read-only diff. No mutation, merge, commit, or push occurs.

### Stop Run

1. Resolve live run.
2. Safety service evaluates stop policy.
3. Runner receives graceful stop request.
4. If graceful stop reaches `graceful_stop_timeout_ms`, the user may request
   kill.
5. Kill is a separate explicit operation.
6. Terminal outcome is recorded via the binding→outcome transition.

### Close Session

1. Resolve session.
2. Re-observe run state.
3. Block if a live run exists unless the close request includes an accepted stop
   plan.
4. Record close attempt.
5. If allowed, close session visibility with `closed_at`.
6. Leave workspace intact unless a workspace close is explicitly requested.

### Close Managed Worktree

1. Resolve managed workspace.
2. Re-resolve real path.
3. Verify path is under configured workspace roots.
4. Verify agency marker matches workspace id.
5. Verify git worktree identity and repository.
6. Recompute git status and ancestry.
7. Verify no live sessions use the workspace.
8. If any blocker applies, record the full blocker set and stop.
9. Memoize the resolved worktree path, git identity, and tmux target handle into
   the durable close operation's replay state.
10. Unpublish by deleting the `active_worktrees` row and inserting the
   invisible `closing_worktrees` teardown row in one transaction.
11. Run `git worktree remove`.
12. Verify absence.
13. Delete the `closing_worktrees` row and record the `removed_worktrees` fact
   in one transaction.

Support handles are memoized (step 9) before visibility is removed (step 10) so
teardown can complete after a crash. Raw `rm -rf` is not part of normal close.

### Doctor

Doctor performs read-only observation by default:

- Provider CLI presence.
- Provider auth probes when supported without mutation.
- tmux availability and server instance identity.
- git availability.
- Project root existence.
- Managed worktree marker and path consistency.
- Runner heartbeats and protocol versions.
- Orphaned tmux targets.
- Orphaned agency-owned branches (reported, never deleted).
- Stale active-run bindings.
- Redacted-env scan for high-entropy values.

Every repair is a separate named operation on the owning service with its own
safety check.

## Status Model

Status is a projection over durable facts and fresh observations. It is not the
lifecycle source of truth. All values are defined in [schema.md Canonical
Enums](schema.md#canonical-enums).

### Run Status

`RunStatus` values and their derivation:

- `Starting`: run row exists and no runner heartbeat has been accepted.
- `Live`: runner heartbeat is current (monotonic) and no terminal outcome
  exists.
- `NeedsInput`: prompt-state detection hints a user-input prompt.
- `NeedsApproval`: prompt-state detection hints an approval wait, or the safety
  service has an outstanding approval request.
- `Quiet`: runner is live, no output has arrived within `quiet_threshold_ms`, and
  no prompt hint is present.
- `Exited`: terminal outcome is `ProviderExited`.
- `Stopped`: terminal outcome is `UserStopped`.
- `Killed`: terminal outcome is `UserKilled`.
- `Failed`: terminal outcome is `RunnerFailed`, `StartFailed`, or `Orphaned`.
- `LostTmuxTarget`: an expected tmux target is no longer reported. This is a
  modeled external-disappearance outcome, not projection drift.
- `LostRunner`: an active binding exists but the heartbeat exceeded its TTL.
- `Closed`: session has `closed_at`.
- `RepairRequired`: durable facts contradict each other (for example a binding
  with a terminal outcome, or a quarantined incompatible runner). This is the
  narrow defect boundary, distinct from the modeled `Lost*` outcomes.

`Unknown` is not a product status.

### Git Status

Git status is the multi-axis record defined in [schema.md](schema.md#gitsummary-and-git-axes)
(presence, tree, untracked, ignored user files, conflicts, upstream), with
`GitSummary` derived for dense display. A workspace can hold several conditions
at once; they are not one flat value.

### Close Eligibility

Close eligibility is `{ closable, blockers }` where `blockers` is a set of
`CloseBlocker` values, with `CloseSummary` derived for dense display. A workspace
can trip several blockers at once, and all are recorded and shown.

## Operation Model

Every mutating operation runs through the managed operation runner with a stable
replay key derived from the request payload and namespaced by the operation name.
`--replay-key` only overrides that derivation. Replay identity is persisted in
`idempotency_keys`, so a client retry after a dropped response returns the stored
result instead of re-applying effects.

Retry policy is categorical, from one central catalog keyed by dependency class
(infrastructure: SQLite, filesystem; external: git, tmux, provider CLI, runner).
Each durable operation names the policy it uses. Retry-budget exhaustion is a
defect unless the operation explicitly models persistent unavailability as an
outcome.

### Operation Categories

Categories use the names from [operation-types.md](../../rules/operation-types.md).
Linearization names both the conflict key and the strategy. Replay key is the
stable idempotency key; conflict key is what the operation serializes on.

| Operation | Category | Replay key | Linearization |
| --- | --- | --- | --- |
| List sessions | unreplayable read | — | read snapshot |
| Get status | unreplayable read | — | re-observe then read projection |
| Show diff | unreplayable read | — | read snapshot of git |
| Config set | replayable single mutation | config set + key | single-mutation live lock on config key |
| Create session record | replayable single mutation | create session + session key | single-mutation live lock on session key |
| Create managed worktree | durable operation | create worktree + project + workspace key | multi-step exclusivity on workspace key |
| Start run | durable operation | start run + session key + run_seq | multi-step exclusivity on session key and workspace key |
| Send input | durable operation (uncertain transition + stabilization) | send input + run key + input_seq | multi-step exclusivity on run key; ack stabilizes; PTY write memoized on (run, input_seq) |
| Stop run | durable operation | stop run + run key | multi-step exclusivity on run key |
| Close session | durable operation | close session + session key | multi-step exclusivity on session key and its run keys |
| Close worktree | durable operation | close worktree + workspace key | multi-step exclusivity on workspace key |
| Doctor read | unreplayable read | — | observation scope |
| Repair action | durable operation | repair + target key | multi-step exclusivity on repair target key |

Send input is a durable operation, not a single mutation: it performs an external
PTY write plus storage writes, so it exceeds the single-side-effect ceiling. It
is modeled as an uncertain transition whose runner acknowledgement is the
stabilizing recheck, with the PTY write memoized on `(run_id, input_seq)` so
replay never double-sends.

## Concurrency Rules

Contending operations must share a conflict keyspace, or they do not actually
serialize:

- Concurrent creates for the same session key serialize on the session key.
- Concurrent starts for the same open session serialize on the session key; a
  start also acquires the workspace key so it serializes with a concurrent
  worktree close on that workspace.
- Concurrent sends to the same run serialize on the run key and allocate input
  sequence numbers.
- Concurrent close-session and send on the same run serialize because close
  session acquires the affected run keys; close either observes input accepted
  first or blocks the input after close visibility is removed.
- Concurrent worktree close and run start on the same workspace serialize on the
  workspace key.
- Doctor cannot mutate state; repairs are named operations on owning services.

All cross-process serialization relies on the [Supervisor
Singleton](#supervisor-singleton); a second supervisor would bypass these
in-process conflict keys, which is why the singleton is enforced, not assumed.

## Error Model

Expected typed errors (modeled, actionable failures):

- `ProviderNotFound`, `UnsupportedModel`, `UnsupportedEffort`,
  `UnsupportedControl`.
- `ProviderCliMissing`, `TmuxCliMissing`, `GitCliMissing`: the named binary is
  absent or fails its capability precheck. These are prechecks, an intended
  modeled outcome, not synthetic middle-ground errors for a transient outage.
- `InvalidProjectRoot`, `ProjectNotFound`, `SessionNotFound`, `RunNotLive`,
  `WorkspaceNotFound`.
- `WorktreeCloseBlocked` (carries the blocker set).
- `SafetyApprovalRequired`, `SafetyDenied`.
- `SupervisorAlreadyRunning`, `RemoteSupervisorUnavailable` (client-side
  connectivity).

Transient failures of git, tmux, the provider CLI, or the runner during an
operation retry-and-defect on exhaustion; they are not surfaced as synthetic
`Unavailable` product errors.

Defects (broken invariants; never converted into product recovery branches):

- Stored row points to a missing required parent.
- Active run has a terminal outcome and a live binding.
- Managed workspace marker points to a different workspace id.
- Storage claims agency ownership but the live path resolves outside workspace
  roots.
- Runner reports an input acknowledgement for the wrong run id.
- Provider adapter returns a launch plan that violates its declared
  capabilities.
- A durable operation exhausts its retry budget or dead-letters.

`RepairRequired` is the operator-visible face of a dead-lettered durable prefix
or a genuine invariant contradiction. It is not a routine recovery status; the
default operator action is to fix the underlying defect and replay the durable
operation to completion. It is distinct from the modeled `Lost*` outcomes, which
are expected external disappearances.

## Safety Contract

Safety is pessimistic. If live state cannot prove an operation is safe, the
operation blocks.

### Launch Safety

Launch blocks when:

- Provider CLI is missing or fails its capability precheck.
- Requested model or effort is unsupported by the adapter.
- A dangerous permission mode, sandbox mode, or approval/sandbox combination is
  requested without an explicit request flag.
- Worktree creation path is outside allowed roots.
- Base ref cannot be resolved.
- tmux capability check fails.

### Stop Safety

Stop is allowed for agency-owned live runs. Kill requires a separate explicit
operation after graceful stop fails or times out.

### Worktree Close Safety

Worktree close records every applicable `CloseBlocker` (see
[schema.md](schema.md#closeblocker)) and blocks when the set is non-empty:

- A live session uses the workspace (`SessionStillRunning`).
- Another live session shares the workspace (`WorkspaceShared`).
- Tracked changes exist (`WorktreeDirty`).
- Untracked files exist (`WorktreeHasUntrackedFiles`).
- Ignored user files exist (`WorktreeHasIgnoredUserFiles`).
- Merge or rebase conflicts exist (`WorktreeHasConflicts`).
- Unique local commits exist (`BranchHasUnpushedCommits`).
- Upstream proof is missing (`NoRemoteTrackingProof`).
- The marker file is missing or mismatched (`OwnershipMarkerMismatch`).
- The path resolves outside configured roots (`PathOutsideWorkspaceRoot`).
- The expected git worktree is missing (`GitWorktreeMissing`).
- The path is not the expected git worktree (`NotExpectedWorktree`).
- Another worktree has the same branch checked out (`BranchCheckedOutElsewhere`).
- An expected tmux target is missing or the runner heartbeat expired
  (`TmuxTargetMissing`, `RunnerHeartbeatExpired`), which surface as
  `RepairRequired`.

Structural blockers (missing/mismatched marker, path escape, missing/wrong
worktree, branch checked out elsewhere, missing tmux target, expired heartbeat)
are always enforced and not toggleable. The tracked/untracked/ignored/conflict/
unpushed/upstream blockers are governed by the `[safety.policies.*]` toggles.

## Remote And Devbox Design

The supervisor runs where the agent process runs. A local laptop TUI can connect
to a remote supervisor, but remote sessions are owned by the remote supervisor.

Supported remote controls:

- SSH bootstrap.
- SSH command execution for supervisor start/discovery.
- SSH-forwarded Unix socket, or an authenticated loopback TCP tunnel, to the
  supervisor API.
- SSH tmux attach.
- A systemd user unit or launchd agent to auto-start the supervisor on boot.

SSH-forwarded commands are built from validated `ShellArgumentText` tokens and
quoted through the shell-quoting helper; no remote command line is assembled by
string concatenation.

Mosh is attach-only. If a host is reachable only by mosh, `agency` allows
interactive attach commands and blocks provisioning, cleanup, provider launch,
and other control-plane operations.

`agency` transports no provider credentials to the devbox; the remote runner
uses the devbox's own provider login.

## CLI API Design

The primary CLI is `agency`.

### Commands

```text
agency
agency new <provider> [prompt]
agency run <session>
agency list
agency status <session>
agency diff <session>
agency attach <session>
agency send <session> [message]
agency rename <session> <title>
agency stop <session>
agency close <session>
agency close --terminal            # bulk close terminal sessions
agency worktree list
agency worktree status <workspace>
agency worktree diff <workspace>
agency worktree close <workspace>
agency model list [provider]
agency profile list
agency profile create <profile> --provider <p> [controls]
agency profile set <profile> [controls]
agency profile set-default <profile>
agency project init
agency project list
agency doctor [--supervisor]
agency repair <repair-key>
agency prune
agency export <path>
agency config get <key>
agency config set <key> <value>
```

### Global Flags

```text
--project <path-or-key>
--host <host-key>
--profile <profile-key>
--json
--no-color
--replay-key <key>
```

### `new` And `run` Flags

```text
--cwd <path>
--title <title>
--worktree
--no-worktree
--worktree-name <key>
--base <ref>
--model <model-key-or-provider-model>
--effort <provider-effort>
--permission-mode <provider-permission-mode>
--sandbox <provider-sandbox-mode>
--approval-policy <provider-approval-policy>
--add-dir <path>
--env <name=value>
--allow-dangerous
--command-preview
```

`--allow-dangerous` is the explicit request required before a dangerous
permission mode, sandbox mode, or approval/sandbox combination is accepted.
Provider-specific flags are accepted only through provider adapters that map them
into typed launch inputs.

### Exit Codes

- `0`: success.
- `1`: typed user or environment error.
- `2`: invalid command input.
- `3`: safety blocked the operation.
- `4`: supervisor unavailable.
- `5`: defect or repair-required state.

### JSON Output

JSON output emits schema-shaped objects with `PascalCase` enum values and
handles. It does not emit pretty table labels.

## TUI Design

The TUI is the default `agency` command.

### Dashboard View

The dashboard shows sessions grouped so every `RunStatus` maps to exactly one
group:

1. Needs input (`NeedsInput`).
2. Needs approval (`NeedsApproval`).
3. Live (`Live`, `Starting`, `Quiet`).
4. Terminal (`Exited`, `Stopped`, `Killed`, `Failed`).
5. Repair required (`RepairRequired`, `LostTmuxTarget`, `LostRunner`).
6. Closed (`Closed`, shown when `show closed` is enabled).

Columns: session key, provider, title, project, workspace, run status, git
summary, model, effort, last event, close summary.

### Session View

The session view shows the tmux attach action, recent runner output, provider
controls used at launch, workspace path, git summary, read-only diff summary,
active blockers, and the event timeline. It does not render provider private
transcripts.

### Launcher View

The launcher shows provider, profile, prompt editor, workspace mode, base ref,
model, effort, permission/sandbox controls, and the exact command preview for the
selected provider. The launch button is disabled when validation fails.

### Worktree View

The worktree view shows workspace key and handle, path, branch, base ref and base
SHA, linked sessions, git summary, diff summary, close summary with the full
blocker set, and marker status.

### Doctor View

The doctor view shows observations and explicit repair actions. It does not
perform hidden repairs.

## Feature Catalog

### F1 Project Initialization

Creates a project record for the current repository and records default
workspace root, managed worktree root, and default provider profile.

Acceptance:

- Running `agency project init` in a git repository creates one project record.
- Re-running with the same derived replay key returns the same project.
- Running outside a git repository returns `InvalidProjectRoot`.

### F2 Provider Profiles

Stores named provider launch defaults for Claude and Codex, created and updated
through `agency profile create`/`agency profile set`.

Acceptance:

- Profile updates create a new revision.
- Existing sessions keep their launch revision.
- Unsupported model/effort combinations block launch.

### F3 Session Launch

Starts a provider CLI in tmux through `agency-runner`.

Acceptance:

- One session record and one run record are created.
- Tmux target exists.
- Runner heartbeat is recorded.
- The exact argv token array is persisted; the env snapshot is redacted.

### F4 Managed Worktree Launch

Creates a managed worktree and starts a session there.

Acceptance:

- Worktree path is outside the project root unless configured otherwise.
- Marker file exists and matches workspace id.
- Original checkout files are not modified.
- Failure before publication leaves no active workspace row, and reconciliation
  records the on-disk worktree as an adoptable orphan.

### F5 Additional Run

Starts an additional run for an existing session in the same workspace,
resuming the provider session where supported.

Acceptance:

- A new `agent_runs` row with the next `run_seq` is created.
- The workspace is unchanged.
- A concurrent worktree close on that workspace serializes with the run start.

### F6 Dashboard Watch

Shows live sessions and status changes.

Acceptance:

- TUI restart reconstructs state from storage and live observations.
- TUI exit leaves sessions running.
- Deleting a tmux target externally produces `LostTmuxTarget`, and a runner
  quarantined by version mismatch produces `RepairRequired`.

### F7 Attach

Attaches the user's terminal to a tmux target.

Acceptance:

- Live tmux target opens.
- Missing tmux target does not create a false live status.
- Closed sessions cannot be attached.

### F8 Send Input

Routes input through the runner.

Acceptance:

- Input is acknowledged (byte delivery) before success returns.
- Failed acknowledgement is a typed error.
- Concurrent sends produce ordered input events, and a replay does not
  re-send.

### F9 Diff

Shows a read-only diff of a workspace against its base.

Acceptance:

- `agency diff` returns the diff against the pinned `base_sha`.
- No merge, commit, push, or file mutation occurs.

### F10 Stop

Gracefully stops a live run.

Acceptance:

- Stop records terminal outcome.
- Stop does not remove workspace files.
- Kill is unavailable until graceful stop fails or the user explicitly chooses
  the kill operation.

### F11 Close Session

Closes session visibility.

Acceptance:

- Live run blocks close unless a stop plan is accepted.
- Closed session remains in history.
- Workspace remains until separately closed.

### F12 Close Managed Worktree

Removes a managed worktree after guard checks.

Acceptance:

- Every applicable blocker (dirty, untracked, ignored user files, conflicts,
  unpushed, missing upstream proof, missing marker, live linked session) is
  recorded, and any non-empty set blocks.
- Clean owned worktree is removed with `git worktree remove` after handles are
  memoized and visibility is unpublished.

### F13 Doctor And Reconciliation

Reports broken or stale state and auto-reconciles on startup.

Acceptance:

- Read-only doctor mutates nothing.
- Missing provider CLI, missing tmux target, orphaned agency tmux targets, and
  orphaned agency branches are reported.
- Startup reconciliation adopts compatible runners, quarantines incompatible
  ones, and records orphans without deleting user work.
- Repair commands are explicit.

### F14 Remote Supervisor

Connects local CLI/TUI to a supervisor running on a devbox.

Acceptance:

- An SSH-reachable host can start/discover the supervisor and auto-start it on
  boot.
- The API rejects unauthenticated connections.
- Remote command failures are typed.
- A mosh-only host exposes attach-only capability.

### F15 Notifications

Delivers configured notifications for needs-input, needs-approval, stopped, and
failed events, and close-blocked outcomes.

Acceptance:

- Delivery failure does not change run state.
- Delivery attempts are recorded with bounded retry.
- Duplicate event delivery is deduplicated by event id and channel.

## Acceptance Criteria

The product is acceptable when:

- A user can start Claude and Codex sessions from CLI and TUI.
- Sessions run through `agency-runner` inside tmux.
- The TUI can be closed and reopened without stopping sessions.
- A supervisor restart adopts still-running compatible runners and never kills a
  runner on protocol mismatch.
- Worktree sessions create managed worktrees with marker files.
- Close operations block every data-loss scenario and record the full blocker
  set.
- Status survives client disconnect and supervisor restart, and startup
  reconciliation resolves crash windows.
- Remote devbox operation runs the supervisor on the devbox behind an
  authenticated transport.
- Tests cover real git and tmux behavior.
- No implementation depends on private Claude or Codex transcript formats.
- No command performs merge, commit, push, PR, or branch deletion.
- No persisted row contains a provider credential value.

## Verification Strategy

### Unit Tests

- Config parsing and ingress-to-owned-enum conversion.
- Key and handle parsing.
- Provider launch plan construction and argv token validation.
- Git porcelain and diff parsing.
- Safety blocker classification (full set).
- Status projection from events.
- Env redaction.

### Integration Tests

- SQLite migrations, pragmas, and row invariants.
- Sequence allocation under concurrent writers.
- Real temporary git repositories and worktrees.
- Real tmux sessions when tmux is installed.
- Runner child-process lifecycle and control-socket protocol, including
  supervisor-restart adoption and version-mismatch quarantine.
- Close workflow crash and replay.
- Startup reconciliation across simulated crash windows.

### E2E Tests

- Launch session, detach, reopen TUI, attach.
- Launch managed worktree session and verify the original checkout is untouched.
- Dirty worktree close is blocked, records blockers, and files remain.
- Stop live run then close session.
- Kill tmux externally and verify `LostTmuxTarget` then repair.
- Upgrade the runner binary, restart the supervisor, and verify adoption of the
  old runner and quarantine on forced mismatch.
- Use a remote test host or container for authenticated SSH supervisor
  discovery.

## Hard Cutover Rules

- The first implementation creates schema version `1`.
- Schema version `1` has no legacy migration inputs.
- Command names in this spec are the only supported command names.
- Provider keys are `claude` and `codex`.
- Runtime code does not include compatibility aliases for the database schema,
  command names, or config layout.
- The runner wire protocol is the one exception: it is versioned and adopts
  supported prior versions, because runners outlive supervisor upgrades.
- Unknown enum values are defects after storage decode.
- Unknown config keys are config errors.
- Unknown provider controls are command input errors.

## Deferred Decisions

The following are intentionally outside this target state and require their own
specs before implementation:

- Desktop GUI, browser UI, mobile UI.
- PR review integration, GitHub issue integration.
- Commit or push support.
- Guarded deletion of an agency-owned branch, allowed only when the branch is
  provably merged into its base and pushed, reusing the upstream-proof machinery
  from worktree close. Until then, orphaned branches are reported by Doctor, not
  deleted.
- Team sharing.
- Containerized per-worktree environments.
- Webhook notification channels beyond terminal and desktop.
- Bulk operations beyond closing terminal sessions.
- ACP client or server support.
- Native Codex app-server integration.
- Native Claude agent-view integration.
