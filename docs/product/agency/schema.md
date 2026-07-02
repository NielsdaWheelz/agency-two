# Agency Storage And Configuration Schema

## Status

This document defines the complete target storage and configuration schema for
`agency`. It owns database row shapes, config shape, handles, keys, lifecycle
facts, indexes, invariants, transaction boundaries, retention, and serialized
API payloads.

It is the single source of truth for every persisted enum. [spec.md](spec.md)
and [content-design.md](content-design.md) reference the enum names defined here
and must not introduce their own.

## Storage Decisions

- Primary store: SQLite in WAL mode. `journal_mode=WAL` is set once by the
  first migration; `foreign_keys=ON`, `busy_timeout`, and
  `journal_size_limit` are set on every connection (see [Connection And
  Writer Model](#connection-and-writer-model)).
- Config authoring format: TOML.
- API format: JSON.
- IDs: application-generated UUIDv7 private `*_id` values, stored in `text`
  columns as canonical lowercase UUID strings. SQLite has no `uuid` affinity;
  the bare token `uuid` yields NUMERIC affinity and would silently coerce
  values, so id columns are declared `text`.
- User-visible short references: entity-specific short handles.
- Durable event ordering: integer sequence numbers scoped to the owning entity,
  allocated inside the owning write transaction (see [Sequence
  Allocation](#sequence-allocation)).
- Instants: stored as ISO-8601 UTC text with a `Z` suffix and millisecond
  precision (`YYYY-MM-DDTHH:MM:SS.sssZ`), written by the supervisor's UTC clock,
  which is authoritative for every timestamp it stores. Intervals are right-open
  `[start, end)`; a TTL is active while `now < expires_at` and expired once
  `now >= expires_at`.
- Structured JSON: stored in SQLite's JSON representation and bound through one
  JSON adapter that keeps SQLite `NULL` distinct from JSON `null`. No structured
  JSON is stored as ad hoc plain text, and no query site writes raw JSON casts.
- Secrets: agency stores no long-lived credentials. Persisted environment
  snapshots are redacted (see [`agent_runs`](#agent_runs) and
  [Secret Handling](#secret-handling)). Any auth verifier uses a
  domain-separated `*_hash` column, never a raw credential.
- No database triggers.
- No cascade deletes. Cleanup is explicit in application code and ordered child
  before parent.
- No business `CHECK` constraints, exclusion constraints, or other
  database-enforced business invariants. Conditional nullability, tagged-union
  branch consistency, cross-column correlation, ownership, and lifecycle-state
  rules live in application code plus defects (see [Schema
  Invariants](#schema-invariants)).
- No speculative indexes. Indexes exist only for query patterns this schema and
  [spec.md](spec.md) actually describe.
- No generic unstructured metadata columns. Every `*_json` column has a typed
  shape defined in [JSON Payload Shapes](#json-payload-shapes).

The schema stores durable facts. Status labels are derived from facts and fresh
observations.

## Enum Encoding Contract

Three vocabularies cross this system. They are encoded by their owner, and the
config parser is the boundary that converts ingress vocabulary into owned enums.

- Agency-owned enums are `PascalCase` strings per
  [naming.md](../../rules/naming.md). This is the representation in code, in
  persisted enum columns, and in JSON egress (including `--json` output). Their
  authoritative value sets live in [Canonical Enums](#canonical-enums).
- Config TOML values are human-authored ingress vocabulary and use idiomatic
  lowercase or kebab (`worktree_mode = "prompt"`, `theme = "system"`). The
  config service parses them into owned `PascalCase` enums at ingress; downstream
  code never sees the raw TOML token.
- Provider vocabularies are external contracts and are preserved verbatim
  (`gpt-5.5`, `sonnet`, `opusplan`, `workspace-write`, `read-only`,
  `danger-full-access`, `bypassPermissions`, `acceptEdits`, `on-request`,
  `untrusted`, `never`, `high`, `xhigh`, `minimal`). Agency does not restyle
  them.

Field and object keys are `camelCase` (`runStatus`, `gitStatus`, `close`). They
are field paths, not enum values, and are not subject to the enum rule.

## Paths

Default paths on Unix:

```text
config:  $XDG_CONFIG_HOME/agency/config.toml or ~/.config/agency/config.toml
state:   $XDG_STATE_HOME/agency/agency.db or ~/.local/state/agency/agency.db
lock:    <state dir>/agency.db.lock
runtime: $XDG_RUNTIME_DIR/agency/supervisor.sock
runners: $XDG_RUNTIME_DIR/agency/runners/<run_id>.sock
logs:    $XDG_STATE_HOME/agency/logs/
```

Path values are parsed at config ingress and stored as canonical absolute paths.

The state directory is created mode `0700`. `agency.db` and its `-wal`/`-shm`
sidecars are created mode `0600`. The supervisor asserts these modes on startup
and refuses to serve if they are wider, because run rows can hold redacted
environment snapshots and event payloads.

## Handle And Key Types

Private identities (UUIDv7, never exposed at end-user boundaries):

- `ProjectId`, `RepositoryId`, `HostId`, `ProviderId`, `ProviderModelId`
- `ProfileId`, `ProfileRevisionId`
- `WorkspaceId`, `ManagedWorktreeId`
- `TmuxServerId`, `TmuxTargetId`
- `SessionId`, `RunId`, `RunnerBindingId`
- `SafetyPolicyId`, `SafetyCheckRunId`, `SafetyCheckFindingId`
- `CloseAttemptId`, `CloseBlockerId`, `EventId`
- `ConfigSourceId`, `ConfigRevisionId`, `StatusSubjectId`, `StatusSnapshotId`
- `NotificationChannelId`, `NotificationDeliveryId`, `IdempotencyKeyId`

Meaningful keys (owned, parsed, stable identity):

- `ProjectKey`, `RepositoryKey`, `HostKey`, `ProviderKey`, `ModelKey`
- `ProfileKey`, `WorkspaceKey`
- `TmuxServerKey`, `TmuxTargetKey`
- `SessionKey`, `SafetyPolicyKey`, `NotificationChannelKey`

A `RunKey` is the session-scoped pair `(SessionKey, run_seq)`; runs have no
global meaningful key because `run_seq` is only unique within a session.

Short handles (compact aliases, resolved server-side, not authority-bearing):

| Handle | Prefix | Example | Resolves to |
| --- | --- | --- | --- |
| `ProjectHandle` | `prj_` | `prj_9f21a0` | `ProjectId` |
| `WorkspaceHandle` | `wks_` | `wks_7a23c1` | `WorkspaceId` |
| `SessionHandle` | `ses_` | `ses_f83a91` | `SessionId` |
| `RunHandle` | `run_` | `ses_f83a91/run_1` | `(SessionId, run_seq)` |
| `CloseAttemptHandle` | `cls_` | `cls_4b7d02` | `CloseAttemptId` |

`RunHandle` is session-scoped: it renders as `run_<run_seq>` and is only
resolvable with its session, so it is always displayed session-qualified
(`ses_f83a91/run_1`). All other handles are globally resolvable.

Short handles resolve server-side to typed private ids and are then authorized
against the current scope. They do not authorize actions. They are the
intentional short-alias exception to sealed handles; sealing is waived because
`agency` is a single-user local control plane and handles must be typed and
copied on the terminal.

## Canonical Enums

Every value is `PascalCase`. Unknown values are defects after storage decode.
All finite enums are matched exhaustively per
[control-flow.md](../../rules/control-flow.md).

### `RunStatus`

Projection over durable run facts and fresh observation.

- `Starting`, `Live`, `NeedsInput`, `NeedsApproval`, `Quiet`
- `Exited`, `Stopped`, `Killed`, `Failed`
- `LostTmuxTarget`, `LostRunner`
- `Closed`, `RepairRequired`

`Failed` projects the `RunnerFailed`, `StartFailed`, and `Orphaned` termination
outcomes. `LostTmuxTarget` and `LostRunner` are intentionally-modeled
external-disappearance outcomes, not projection drift. `RepairRequired` is the
narrow defect boundary: durable facts that contradict each other, never a
routine external observation.

### `TerminationOutcome`

Persisted in [`run_terminal_outcomes`](#run_terminal_outcomes).

- `ProviderExited`, `UserStopped`, `UserKilled`
- `RunnerFailed`, `StartFailed`, `Orphaned`

### `GitSummary` and git axes

Git state is multi-axis; a worktree can be dirty and ahead at once. The
authoritative model is a record of orthogonal closed axes; `GitSummary` is the
single label derived from them for dense display, by the priority order listed.

Axes:

- `presence`: `Present`, `Missing`, `NotAWorktree`
- `tree`: `Clean`, `Dirty`
- `untracked`: bool
- `ignoredUserFiles`: bool
- `conflicts`: bool
- `upstream`: `Current`, `Ahead`, `Behind`, `Diverged`, `NoUpstream`, `Detached`

`GitSummary` (derived, highest-priority axis first): `NotAWorktree`,
`MissingWorktree`, `DetachedHead`, `Conflicts`, `Dirty`, `Untracked`,
`IgnoredUserFiles`, `Diverged`, `Behind`, `Ahead`, `Clean`.

### `CloseBlocker`

Persisted in [`close_blockers`](#close_blockers). Close eligibility is a set of
blockers, not one value; a workspace can trip several at once.

- `SessionStillRunning`
- `WorkspaceShared`
- `WorktreeDirty`
- `WorktreeHasUntrackedFiles`
- `WorktreeHasIgnoredUserFiles`
- `WorktreeHasConflicts`
- `BranchHasUnpushedCommits`
- `NoRemoteTrackingProof`
- `OwnershipMarkerMismatch`
- `PathOutsideWorkspaceRoot`
- `GitWorktreeMissing`
- `NotExpectedWorktree`
- `BranchCheckedOutElsewhere`
- `TmuxTargetMissing`
- `RunnerHeartbeatExpired`

### `CloseSummary`

Derived single label for dense display over the blocker set:

- `Closable` (empty blocker set)
- `StopRequired` (`SessionStillRunning`)
- `SharedWorkspaceBlocked` (`WorkspaceShared`)
- `DirtyWorktreeBlocked` (`WorktreeDirty`)
- `UntrackedFilesBlocked` (`WorktreeHasUntrackedFiles`)
- `IgnoredUserFilesBlocked` (`WorktreeHasIgnoredUserFiles`)
- `ConflictsBlocked` (`WorktreeHasConflicts`)
- `UnpushedCommitsBlocked` (`BranchHasUnpushedCommits` or `NoRemoteTrackingProof`)
- `RepairRequired` (`OwnershipMarkerMismatch`, `PathOutsideWorkspaceRoot`,
  `GitWorktreeMissing`, `NotExpectedWorktree`, `BranchCheckedOutElsewhere`,
  `TmuxTargetMissing`, `RunnerHeartbeatExpired`)

When multiple blockers apply, the summary is the highest-priority one in the
order above; the full set is always available in the structured payload.

### Other owned enums

- `Severity`: `Info`, `Warning`, `Blocker`, `Defect`.
- `HostAccessMode` (discriminant `mode`): `Local`, `Ssh`, `AttachOnlyMosh`.
- `StatusSubjectKind` (discriminant `kind`): `Session`, `Run`, `Workspace`,
  `Host`.
- `OutputStream`: `Stdout`, `Stderr`.
- `StatusSource`: `Reconciliation`, `Poll`, `Event`.
- `RepositoryRole`: `Primary`.
- `WorktreeMode`: `Prompt`, `Always`, `Never` (config ingress `prompt`,
  `always`, `never`).
- `Theme`: `System`, `Light`, `Dark` (config ingress `system`, `light`,
  `dark`). The UI display theme; owned by the config service.
- `SafetyCheckKind`: `LaunchPolicy`, `StopPolicy`, `WorktreeClose`,
  `RemoteCommand`.
- `NotificationChannelType`: `Terminal`, `Desktop`.
- `EventType`: see [`events`](#events).

## Database Tables

Column type conventions: `text` id columns hold canonical UUIDv7 strings;
`timestamp` denotes ISO-8601 UTC text; `json` denotes SQLite JSON bound through
the JSON adapter; `integer` is a signed 64-bit integer. Enum columns are `text`
holding a `PascalCase` value from [Canonical Enums](#canonical-enums).

### `schema_migrations`

```text
version text primary key
applied_at timestamp not null
```

Records applied migrations.

### `config_sources`

```text
config_source_id text primary key
source_key text not null unique
path text not null
priority integer not null
created_at timestamp not null
disabled_at timestamp null
```

One row per config source file. Higher `priority` wins on key conflict during
config merge (see [Config Precedence](#config-precedence)).

### `config_revisions`

```text
config_revision_id text primary key
config_source_id text not null references config_sources(config_source_id)
content_hash text not null
effective_config_json json not null
loaded_at timestamp not null
```

`effective_config_json` is the typed effective config payload.

### `projects`

```text
project_id text primary key
project_key text not null unique
display_name text not null
root_path text not null
default_base_ref text not null
default_worktree_mode text not null
managed_worktree_root_path text not null
created_at timestamp not null
archived_at timestamp null
```

Project is active when `archived_at` is null.

### `repositories`

```text
repository_id text primary key
repository_key text not null unique
origin_url text null
git_common_dir_path text not null
default_branch_ref text not null
created_at timestamp not null
archived_at timestamp null
```

`origin_url` is null when a local repository has no remote. The boundary adapter
converts this null to owned absence before returning domain values; downstream
code never sees raw `null`.

### `project_repositories`

```text
project_repository_id text primary key
project_id text not null references projects(project_id)
repository_id text not null references repositories(repository_id)
role text not null
created_at timestamp not null
```

Unique key: `(project_id, role)`. `role` holds a `RepositoryRole`; the first
target state uses `Primary`.

### `hosts`

```text
host_id text primary key
host_key text not null unique
display_name text not null
access_spec_json json not null
created_at timestamp not null
archived_at timestamp null
```

`access_spec_json` is a `HostAccessSpec` (see [JSON Payload
Shapes](#json-payload-shapes)). Mosh hosts are attach-only.

### `agent_providers`

```text
provider_id text primary key
provider_key text not null unique
display_name text not null
command_path text not null
provider_config_json json not null
created_at timestamp not null
disabled_at timestamp null
```

Initial provider keys: `claude`, `codex`.

### `provider_models`

```text
provider_model_id text primary key
provider_id text not null references agent_providers(provider_id)
model_key text not null
provider_model_name text not null
model_spec_json json not null
created_at timestamp not null
retired_at timestamp null
```

Unique key: `(provider_id, model_key)`. `model_spec_json` is a `ModelSpec`
carrying supported effort values and per-model availability notes. Supported
provider controls live in `agent_providers.provider_config_json`.

### `agent_profiles`

```text
profile_id text primary key
profile_key text not null unique
display_name text not null
created_at timestamp not null
archived_at timestamp null
```

### `agent_profile_revisions`

```text
profile_revision_id text primary key
profile_id text not null references agent_profiles(profile_id)
revision_seq integer not null
provider_id text not null references agent_providers(provider_id)
provider_model_id text null references provider_models(provider_model_id)
effort_key text null
permission_policy_json json not null
runtime_limits_json json not null
extra_args_json json not null
created_at timestamp not null
```

Unique key: `(profile_id, revision_seq)`. Historical sessions pin one profile
revision. Changing defaults creates a new revision. `effort_key` is null when
the selected provider/model exposes no effort control.

### `agent_profile_current_revisions`

```text
profile_id text primary key references agent_profiles(profile_id)
profile_revision_id text not null references agent_profile_revisions(profile_revision_id)
selected_at timestamp not null
```

One-to-one row for the selected revision.

### `project_profile_defaults`

```text
project_profile_default_id text primary key
project_id text not null references projects(project_id)
profile_id text not null references agent_profiles(profile_id)
created_at timestamp not null
```

Unique key: `(project_id)`.

### `workspaces`

```text
workspace_id text primary key
workspace_key text not null unique
project_id text not null references projects(project_id)
repository_id text not null references repositories(repository_id)
host_id text not null references hosts(host_id)
path text not null
created_at timestamp not null
closed_at timestamp null
```

A workspace is the directory in which a session runs. Project-root and
managed-worktree workspaces share this durable concept.

### `project_root_workspaces`

```text
project_root_workspace_id text primary key
workspace_id text not null unique references workspaces(workspace_id)
created_at timestamp not null
```

Row exists when the workspace is the project root. It is never removed by
`agency`.

### `managed_worktrees`

```text
managed_worktree_id text primary key
workspace_id text not null unique references workspaces(workspace_id)
branch_name text not null
base_ref text not null
base_sha text not null
initial_head_sha text not null
marker_file_path text not null
marker_file_hash text not null
created_at timestamp not null
```

Row exists only for agency-owned worktrees. This is the neutral resource row of
the resource-plus-state split; [`active_worktrees`](#active_worktrees) and
[`removed_worktrees`](#removed_worktrees) are its state rows.

### `active_worktrees`

```text
active_worktree_id text primary key
managed_worktree_id text not null unique references managed_worktrees(managed_worktree_id)
published_at timestamp not null
```

Presence means the managed worktree is visible through normal list and session
launch paths.

### `removed_worktrees`

```text
removed_worktree_id text primary key
managed_worktree_id text not null unique references managed_worktrees(managed_worktree_id)
removed_at timestamp not null
removal_reason_json json not null
```

Presence means the managed worktree close completed. Removal passes through the
invisible teardown state in [`closing_worktrees`](#closing_worktrees), because
the external `git worktree remove` effect cannot run inside a database
transaction.

### `closing_worktrees`

```text
closing_worktree_id text primary key
managed_worktree_id text not null unique references managed_worktrees(managed_worktree_id)
close_attempt_id text not null references close_attempts(close_attempt_id)
memo_json json not null
created_at timestamp not null
```

Presence means the managed worktree is unpublished from normal product paths and
is in teardown. `memo_json` carries the resolved path, git identity, and support
handles required to replay teardown after a crash.

### `tmux_servers`

```text
tmux_server_id text primary key
tmux_server_key text not null unique
host_id text not null references hosts(host_id)
socket_spec_json json not null
server_identity_json json null
created_at timestamp not null
retired_at timestamp null
```

`server_identity_json` records the live tmux server instance identity
(`{pid, startedAt}` read from `tmux display -p '#{pid}/#{start_time}'`) observed
at last reconciliation. A changed or absent identity means the tmux server
restarted (for example on host reboot), and all targets and bindings under it
are bulk-transitioned to orphaned during reconciliation.

### `tmux_targets`

```text
tmux_target_id text primary key
tmux_target_key text not null unique
host_id text not null references hosts(host_id)
tmux_server_id text not null references tmux_servers(tmux_server_id)
target_spec_json json not null
created_at timestamp not null
retired_at timestamp null
```

`target_spec_json` is a `TmuxTargetSpec` storing the expected session, window,
and pane names.

### `agent_sessions`

```text
session_id text primary key
session_key text not null unique
project_id text not null references projects(project_id)
workspace_id text not null references workspaces(workspace_id)
profile_revision_id text not null references agent_profile_revisions(profile_revision_id)
host_id text not null references hosts(host_id)
title text not null
created_at timestamp not null
closed_at timestamp null
```

Session is open when `closed_at` is null. `project_id` and `host_id` are direct
relationships (a session belongs to a project and runs on a host), used by the
dashboard-grouping indexes, not derived discriminators. The initial prompt is a
run input, not a session column.

### `session_tmux_targets`

```text
session_tmux_target_id text primary key
session_id text not null references agent_sessions(session_id)
tmux_target_id text not null references tmux_targets(tmux_target_id)
attached_at timestamp not null
detached_at timestamp null
```

An active tmux association has `detached_at` null. A session has at most one
active target and a target serves at most one session, enforced by partial
unique indexes (see [Indexes](#indexes)).

### `agent_runs`

```text
run_id text primary key
session_id text not null references agent_sessions(session_id)
run_seq integer not null
launch_argv_json json not null
launch_env_json json not null
working_directory text not null
requested_at timestamp not null
started_at timestamp null
```

Unique key: `(session_id, run_seq)`. Run start intent is durable before the
external process starts. `launch_argv_json` is the exact argv token array (never
a concatenated string). `launch_env_json` is a redacted `LaunchEnv` (see
[Secret Handling](#secret-handling)): it records the agency/profile-set env
delta and the names of inherited variables, with secret values replaced by
`{present:true,source:"inherited"}`. No credential value is persisted.

### `active_runner_bindings`

```text
runner_binding_id text primary key
run_id text not null unique references agent_runs(run_id)
tmux_target_id text not null references tmux_targets(tmux_target_id)
runner_endpoint_json json not null
runner_protocol_version integer not null
runner_binary_version text not null
process_group_ref text null
last_heartbeat_at timestamp not null
bound_at timestamp not null
```

Presence means the supervisor expects a live runner. Liveness still requires a
fresh heartbeat evaluated against a monotonic clock (see [spec.md Time And
Clocks](spec.md#time-and-clocks)). `runner_endpoint_json` is a `RunnerEndpoint`.
`runner_protocol_version` and `runner_binary_version` are captured at bind time
so a restarted supervisor can adopt a compatible runner or quarantine an
incompatible one instead of killing it. `process_group_ref` records the child
process-group id for last-resort kill when the control socket is dead.

### `run_terminal_outcomes`

```text
run_terminal_outcome_id text primary key
run_id text not null unique references agent_runs(run_id)
outcome text not null
termination_json json not null
occurred_at timestamp not null
```

`outcome` is a `TerminationOutcome`. `termination_json` is a `Termination`
tagged union whose branch is selected by `outcome`, carrying exit code, signal,
or failure detail (see [JSON Payload Shapes](#json-payload-shapes)). The
binding→terminal-outcome transition is one serializable transaction that deletes
[`active_runner_bindings`](#active_runner_bindings) and inserts this row.

### `runner_output_chunks`

```text
runner_output_chunk_id text primary key
run_id text not null references agent_runs(run_id)
chunk_seq integer not null
stream text not null
content_ref_json json not null
captured_at timestamp not null
```

Unique key: `(run_id, chunk_seq)`. `stream` is an `OutputStream`.
`content_ref_json` is a `ContentRef` (inline bytes or a spool-file reference).
Subject to retention (see [Retention And Compaction](#retention-and-compaction)).

### `run_input_events`

```text
run_input_event_id text primary key
run_id text not null references agent_runs(run_id)
input_seq integer not null
input_ref_json json not null
delivery_json json not null
created_at timestamp not null
```

Unique key: `(run_id, input_seq)`. `delivery_json` is an `InputDelivery` tagged
union (`Pending`, `Accepted{at}`, `Failed{at, failure}`); acceptance means bytes
were delivered to the provider PTY and acknowledged by the runner, not that the
provider consumed them.

### `status_subjects`

```text
status_subject_id text primary key
subject_json json not null
subject_hash text not null unique
created_at timestamp not null
```

`subject_json` is a `StatusSubject` (discriminant `kind`, see [JSON Payload
Shapes](#json-payload-shapes)). `subject_hash` deduplicates subjects.

### `status_snapshots`

```text
status_snapshot_id text primary key
status_subject_id text not null references status_subjects(status_subject_id)
captured_at timestamp not null
source text not null
snapshot_hash text not null
snapshot_json json not null
```

`source` is a `StatusSource`. Snapshots are written only when the snapshot hash
changes, and are pruned by retention; [`latest_status_snapshots`](#latest_status_snapshots)
is the authoritative projection.

### `latest_status_snapshots`

```text
status_subject_id text primary key references status_subjects(status_subject_id)
status_snapshot_id text not null references status_snapshots(status_snapshot_id)
updated_at timestamp not null
```

Rebuildable projection.

### `safety_policies`

```text
safety_policy_id text primary key
safety_policy_key text not null unique
policy_json json not null
created_at timestamp not null
archived_at timestamp null
```

`policy_json` is a `SafetyPolicy` (the parsed `[safety.policies.*]` block).

### `safety_check_runs`

```text
safety_check_run_id text primary key
safety_policy_id text not null references safety_policies(safety_policy_id)
status_subject_id text not null references status_subjects(status_subject_id)
check_kind text not null
started_at timestamp not null
completed_at timestamp null
failed_at timestamp null
failure_json json null
```

`check_kind` is a `SafetyCheckKind`. `completed_at` and `failed_at` are mutually
exclusive; `failure_json` is present iff `failed_at` is present.

### `safety_check_findings`

```text
safety_check_finding_id text primary key
safety_check_run_id text not null references safety_check_runs(safety_check_run_id)
blocker text null
severity text not null
location_json json not null
message text not null
evidence_json json not null
created_at timestamp not null
```

`severity` is a `Severity`. `blocker` is a `CloseBlocker` when the finding is a
close blocker, and null for launch/stop findings. `location_json` and
`evidence_json` are typed (see [JSON Payload Shapes](#json-payload-shapes)).
`Defect` severity denotes a finding about the user's environment or git state,
never an internal agency invariant break; internal invariant breaks are defects
in code, not rows.

### `close_attempts`

```text
close_attempt_id text primary key
close_attempt_key text not null unique
target_json json not null
requested_by_actor_json json not null
requested_at timestamp not null
completed_at timestamp null
failed_at timestamp null
failure_json json null
```

`target_json` is a `StatusSubject`. `requested_by_actor_json` is an `Actor`.

### `close_blockers`

```text
close_blocker_id text primary key
close_attempt_id text not null references close_attempts(close_attempt_id)
blocker text not null
summary text not null
evidence_json json not null
created_at timestamp not null
```

`blocker` is a `CloseBlocker`. Blockers are evidence for one close attempt only.

### `events`

```text
event_id text primary key
status_subject_id text not null references status_subjects(status_subject_id)
event_seq integer not null
occurred_at timestamp not null
event_type text not null
actor_json json not null
payload_json json not null
correlation_json json not null
causation_event_id text null references events(event_id)
```

Append-only event log. `status_subject_id` references the deduplicated subject
so events are joinable and indexable by subject. `event_seq` is allocated inside
the writing transaction and is unique per `status_subject_id`. `event_type` is
an `EventType`; `payload_json` is the `EventPayload` variant selected by
`event_type`. `correlation_json` is a `Correlation` `{ "correlationId": text }`
grouping events that belong to one logical operation; `causation_event_id` is
the nullable in-log parent that directly caused this event (for example a
`RunStopped` caused by a `StopRequested`), forming an intra-log causation chain.

`EventType` values:

- `SessionCreated`, `RunRequested`, `RunnerHeartbeatAccepted`
- `RunNeedsInput`, `RunNeedsApproval`, `InputAccepted`
- `ProviderProcessExited`, `StopRequested`, `RunStopped`, `RunKilled`, `RunFailed`
- `CloseAttemptStarted`, `CloseBlocked`, `SessionClosed`, `ManagedWorktreeRemoved`
- `DoctorIssueObserved`, `RepairCompleted`
- `RunnerAdopted`, `RunnerQuarantined`, `TmuxServerRestarted`, `WorktreeReconciled`

### `notification_channels`

```text
notification_channel_id text primary key
notification_channel_key text not null unique
channel_type text not null
channel_spec_json json not null
created_at timestamp not null
disabled_at timestamp null
```

`channel_type` is a `NotificationChannelType`. `channel_spec_json` is a
`NotificationChannelSpec` and holds no secret material; any credential is an
external secret reference.

### `notification_deliveries`

```text
notification_delivery_id text primary key
notification_channel_id text not null references notification_channels(notification_channel_id)
event_id text not null references events(event_id)
attempt_seq integer not null
attempted_at timestamp not null
delivered_at timestamp null
failed_at timestamp null
failure_json json null
```

Unique key: `(notification_channel_id, event_id)`. `attempt_seq` counts bounded
retries owned by the notification worker; a terminal `failed_at` records
exhaustion. Delivery rows never influence canonical run state.

### `idempotency_keys`

```text
idempotency_key_id text primary key
replay_key text not null unique
operation_key text not null
request_hash text not null
result_ref_json json null
state text not null
created_at timestamp not null
completed_at timestamp null
```

Backs replay identity for durable and replayable-single-mutation operations. The
`replay_key` is derived from the request payload namespaced by `operation_key`
(see [spec.md Operation Model](spec.md#operation-model)). `state` is `InFlight`
or `Completed`; a repeat key returns the stored `result_ref_json` or blocks
while in flight. Pruned by retention after completion.

## JSON Payload Shapes

Every `*_json` column decodes to a named typed value. Discriminated unions use a
semantic discriminant (`mode`, `kind`) per
[tagged-unions.md](../../rules/tagged-unions.md). Absence inside owned payloads
uses the owned absence representation, not JSON `null`.

- `HostAccessSpec` (discriminant `mode`):

  ```json
  { "mode": "Local" }
  { "mode": "Ssh", "hostAlias": "devbox", "socketForwarding": true }
  { "mode": "AttachOnlyMosh", "hostAlias": "mosh-box" }
  ```

- `StatusSubject` (discriminant `kind`), used internally by `status_subjects`,
  `events`, and `close_attempts` with private ids:

  ```json
  { "kind": "Session", "sessionId": "..." }
  { "kind": "Run", "runId": "..." }
  { "kind": "Workspace", "workspaceId": "..." }
  { "kind": "Host", "hostId": "..." }
  ```

  Public API payloads carry the handle form (`{ "kind": "Workspace",
  "workspace": "wks_7a23c1" }`), never private ids.

- `Termination` (discriminant matches `outcome`):

  ```json
  { "outcome": "ProviderExited", "exitCode": 0 }
  { "outcome": "UserStopped" }
  { "outcome": "UserKilled", "signal": "SIGKILL" }
  { "outcome": "RunnerFailed", "failure": { "code": "PtyClosed", "detail": "..." } }
  { "outcome": "StartFailed", "failure": { "code": "SpawnFailed", "detail": "..." } }
  { "outcome": "Orphaned", "detail": "runner unreachable past TTL" }
  ```

- `InputDelivery` (discriminant `state`): `{ "state": "Pending" }`,
  `{ "state": "Accepted", "at": "..." }`, `{ "state": "Failed", "at": "...",
  "failure": { "code": "...", "detail": "..." } }`.
- `RunnerEndpoint`: `{ "kind": "UnixSocket", "path":
  "$XDG_RUNTIME_DIR/agency/runners/<run_id>.sock" }`.
- `ContentRef`: `{ "kind": "Inline", "bytesBase64": "..." }` or `{ "kind":
  "SpoolFile", "path": "...", "offset": 0, "length": 4096 }`.
- `LaunchEnv`: `{ "set": { "AGENCY_RUN_ID": "..." }, "inheritedNames":
  ["PATH", "HOME"], "redacted": ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"] }`.
- `ModelSpec`: `{ "efforts": ["low", "medium", "high"], "availability":
  "general", "notes": "..." }`.
- `ProviderConfig`: `{ "permissionModes": ["default"], "sandboxModes":
  ["workspace-write"], "approvalPolicies": ["on-request"] }`.
- `Correlation`: `{ "correlationId": text }` — groups events of one logical
  operation (see [`events`](#events)).
- `SafetyPolicy`, `NotificationChannelSpec`, `TmuxTargetSpec`, `SocketSpec`,
  `PermissionPolicy`, `RuntimeLimits`, `ExtraArgs`, `ProviderConfig`,
  `EffectiveConfig`, `Actor`, `Location`, `Evidence`, `RemovalReason`,
  `Correlation`, `EventPayload` each have a named schema owned by this document's
  config and storage model; every one is a closed shape, not an open bag.

## Indexes

Composite alternate keys and one-to-one state links (declared as unique indexes;
inline `unique`/`primary key` constraints are not repeated here):

```text
project_repositories(project_id, role)               unique
provider_models(provider_id, model_key)              unique
agent_profile_revisions(profile_id, revision_seq)    unique
project_profile_defaults(project_id)                 unique
agent_runs(session_id, run_seq)                       unique
runner_output_chunks(run_id, chunk_seq)              unique
run_input_events(run_id, input_seq)                  unique
events(status_subject_id, event_seq)                 unique
notification_deliveries(notification_channel_id, event_id)  unique
session_tmux_targets(session_id) where detached_at is null      unique
session_tmux_targets(tmux_target_id) where detached_at is null  unique
```

The two partial unique indexes enforce at most one active tmux association per
session and per target.

Query indexes (each backs a described access path; none speculative):

```text
projects(archived_at, created_at)
repositories(archived_at, repository_key)
hosts(archived_at, host_key)
workspaces(project_id, closed_at, created_at)
workspaces(host_id, path)
agent_sessions(project_id, closed_at, created_at)
agent_sessions(workspace_id, closed_at)
agent_sessions(host_id, closed_at)
agent_runs(session_id, requested_at)
active_runner_bindings(last_heartbeat_at)
active_runner_bindings(tmux_target_id)
session_tmux_targets(session_id)
session_tmux_targets(tmux_target_id)
config_revisions(config_source_id)
status_snapshots(status_subject_id, captured_at)
safety_check_runs(status_subject_id, started_at)
safety_check_findings(safety_check_run_id)
close_attempts(requested_at)
close_blockers(close_attempt_id)
events(occurred_at)
events(status_subject_id, occurred_at)
events(event_type, occurred_at)
events(causation_event_id)
notification_deliveries(event_id)
```

## Connection And Writer Model

- Exactly one supervisor writes one state database (see [spec.md Supervisor
  Singleton](spec.md#supervisor-singleton)). WAL then gives concurrent readers a
  single consistent writer.
- Every connection sets `PRAGMA foreign_keys=ON` (SQLite defaults it off and it
  is per-connection, so every `references` clause is otherwise inert),
  `PRAGMA busy_timeout=<configured>`, and `PRAGMA journal_size_limit=<configured>`.
  The supervisor asserts `foreign_keys` is active after connecting and defects
  otherwise.
- Write transactions run at serializable-equivalent isolation. A serialization
  conflict retries through the database family's retrying primitive and defects
  on retry-budget exhaustion.
- Transactions are scoped to the single atomic boundary they protect. Multi-step
  durable flows commit each step in its own transaction and compose through the
  durable operation model, not one wide transaction.

Transactions that must be atomic (readers must never observe neither or both
rows):

- Allocating a sequence number and inserting the row that owns it.
- The active→closing worktree transition (delete `active_worktrees`, insert
  `closing_worktrees`).
- The closing→removed worktree transition (delete `closing_worktrees`, insert
  `removed_worktrees`).
- The binding→terminal-outcome transition (delete `active_runner_bindings`,
  insert `run_terminal_outcomes`).
- Writing an `idempotency_keys` row together with the single-mutation effect it
  guards.

## Sequence Allocation

`run_seq`, `revision_seq`, `chunk_seq`, `input_seq`, and subject-scoped
`event_seq` are allocated as `MAX(seq)+1` for the owner, computed and inserted
inside the same serializable write transaction that inserts the owning child row.
A concurrent allocation surfaces as a unique-constraint or serialization failure
and retries. Sequences are monotonic and gap-tolerant: a rolled-back transaction
may skip a value. Allocation is safe under the single-writer model.

## Retention And Compaction

Append-only and high-churn tables are pruned by an application-owned compactor
running on the supervisor, plus the `agency prune` maintenance operation. Cleanup
is explicit and ordered child before parent (no cascades). Governed by the
`[retention]` config block:

- `runner_output_chunks`: inline content is size-capped; larger output spills to
  rotating spool files under `logs/`. Chunks for closed sessions older than
  `output_retention` are pruned, keeping a bounded tail.
- `status_snapshots`: only change-on-hash snapshots are written, bounded to a
  short rolling window; `latest_status_snapshots` remains authoritative.
- `events`: a hot window stays in SQLite; events for closed sessions past
  `event_retention` are archived to `logs/` and pruned.
- `idempotency_keys`, `notification_deliveries`, `close_attempts`, and
  `safety_check_*` rows are pruned past their retention window.
- The compactor runs `PRAGMA wal_checkpoint(TRUNCATE)` and incremental vacuum on
  its schedule; `auto_vacuum=INCREMENTAL` is set at migration time.

`agency export` takes a consistent backup via SQLite's online backup after a WAL
checkpoint.

## Config TOML Schema

```toml
version = 1

[paths]
state_dir = "~/.local/state/agency"
runtime_dir = "$XDG_RUNTIME_DIR/agency"
log_dir = "~/.local/state/agency/logs"

[defaults]
project = "agency_two"
host = "local"
profile = "codex_default"
worktree_mode = "prompt"   # prompt | always | never
base_ref = "origin/main"

[ui]
theme = "system"
refresh_ms = 1000
show_closed = false

[timing]
heartbeat_interval_ms = 2000
heartbeat_ttl_ms = 10000       # >= several intervals; monotonic liveness
quiet_threshold_ms = 30000
graceful_stop_timeout_ms = 10000
reconcile_interval_ms = 15000
suspend_resume_reset = true    # reset liveness baseline on large wall-clock jumps

[retention]
output_retention_days = 14
event_retention_days = 90
status_snapshot_window = 50
idempotency_retention_days = 7
busy_timeout_ms = 5000
journal_size_limit_bytes = 67108864

[security]
socket_peer_credential_check = true   # verify SO_PEERCRED on the Unix socket
tcp_tunnel_requires_token = true      # bearer token required for any TCP tunnel
tcp_tunnel_loopback_only = true

[hosts.local]
display_name = "Local"
access = { mode = "local" }
tmux_server = "default"

[hosts.devbox]
display_name = "Devbox"
access = { mode = "ssh", host_alias = "devbox", socket_forwarding = true }
tmux_server = "default"

[hosts.mosh_box]
display_name = "Mosh Box"
access = { mode = "attach_only_mosh", host_alias = "mosh-box" }
tmux_server = "default"

[tmux.servers.default]
socket = "default"
session_prefix = "agency"

[providers.claude]
display_name = "Claude Code"
command = "claude"

[providers.claude.controls]
# Provider vocabulary, verbatim. Curated allowlist, not exhaustive.
models = ["sonnet", "opus", "opusplan", "haiku", "fable"]
efforts = ["low", "medium", "high", "xhigh", "max"]
permission_modes = ["default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions"]

[providers.codex]
display_name = "Codex"
command = "codex"

[providers.codex.controls]
models = ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"]
efforts = ["minimal", "low", "medium", "high", "xhigh"]
sandbox_modes = ["read-only", "workspace-write", "danger-full-access"]
approval_policies = ["untrusted", "on-request", "never"]

[profiles.claude_default]
display_name = "Claude Default"
provider = "claude"
model = "sonnet"
effort = "high"
permission_mode = "default"

[profiles.codex_default]
display_name = "Codex Default"
provider = "codex"
model = "gpt-5.5"
effort = "high"
sandbox_mode = "workspace-write"
approval_policy = "on-request"

[projects.agency_two]
display_name = "agency-two"
root = "/home/niels/src/personal/agency-two"
default_host = "local"
default_profile = "codex_default"
base_ref = "origin/main"
managed_worktree_root = "/home/niels/src/personal/.agency-worktrees/agency-two"

[projects.agency_two.repositories.primary]
git_common_dir = "/home/niels/src/personal/agency-two/.git"
default_branch_ref = "refs/heads/main"

[safety.policies.strict]
block_dirty_worktree = true
block_untracked_files = true
block_ignored_user_files = true
block_conflicts = true
block_unpushed_commits = true
block_missing_upstream_proof = true
block_missing_marker = true
block_path_escape = true
block_live_session_workspace_close = true
# Structural blockers (missing worktree, missing/mismatched marker, missing tmux
# target, expired heartbeat, wrong worktree, branch checked out elsewhere) are
# always enforced and not toggleable; they gate a RepairRequired summary.
dangerous_claude_permission_modes = ["bypassPermissions"]
dangerous_codex_sandbox_modes = ["danger-full-access"]
dangerous_codex_flags = ["--dangerously-bypass-approvals-and-sandbox", "--yolo"]
# `never` approval is not categorically dangerous; it is gated only in
# combination with danger-full-access or an absent sandbox.
danger_requires_explicit_request = true

[notifications.channels.terminal]
type = "terminal"
events = ["RunNeedsInput", "RunNeedsApproval", "RunStopped", "RunFailed", "CloseBlocked"]
```

### Config Precedence

Config sources merge by `priority` (higher wins) with last-writer-wins per key.
Conflicting typed values across sources are typed config errors, not silent
overrides. Unknown keys are config errors.

### Secret Handling

`agency` sources provider credentials only from the provider's own mechanism on
the host where the runner runs (Claude Code: `ANTHROPIC_API_KEY` or its OAuth
store; Codex: `OPENAI_API_KEY` or its auth file). Credentials are never
transported over SSH; a remote runner uses the devbox's own provider login.
`launch_env_json` is redacted through a secret filter (denylist of
`*_API_KEY`, `*_TOKEN`, `*_SECRET`, `AWS_*`, `ANTHROPIC_*`, `OPENAI_*`, plus a
high-entropy heuristic) that stores only variable names and non-secret values.
Doctor scans stored env snapshots for high-entropy values and reports a defect.

## Public JSON Shapes

Public payloads use `PascalCase` owned enum values, `camelCase` keys, and
handles (never private ids).

### `SessionSummary`

```json
{
  "session": "ses_abc123",
  "title": "Fix reader race",
  "provider": "codex",
  "project": "agency_two",
  "workspace": "wks_abc123",
  "runStatus": "Live",
  "git": {
    "summary": "Clean",
    "presence": "Present",
    "tree": "Clean",
    "untracked": false,
    "ignoredUserFiles": false,
    "conflicts": false,
    "upstream": "Current"
  },
  "model": "gpt-5.5",
  "effort": "high",
  "close": { "closable": false, "summary": "StopRequired", "blockers": ["SessionStillRunning"] },
  "lastEventAt": "2026-07-01T12:00:00Z"
}
```

### `ProjectResult`

```json
{
  "project": "agency_two",
  "displayName": "agency-two",
  "rootPath": "/home/niels/src/personal/agency-two",
  "defaultBaseRef": "origin/main",
  "defaultHost": "local",
  "defaultWorktreeMode": "prompt",
  "managedWorktreeRoot": "/home/niels/src/personal/.agency-worktrees/agency-two",
  "projectRootWorkspace": "project_root"
}
```

### `Model`

```json
{
  "provider": "codex",
  "key": "gpt-5.5",
  "name": "gpt-5.5",
  "efforts": ["minimal", "low", "medium", "high", "xhigh"],
  "permissionModes": [],
  "sandboxModes": ["read-only", "workspace-write", "danger-full-access"],
  "approvalPolicies": ["untrusted", "on-request", "never"],
  "availability": "general"
}
```

### `AgentProfile`

```json
{
  "key": "codex_default",
  "name": "Codex Default",
  "provider": "codex",
  "model": "gpt-5.5",
  "effort": "high",
  "sandboxMode": "workspace-write",
  "approvalPolicy": "on-request"
}
```

### `SessionStatus`

`SessionStatus` extends `SessionSummary` with detail fields:

```json
{
  "workspaceKey": "reader-race",
  "path": "/home/niels/src/personal/.agency-worktrees/agency-two/reader-race",
  "tmux": { "target": "agency-ses_abc123", "attachArgv": ["tmux", "attach-session", "-t", "agency-ses_abc123"] },
  "launch": {
    "provider": "codex",
    "model": "gpt-5.5",
    "effort": "high",
    "workingDirectory": "/home/niels/src/personal/.agency-worktrees/agency-two/reader-race",
    "sandboxMode": "workspace-write",
    "approvalPolicy": "on-request"
  },
  "recentOutput": [],
  "events": [],
  "diff": { "available": true, "base": "abc123", "stat": "" }
}
```

### `WorkspaceSummary`

```json
{
  "workspace": "wks_abc123",
  "workspaceKey": "reader-race",
  "project": "agency_two",
  "path": "/home/niels/src/personal/.agency-worktrees/agency-two/reader-race",
  "managedWorktree": true,
  "branch": "agency/ses_abc123-reader-race",
  "baseRef": "origin/main",
  "baseSha": "abc123",
  "git": {
    "summary": "Dirty",
    "presence": "Present",
    "tree": "Dirty",
    "untracked": false,
    "ignoredUserFiles": false,
    "conflicts": false,
    "upstream": "Current"
  },
  "close": { "closable": false, "summary": "DirtyWorktreeBlocked", "blockers": ["WorktreeDirty"] },
  "sessions": ["ses_abc123"],
  "marker": { "path": "/home/niels/src/personal/.agency-worktrees/agency-two/reader-race/.agency-worktree", "present": true, "matches": true },
  "diff": { "available": true, "base": "abc123", "stat": "" }
}
```

### `CloseAttemptResult`

```json
{
  "closeAttempt": "cls_abc123",
  "target": { "kind": "Workspace", "workspace": "wks_abc123" },
  "closed": false,
  "blockers": [
    {
      "blocker": "WorktreeDirty",
      "summary": "Workspace has tracked changes.",
      "evidence": {
        "path": "/home/niels/src/personal/.agency-worktrees/agency-two/reader-race",
        "files": ["src/app.ts"]
      }
    }
  ]
}
```

## Schema Invariants

Structural invariants (storage-enforced): every table has a primary key; every
outward reference resolves through a typed private id; foreign keys record hard
storage reachability with `foreign_keys=ON`.

Application-enforced invariants (defect on violation, per
[database.md](../../rules/database.md) — not encoded as DB constraints):

- Historical sessions pin exact profile revisions.
- Worktree removal requires a managed worktree row.
- Project-root workspaces cannot have managed worktree rows.
- At most one project-root workspace exists per project.
- Active, closing, and removed worktree rows are mutually exclusive, guaranteed
  by the atomic active→closing and closing→removed transitions.
- Active runner binding and terminal outcome are mutually exclusive, guaranteed
  by the single atomic binding→outcome transition.
- At most one active `session_tmux_targets` row per session and per target
  (also backed by partial unique indexes).
- `run_terminal_outcomes.termination_json` branch matches `outcome`.
- `run_input_events.delivery_json` is `Accepted` xor `Failed` xor `Pending`.
- `safety_check_runs.completed_at` xor `failed_at`; `failure_json` present iff
  `failed_at` present. The same failed-iff-failure rule holds for
  `close_attempts` and `notification_deliveries`.
- Latest status snapshot rows are rebuildable.
- Events are append-only.
- Close blockers are evidence for one close attempt only.
- Notification delivery rows do not influence canonical run state.
- No persisted `launch_env_json` contains a credential value.

## Migration Contract

Version `1` is the first schema. It has no legacy input. The first migration
sets `journal_mode=WAL` and `auto_vacuum=INCREMENTAL`; per-connection pragmas
(`foreign_keys`, `busy_timeout`, `journal_size_limit`) are applied by the
storage service on every connection and asserted. Failed startup on an unknown
schema version is an operator-facing startup error. The product does not attempt
downgrade, fallback, or compatibility reads.
