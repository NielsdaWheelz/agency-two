# Agency Spec Adversarial Review Record

## Status

This is the audit record for the 2026-07-01 adversarial review of
[spec.md](spec.md), [schema.md](schema.md), and [content-design.md](content-design.md).
It documents what was checked, which findings were accepted and fixed, which were
rejected, what was deferred, and the residual risks. It is not a product
document; it exists for traceability.

The record has three parts: the original spec-document review (below), the
"Implementation Validation Record" that first validated the Go code against the
specs, and the "Spec-by-Spec Validation Pass" (latest) that audited the
implementation clause-by-clause and fixed 16 remaining gaps with regression
tests. The whole tree passes `go build`, `go vet`, `gofmt`, `go test ./...`, and
`go test -race`.

## Method

Eight parallel investigations plus a direct rule read covered: Claude Code CLI
accuracy (web-verified), Codex CLI accuracy (web-verified), compliance with
`docs/rules` operations/concurrency/correctness/errors, compliance with the data
and type rules, a deep audit against `database.md`, cross-document consistency,
architecture and production-readiness, and the subsystem/layer decomposition. All
load-bearing rule quotes were verified against the rule files directly.

## Provider accuracy verdicts (web-verified 2026-07-01)

- Claude `--effort` with `low|medium|high|xhigh|max` is REAL. The original spec
  was correct; kept.
- Claude permission modes `default|acceptEdits|plan|auto|dontAsk|bypassPermissions`
  are REAL. Kept.
- Claude models: `sonnet|opus|haiku|fable` are real aliases; added `opusplan`.
- "background-disabled interactive launch" did not map to a real control. Fixed:
  Claude launches interactively (no `--print`), does not use `--bg`.
- "plugins" is not a single control; fixed to `--plugin-dir`/`--plugin-url`.
  Added `--session-id`/`--resume`/`--continue` for additional-run resume.
- Added the missing Claude command-preview example to content-design.
- Codex models `gpt-5.5|gpt-5.4|gpt-5.4-mini|gpt-5.3-codex-spark` are all REAL
  (including codex-spark, a Pro-only preview). Efforts, sandbox modes, and
  approval policies are all REAL. Kept.
- Fixed the Codex adapter rule that routed sandbox/approval through `-c`: only
  reasoning effort uses `-c model_reasoning_effort=`; sandbox and approval have
  dedicated `--sandbox` and `--ask-for-approval` flags.
- Fixed the effort error hint to include `minimal`.
- Split the conflated `dangerous_permission_modes` list (which mixed a Claude
  permission mode, a Codex sandbox mode, a Codex approval policy, and a flag
  alias) into namespaced lists, removed bare `never`, and matched `--yolo` /
  `--dangerously-bypass-approvals-and-sandbox` as flags.

## Accepted findings, grouped by fix

### Correctness and concurrency (critical)

- Send input was miscategorized as a replayable single mutation (a PTY write +
  storage writes), which would double-send keystrokes on replay. Reclassified as
  a durable operation modeled as uncertain-transition + stabilization, with the
  PTY write memoized on `(run_id, input_seq)`.
- Linearization keys did not compose: close-session keyed on session vs
  send-input keyed on run, and worktree-close keyed on workspace vs start-run
  keyed on session, so the "serialize" rules could not hold and a worktree close
  could race a run start. Fixed: start-run also acquires the workspace key;
  close-session also acquires its run keys. The operation table now names both
  conflict key and strategy, and separates replay key from conflict key.
- The supervisor singleton was asserted but unenforced, so two supervisors could
  corrupt state and bypass all in-process serialization. Added `flock` on a
  lockfile + socket liveness check + `SupervisorAlreadyRunning`.
- Replay keys were "created" per invocation instead of derived from the payload,
  so naive retries were not idempotent, and durable ops had no persisted replay
  identity. Added payload-derived namespaced replay keys and an
  `idempotency_keys` table.

### Data-loss, security, and secrets (critical)

- `launch_env_json` persisted the environment snapshot, which could store
  provider API keys in plaintext SQLite — contradicting the schema's own secrets
  rule. Fixed: agency stores no credentials; env snapshots are redacted; DB files
  are `0600`; a Doctor scan flags high-entropy values; remote runners use the
  devbox's own provider login.
- The runner wire protocol was under hard-cutover with no adoption path, so a
  supervisor upgrade would meet old runners and (per the rules) treat them as
  defects — killing live agent work. Added a versioned, forward-compatible runner
  protocol with a stable preamble, adopt/quarantine on handshake, and an explicit
  "never kill on version mismatch" rule.
- The private TCP tunnel had no authentication, an RCE-equivalent surface. Added
  peer-credential checks on the Unix socket and a required loopback bearer token
  for any TCP tunnel.

### Illegal states and cross-document consistency (critical/major)

- Three terminal outcomes (`RunnerFailed`, `StartFailed`, `Orphaned`) had no run
  status. Added `Failed`, plus `RepairRequired`, to the enumerated status set and
  the dashboard grouping.
- `agent_sessions.objective` was `NOT NULL` with no CLI/TUI/content source.
  Removed it; title plus the initial prompt suffice.
- Close eligibility and git status were flat single-value enums, but a workspace
  can be dirty and untracked and unpushed at once. Remodeled close eligibility as
  `{ closable, blockers[] }` with a derived summary, and git status as orthogonal
  axes with a derived `GitSummary`. This also closed every dangling blocker
  (missing upstream proof, missing worktree, tmux/heartbeat) because each blocker
  is now a set member rather than needing a one-to-one eligibility value.
- Added the missing `WorktreeHasConflicts`, `NotExpectedWorktree`, and
  `BranchCheckedOutElsewhere` blockers so every close-safety condition has a
  recordable key.
- Unified enum encoding: agency-owned enums are `PascalCase` everywhere (code,
  columns, JSON egress); config TOML stays lowercase ingress vocabulary; provider
  vocabularies are preserved verbatim. Renamed the enum columns that misused the
  `*_key` suffix. Added the canonical enum tables to the schema and a
  Title→enum→JSON mapping to content-design.
- Reconciled event vocabulary to one canonical `EventType` set; added the missing
  needs-input/needs-approval/failed events; fixed the checklist rule so it no
  longer forbids the legitimate `RunFailed` wording.
- Added handle prefixes, made `RunHandle` session-scoped, standardized workspace
  display (handle plus workspace key), enumerated `WorktreeMode`, added
  `agency profile create`/`set` and fixed the empty-state guidance, and used a
  distinct attach-only host name (`mosh-box`) so its mode matches its description.

### Storage and rules compliance (major)

- Mandated `PRAGMA foreign_keys=ON` per connection (SQLite defaults it off, which
  made every `references` inert), declared WAL/`busy_timeout`/`journal_size_limit`
  provisioning, and documented the single-writer serializable model.
- Fixed id and timestamp affinity: bare `uuid`/`timestamp` yield NUMERIC affinity
  in SQLite; declared ids as `text` UUIDv7 and instants as ISO-8601 UTC text.
- Documented the two atomic state-swap transactions (active→removed worktree,
  binding→terminal-outcome) and the sequence-allocation contract.
- Moved structured JSON to SQLite-native JSON with a bind adapter and gave every
  `*_json` column a named typed shape (the events `payload_json` bag was the
  clearest violation of the schema's own "no generic metadata columns" rule).
- Added the missing indexes for stated query paths (session_tmux_targets,
  close_blockers, safety_check_findings, events by subject/type/causation,
  config_revisions) and the partial unique indexes for one active tmux
  association per session and per target; de-duplicated the index list.
- Enumerated the previously open enum domains (stream, source, role, check kind,
  event type) and added the conditional-nullability and one-root-per-project
  invariants to the app-code invariant list.
- Modeled the co-null outcome/delivery columns as tagged unions
  (`termination_json`, `delivery_json`).

### Architecture, timing, and production-readiness (major)

- Clarified ownership overlaps: worktree close is safety (predicate) + status
  (projection) + workspace (mutation) + close workflow (thin orchestrator);
  folded the redundant input service into the run service; made Doctor observe
  only, with repairs as named ops on owning services; reclassified the 26 "systems"
  by kind and added `internal/agency/supervisor`, `internal/agency/runner`, and
  `internal/agency/runnerproto` homes.
- Added named timing constants (`[timing]`), a categorical retry-policy reference,
  a `justify-polling`-style event-driven-preferred status note, monotonic-clock
  heartbeat liveness with suspend/resume handling, startup reconciliation,
  reboot/tmux-server-restart recovery, a retention/compaction policy, and an
  observability contract (structured logs + health method).
- Added the shell/argv escaping contract: argv is a validated token array,
  dynamic tokens are `ShellArgumentText` quoted through the shell helper, and
  SSH-forwarded commands are never concatenated.
- Scoped `*Unavailable` errors to binary-absent prechecks (`ProviderCliMissing`
  etc.) and defined `RepairRequired` as the durable dead-letter / invariant-break
  boundary, distinct from the intentionally-modeled `Lost*` external
  disappearances.
- Resolved the transcript-parsing tension: prompt-state detection reads only the
  rendered TTY, is a best-effort signal that never gates a destructive decision,
  and is fingerprinted so breakage is visible.

### Scope (major)

- Added a read-only diff surface (`agency diff` / `worktree diff`) against the
  pinned `base_sha` — the biggest capability gap vs prior art, and one that needs
  no merge/push.
- Resolved the multi-run inconsistency (schema modeled `run_seq` but the CLI had
  no way to start a second run) by adding `agency run <session>`.
- Added session rename and bulk-close of terminal sessions.

## Rejected or intentionally not applied

- Dropping the denormalized `agent_sessions.project_id`/`host_id` columns. A
  session has a direct, authorized relationship to a project and a host (not a
  mere derivation through workspace), and the dashboard-grouping indexes need
  them. Kept, and documented as direct relationships rather than discriminators.
- Making the `--json` wire lowercase/camelCase per external REST convention.
  `naming.md` states unconditionally that enums are `PascalCase`, the output is
  agency's own owned API (not a third-party protocol it must conform to), and the
  original three encodings were internally inconsistent regardless. Chose uniform
  `PascalCase` for owned enums.
- Adding branch deletion to the target state. The spec deliberately scopes it out
  "in the first target state." Respected the non-goal; instead surfaced orphaned
  branches in Doctor and recorded guarded merged-and-pushed deletion as an
  explicit Deferred Decision with its mechanism.

## Deferred (recorded in spec Deferred Decisions)

- Guarded deletion of provably merged-and-pushed agency branches.
- Webhook notification channels beyond terminal and desktop.
- Bulk operations beyond closing terminal sessions.

## Residual risks and open questions for the owner

- Prompt-state detection over rendered TTY is inherently fragile across provider
  TUI redesigns. The fingerprint-and-defect approach makes breakage visible but
  does not prevent it; a provider-published machine-readable status signal would
  be the durable fix if one ever ships.
- `gpt-5.3-codex-spark` is a Pro-only, text-only preview; agency validation
  passes it but Codex may reject it at runtime for non-Pro accounts. The
  `ModelSpec.availability` field is the place to warn, but the warning copy is not
  yet specified.
- The retention defaults (`output_retention_days`, `event_retention_days`) are
  placeholders; the owner should confirm them against real devbox disk budgets.
- N-1 runner protocol adoption bounds how far an upgrade can skip; a documented
  support window (how many prior protocol versions are adopted) should be pinned
  before the first breaking protocol change.

# Implementation Validation Record (2026-07-01)

A second adversarial pass validated the Go **implementation** against the three
specs and `docs/rules`. Ten parallel reviewers audited storage, enum encoding,
provider adapters, runner/protocol, supervisor lifecycle, transport
security/observability, safety/close/git, the operation model, CLI/TUI/config,
and status/events/notifications/remote/doctor. Findings were triaged and the
correctness-critical set was fixed and verified (`go test ./...` and
`go test -race` on the concurrency-heavy packages all green).

## Fixed and verified

Runner / wire protocol (`runner`, `runnerproto`):
- Data race on server-state init (`clients`/`nextChunkSeq`/`inputSeqs` were set
  inside `run()` after `captureOutput` started) that could also emit
  `chunk_seq=0` and reset the counter, violating `(run_id, chunk_seq)`
  monotonicity+idempotency. State is now initialized in `Serve` before any
  goroutine starts (race-tested).
- Wire protocol is now genuinely forward-compatible: `ReadMessage` returns an
  `Unknown` value (not an error) for unrecognized message types, so a peer that
  outlives an upgrade skips them instead of dropping the connection.
- Terminal `Exit` could be dropped when the child-wait goroutine's `cancel()`
  raced the `done` branch; removed the cancel so `run()` observes the exit
  deterministically, and Exit is now emitted only after the PTY output is fully
  drained (no trailing-output loss).
- Heartbeats now go to all connected clients, not only output subscribers.
- `validateEnv` no longer includes the variable value in error text (secret
  leak); env errors name only the variable.
- Server shutdown closes accepted client connections; removed dead `broadcast`.

Storage (`storage`):
- Per-connection pragmas (`foreign_keys`, `busy_timeout`, `journal_size_limit`)
  are now applied via the DSN so every pooled/replacement connection gets them,
  not just the first; `foreign_keys=1` is asserted per startup. `journal_mode=WAL`
  and `auto_vacuum=INCREMENTAL` are set at first migration on the empty DB.
- `TmuxServerRestarted` events attach to a canonical `Host` StatusSubject
  (was a non-canonical `TmuxServer` kind); the unreachable `publicEventSubject`
  fallback now defects instead of emitting a non-canonical `Subject` kind.

Time and liveness (`supervisor`, `storage`):
- Heartbeat liveness is now evaluated on the supervisor's **monotonic** clock, as
  the spec requires. The store no longer derives `LostRunner`/`Orphaned` from a
  persisted wall-clock delta; the supervisor tracks a per-run monotonic baseline
  set at bind/adoption/heartbeat, treats every binding as unproven-but-not-lost
  for one fresh TTL window after (re)start, and (via `CLOCK_MONOTONIC`) does not
  expire live runners across suspend/resume. `heartbeat_ttl_ms` and
  `reconcile_interval_ms` are now config-injectable with schema-matching
  defaults; the previously divergent status/reconcile TTLs are unified. The
  worktree-close safety check derives `RunnerHeartbeatExpired` from the same
  monotonic predicate, and that structural blocker is no longer gated behind the
  optional tmux predicate.
- Runner adoption accepts current or supported prior protocol versions
  (`SupportedProtocolVersion`) rather than exact-match.

Operation model / CLI (`app`, `supervisor`):
- `agency send` no longer derives its replay key from the input bytes: identical
  keystrokes were collapsed to one replay key and silently swallowed. Each send
  is a distinct action (fresh nonce); `--replay-key` remains for opt-in
  idempotency.
- Exit codes: safety-blocked launch now returns 3 (was 1/2) with the typed
  reason and next action; repair-required attach returns 5. Added shared
  error-classification helpers.
- Provider control-rejection errors collapsed to the canonical `UnsupportedControl`
  code (was `UnsupportedPermissionMode`/`UnsupportedSandboxMode`/…).
- **Conflict-key linearization** (spec Concurrency Rules): added a deadlock-free
  keyed in-process lock registry (sorted multi-key acquire) applied at the
  dispatch layer. Session-scoped mutations (send/stop/kill/close-session/rename)
  serialize on the session key — so concurrent sends allocate their input
  sequence and write to the PTY in order — worktree close serializes on the
  workspace key, and start-run acquires both session and workspace keys so it
  serializes with a concurrent worktree close. Covered by a race-tested unit
  test; cross-process safety still rests on the supervisor singleton.

Observability / reconciliation (`supervisor`):
- `health` now reports WAL size and clock baseline; added a peer-cred-guarded
  goroutine-dump endpoint.
- `WorktreeReconciled` events are emitted on reconcile publish/discard
  transitions (was never emitted). Doctor now also observes git and tmux CLI
  availability and project-root existence.
- Doctor issue code casing fixed to the spec name (`ProviderCliMissing`).

## Second implementation pass (2026-07-01) — subsystems now built

A follow-up pass implemented the previously-missing subsystems. Each is
verified (`go test ./...` and `go test -race` on the concurrency-heavy packages
green):

- **Config service (TOML)** — new `internal/agency/config` package: strict TOML
  parsing (unknown keys, unknown ingress enums, and same-priority conflicts are
  typed errors), ingress→owned-enum conversion, precedence merge over built-in
  defaults, validation, and typed `Config`. The supervisor loads it at startup;
  runtime timing (heartbeat TTL, reconcile interval) comes from it, and the
  effective-config revision now reflects the parsed values.
- **Operational logging** — new `internal/agency/logging` package: leveled JSON
  logs to `logs/supervisor.log` with size-based rotation and bounded retention.
  The supervisor logs startup, reconcile summaries, reconcile-loop errors (no
  longer swallowed), compaction, and notification exhaustion.
- **Retention/compaction** — `Prune` now deletes past-retention rows
  (idempotency keys, notification deliveries, close attempts, safety checks,
  closed-session output chunks, non-latest status snapshots) child-before-parent
  in one transaction, then checkpoints + incremental-vacuums; a background
  compaction loop runs it on a cadence; `agency prune` reports counts.
- **Notification worker** — delivery decoupled from the event transaction; a
  worker delivers selected events to terminal (spool file) and desktop
  (`notify-send`) channels with bounded retry and per-attempt recording; dedup by
  `(channel, event_id)`.
- **Reconciliation steps d/e + `DoctorIssueObserved`** — step (e) sweeps zombie
  runs (tmux target, no binding, no outcome, past a re-observation grace) to
  `StartFailed`; step (d) scans for adoptable-orphan worktree markers and reports
  `DoctorIssueObserved` (never deletes user work); `WorktreeReconciled` is
  emitted on publish/discard transitions.
- **Prompt-state detection** — reads the rendered `tmux capture-pane` snapshot
  (not raw PTY bytes) via a per-run detection loop; `provider.DetectPromptState`
  classifies per provider, scans only the bottom region, and degrades to Live/
  Quiet on uncertainty (never fabricates). `FingerprintKnown` exposed for Doctor.
- **Idempotency recovery** — a crash-stranded `InFlight` key older than the
  recovery window is reclaimed on the next replay (safe now that same-key ops are
  serialized by the conflict-key locks), so replay is never dead-ended.
- **Transport hardening (partial)** — peer-credential verification is now
  build-tagged (Linux `SO_PEERCRED`, Darwin `LOCAL_PEERCRED`, others fail closed
  — no more non-Linux fail-open); the API socket path uses a private per-user
  runtime dir instead of the world-shared system temp dir.
- **Provider capability contract** — added `command_candidates` (wired into
  Doctor's availability probe), `extra_directory`, `initial_prompt`, and
  `session_resume` declarations; canonical `UnsupportedControl` error code.
- **TUI launcher** — `RenderLauncher` view with the exact command preview and
  blocking `Cannot launch.` validation strings.
- **Event correlation** — every event now carries a non-empty correlation id
  (the entity's run/session/workspace/host id) so a request can be traced across
  event-log and operational-log lines.
- **Remote / devbox** — new `internal/agency/remote` package: `agency host
  install-unit [--launchd]` generates a systemd user unit / launchd agent to
  auto-start the supervisor on boot; `agency host check <host>` performs an SSH
  bootstrap check via a validated token-array argv (no string concatenation);
  forwarded-socket and discovery argv builders are provided. Mosh attach-only is
  enforced end to end.

## Third implementation pass (2026-07-01) — remaining items built

The follow-on gaps are now implemented and verified:

- **Authenticated loopback TCP tunnel** — the supervisor optionally serves the
  API over a loopback TCP listener (`Config.TCPTunnelAddr`); it mints a
  per-supervisor bearer token, writes it and the chosen address to `0600` files
  in the runtime dir, enforces loopback binding, and verifies the token
  (constant-time) before any dispatch on TCP connections. `CallTCP` is the token
  client. Unix connections still authenticate by peer credential. Tested
  (wrong-token rejected, valid-token works, token file `0600`).
- **Remote discovery/start** — `agency host check|discover|start <host>` resolve
  the host's SSH alias and run validated token-array argv
  (`remote.BootstrapArgv`/`DiscoverArgv`/`StartArgv`); `agency host install-unit
  [--launchd]` generates the auto-start unit. Mosh hosts are attach-only.
- **Provider per-flag + `ShellArgumentText`** — every dynamic argv token is now a
  validated `ShellArgumentText` built via an `argvBuilder` (no string
  concatenation of unvalidated values); the adapters emit Claude
  `--settings`/`--mcp-config`/`--session-id`/`--resume`/`--continue` and Codex
  `--profile`/`--search` from the launch input, and F5 resume is adapter-owned
  via `provider.ContinueArgv` (storage no longer hardcodes `--continue`).
- **Event correlation + causation** — every event carries a non-empty
  correlation id; user-initiated terminal outcomes (`RunStopped`/`RunKilled`)
  link their `causation_event_id` to the preceding `StopRequested`.
- **`agency run` flags** — `run` accepts the full new/run flag set; control- and
  workspace-changing flags return a clear `UnsupportedControl` error (additional
  runs inherit the session's pinned workspace and profile) rather than an
  unknown-flag rejection; `--json` output is supported.

## Still remaining (minor)

- The `{input, executionPolicy}` provider parameter shape is not split out
  (`AllowDangerous` remains a field on `LaunchInput`); dangerous-launch gating
  still lives in the adapter rather than a dedicated safety-service method.
- Profile creation does not yet expose the Codex `--search`/`--profile` or Claude
  `--settings`/`--mcp-config` controls for persistence into a profile revision
  (the adapter emits them when set; the profile-config surface to set them is
  minimal).
- Per-event actor attribution is coarse (Supervisor/User by subject class) rather
  than threaded per operation.

# Spec-by-Spec Validation Pass (2026-07-01, third)

A further adversarial pass validated the implementation clause-by-clause against
`spec.md`, `schema.md`, and `content-design.md` (and `docs/rules`). Every fix
below is strict (no lab-only shortcut) and lands with a regression test unless
noted; the whole tree passes `go build`, `go vet`, `go test ./...`, and
`go test -race ./...`, and `gofmt` is clean.

## Fixed and verified (with tests)

- **Timing config-ownership (`spec.md` Time And Clocks).** All five timing
  params are now config-owned, no inline literals. `quiet_threshold_ms` flows
  into `storage.Store.quietThreshold` (via `SetQuietThreshold`) and
  `deriveRunStatus` takes it as a parameter; `graceful_stop_timeout_ms` is a
  `Server.gracefulStopTimeout` threaded through `requestRunnerStop/Kill/Terminal`
  (was a hardcoded 5s that disagreed with the 10s config default);
  `heartbeat_interval_ms` is passed to the runner via a new
  `-heartbeat-interval-ms` argv flag (was a runner package const).
- **Config security merge downgrade (security).** `mergeSecurity` clobbered the
  default-true toggles (`socket_peer_credential_check`,
  `tcp_tunnel_requires_token`, `tcp_tunnel_loopback_only`) to false whenever a
  higher-priority source omitted `[security]`. Introduced TOML-metadata
  `declaredFlags` (`md.IsDefined`) so a bool that defaults true is overlaid only
  when the source actually declared it; `= false` is honored, absence is not.
  Same mechanism fixed `suspend_resume_reset` (previously dropped from the timing
  merge) and `ui.show_closed` (previously an un-resettable OR-merge).
- **Prompt-hint stickiness (`spec.md` Prompt-State Detection).** A `NeedsInput`/
  `NeedsApproval` snapshot pinned the run status forever when the prompt was
  resolved outside agency (direct pane interaction, timeout, auto-approve).
  `DetectPromptState` is now three-valued (`NeedsInput`/`NeedsApproval` /
  `PromptStatusNone` / `""` uncertain); the supervisor calls a new
  `ClearPromptHint` on a readable-but-not-prompting pane, which un-pins only a
  prompt snapshot (never a RepairRequired one) and reverts to derived Live/Quiet.
- **Retention: `status_snapshot_window` (`schema.md`/`spec.md`).** Prune treated
  the per-subject snapshot *count* as a time cut. It is now a `row_number()`
  window keeping the newest N per subject, always retaining the latest pointer.
- **Retention: events pruning (`spec.md` Maintenance Capabilities).** Events were
  never pruned. They now age out per `event_retention_days`, scoped to closed
  sessions and terminal runs, excluding any event still referenced by a
  notification delivery or as another event's causation parent (so neither
  foreign key can abort the sweep; a held-back parent ages out a round later).
  `PruneResult.Events` and the CLI `events:` line were added.
- **Prune FK abort (correctness).** A `close_attempt` still backing a
  `closing_worktrees` teardown could age past retention and abort the whole
  compaction on its FK child; the `close_attempts`/`close_blockers` deletes now
  exclude attempts referenced by `closing_worktrees`.
- **Notification delivery invariant (`schema.md`).** `RecordNotificationFailed`
  wrote `failure_json` on every attempt while setting `failed_at` only on the
  terminal one, violating "`failure_json` present iff `failed_at` present". Both
  are now written together, only on exhaustion.
- **Notification content (`content-design.md`).** The worker emitted a generic
  title/label. `notificationContent` now renders the exact templates
  (needs-input/needs-approval/failed/close-blocked), naming the session title,
  provider, workspace, failure reason, and leading close blocker — passing the
  content-design copy checklist (one next action for failures; a close blocker is
  never masked as "failed"). The `PendingNotifications` query was enriched with
  title/workspace/provider/payload.
- **Desktop channel deliverability.** Configured notification channels were never
  synced to `notification_channels`, so a `Desktop` channel was inert. Added
  `SyncNotificationChannels` (config-authoritative upsert-by-key + soft-disable of
  dropped channels), called at supervisor open.
- **Config path expansion.** `[paths]` values are now `~`-, `$VAR`-expanded and
  absolutized at config ingress (`expandPaths`), so downstream services never see
  a relative or tilde path.

## Fixed and verified (no dedicated test; covered by build/existing suites)

- **Reconciliation step 5 rediscover (`spec.md` Startup Reconciliation).** A
  zombie run (tmux target, no binding) whose runner was actually alive was
  skipped forever. `reconcileZombieRuns` now rediscovers a live, compatible
  runner and rebinds+adopts it in place (via the new `storage.ZombieRuns`
  returning the tmux target id); it marks `StartFailed` only when no matching
  compatible runner answers.
- **Reconciliation step 3 quarantine (`spec.md`).** A managed worktree whose
  marker hash mismatched was silently left unpublished; it is now quarantined
  (`WorktreeReconciled{action:"quarantined-inconsistent"}`, left unpublished for
  repair) rather than skipped.
- **API socket directory hardening (security).** The supervisor did not assert
  perms on a pre-existing socket directory (`MkdirAll` leaves them untouched), so
  an attacker-created wide parent could be used and the socket MITM'd.
  `assertPrivateSocketDir` now tightens-then-verifies 0700 (chmod fails on a
  directory we do not own). `privateRuntimeDir` no longer falls back to the
  world-shared temp dir (prefers `~/.agency/run`, else a uid-scoped temp subdir).
- **Doctor prompt-fingerprint check.** `provider.FingerprintKnown` is now wired
  into `doctor`: a live pane matching no known provider TUI fingerprint raises a
  `PromptFingerprintUnknown` warning, so heuristic drift is observable rather
  than a silently stuck status.

## Documentation reconciled to the implementation

- `schema.md`: added the `Theme` owned enum (`System`/`Light`/`Dark`) and the
  `Correlation` JSON shape (`{ "correlationId": text }`) plus a causation-chain
  note on `events`.
- `spec.md` Commands: added the implemented, spec-feature-backing commands
  `events`, `kill`, and `host list|check|discover|start|install-unit`.
- `config`: `DiscoverSources` now uses `OverridePath` (removing a duplicated path
  literal) and `OverridePath`'s stale comment was corrected (runtime
  `config set` persists to the database, not this file).

## Assessed and intentionally not changed (with reasoning)

- **`codexDangerous` "dead branch".** `never` approval combined with
  `workspace-write` is intentionally NOT dangerous per `schema.md` (only with
  `danger-full-access` or an absent sandbox). Agency always sets a sandbox, so the
  absent-sandbox arm is unreachable in practice; the gate is spec-correct.
- **Runner command via `provider.ShellPreview`.** `tmux new-session -- <string>`
  runs the command through `sh -c`, so shell quoting is required; `shellQuote`
  does robust POSIX single-quoting. A token-array form would risk tmux
  version-specific behavior, so the current portable, shell-safe path is kept.
- **Event actor kinds.** `User`/`Supervisor` are the closed-shape actor kinds the
  schema calls for; finer per-operation attribution remains a documented minor.
- **TUI launcher scaffolding.** `tui.RenderLauncher`/`LauncherView` and the
  `content.LaunchBlockedBaseRef`/`LaunchBlockedTmux` messages are implemented and
  unit-tested but not yet wired into an interactive TUI loop. They are spec
  launcher features pending wiring, deliberately retained (not deleted) to avoid
  reducing spec coverage.

## Still remaining (minor, ranked)

1. `ParseOutput(chunk) -> ProviderEvent[]` from the `spec.md` Provider Contract
   is not implemented: there is no consumer of `ProviderEvent[]` in the current
   architecture (prompt hints are produced by `DetectPromptState` over captured
   panes), so adding it now would be speculative dead code against
   `docs/rules/simplicity`. To be implemented alongside its first consumer.
2. Wire the TUI launcher (`RenderLauncher`) and its `LaunchBlocked*` copy into an
   interactive loop.
3. Split the provider `{input, executionPolicy}` parameter and move
   dangerous-launch gating into a dedicated safety-service method.
4. Persist Codex `--search`/`--profile` and Claude `--settings`/`--mcp-config`
   into profile revisions from the profile-config surface.
5. Thread per-operation actor attribution rather than by subject class.

# End-to-End CLI Validation (2026-07-01)

The CLI was exercised end-to-end against real `tmux`, `git`, `claude`, and
`codex`: build the three binaries, start a supervisor with an isolated state DB
and socket, `project init` a real git repo, launch real codex and claude agents
(root workspace and a managed worktree), observe status, stream events, send
input, stop/kill, and attempt worktree close. This surfaced six real bugs the
unit suites missed; each was fixed at the source-of-truth and locked with a
regression test.

1. **Displayed tmux target unusable (supervisor).** `agency new` printed
   `tmux: agency:ses_xxxxx` (a `session:window` target naming a nonexistent
   `agency` session) while the real session is `agency-ses_xxxxx`. `new` now
   renders `runnerTmuxSession(created)` — the same source-of-truth function that
   creates the session (`attach`/`status` already used the correct name). Guard:
   `TestNewAndListHumanAndJSON` asserts `tmux: agency-ses_`.
2. **Empty `WORKSPACE` column in `agency list` (storage/CLI).** `SessionSummary.
   WorkspaceKey` was `json:"-"`, so the supervisor used it internally but never
   sent it to the CLI, which rendered an empty column. It is now serialized
   (matching `WorktreeSummary`, and permitted by the public-payload privacy
   policy, which forbids only internal UUIDs); `list` shows `project root`/the
   worktree name. `path` stays private.
3. **`RunnerAdopted` event spam + prompt-snapshot clobbering (supervisor/storage).**
   Reconcile called `MarkRunnerAdopted` for every live binding every
   `reconcile_interval_ms`, emitting a `RunnerAdopted` event *and deleting the run
   status snapshot* each pass — unbounded event growth and a 15s flicker fight
   with the 1s prompt detector. Adoption is now a transition: an already-tracked
   runner (live output subscription) is only heartbeat-refreshed
   (`RefreshRunnerHeartbeat`, no event, no snapshot delete); `RunnerAdopted` fires
   once, only when re-establishing a binding with no live subscription (fresh
   start or post-restart survivor). Verified e2e: a stable runner logged zero
   adoptions; a supervisor restart re-adopted the survivor with exactly one.
4. **Graceful `stop` mislabeled `Exited` (runner).** The runner mapped the
   outcome from the child's exit *signal* only, so a provider that catches SIGTERM
   and exits 0 (claude/codex) was reported `ProviderExited`, losing that the user
   stopped it. The runner now records termination *intent* (stop/kill requested)
   and attributes a clean or non-zero post-request exit to `UserStopped`/
   `UserKilled`. Verified e2e: `stop` now yields `run: Stopped`.
5. **`kill` on an already-terminal run leaked `TerminalOutcomeConflict`
   (storage).** Killing a run that had already exited tried to record `UserKilled`
   over the existing outcome and surfaced an internal invariant error. `KillRun`
   is now an idempotent no-op when a terminal outcome exists (like `kill(2)` on a
   finished process), preserving the recorded outcome.
6. **State/socket directory hardening confirmed working.** The supervisor
   correctly refused to start against a `0755` state directory (the earlier
   private-dir hardening), validating that path end-to-end.

Everything else exercised behaved correctly: worktree creation on
`agency/<name>` with an ownership marker; Live→Quiet transition on the
`quiet_threshold`; `events` audit log; `send` input delivery; `rename`; `diff`;
and — notably — `worktree close` was correctly **blocked** ("no remote tracking
proof — No files were removed", exit 3), exactly the data-loss guard and copy the
content-design specifies.

## High-risk path validation (2026-07-01, second e2e session)

The four highest-risk untested paths were then driven end-to-end against real
tmux/git/claude/codex. Two passed clean; prompt detection surfaced five real
bugs (all fixed with tests).

**1. Worktree close *success* path — PASSED.** With a bare remote configured and
the `agency/<name>` branch pushed (remote-tracking proof), `worktree close`
transitioned close→closing→removed: the git worktree and its directory were
removed (only `main` remained), the branch was *kept* ("branch … remains" — no
branch deletion, data-safe), and `worktree list` went empty.

**2. Prompt-state detection — five bugs found and fixed.**
- **Runner PTY had no window size** (`openPTY` never set `TIOCSWINSZ`), so the
  child terminal was 0x0 and claude/codex rendered garbled (wrapping at ~45
  cols), which also split prompt phrases across lines. The runner now sizes the
  child PTY to its own controlling terminal (the tmux pane), else 80x24. Verified:
  rendering went from garbled to clean.
- **`agency send` submitted `\n`, not `\r`.** A provider TUI in raw mode reads
  carriage return as Enter; a bare line feed left the typed task sitting
  unsubmitted in claude's input box. Confirmed by manually sending `\r` (the task
  then submitted). Both the interactive `send` path and the initial-prompt path
  now send `\r`. This was breaking the core "send a task to the agent" workflow.
- **Detection phrases did not match claude's real UI.** Claude renders every
  blocking decision (trust gate, onboarding menus, tool approvals) with an
  "Enter to confirm · Esc to cancel" footer; none of the heuristics matched, so a
  blocked agent showed `Live`/`Quiet`. Added the observed stable signals; locked
  with golden tests built from real captured panes (idle input box stays
  non-prompt).
- **The runner did not forward its stdin** (the tmux pane) to the child, so an
  attached user's keystrokes never reached the provider — attach was effectively
  read-only, contradicting the spec's "attach opens a session for direct
  interaction." The runner now forwards stdin to the child PTY (socket `send`
  remains the audited path); covered by a unit test.
- Environment note (not an agency bug): the claude/codex launched by the runner
  are not authenticated in this sandbox (claude returned a 401), so the *agent's
  own work* could not be exercised — only agency's lifecycle around it.

**3. `LostTmuxTarget` + repair — PASSED.** Killing a live run's tmux target was
detected as `LostTmuxTarget` (status and `doctor`, which surfaced
`repair: lost-tmux-target:<session>`); `agency repair` marked the unrecoverable
run `Failed`/`Closable` and recorded RunFailed + RepairCompleted. `doctor` also
correctly reported the `OrphanedAgencyBranch` left by test 1's close.

**4. Notification delivery — PASSED.** Real events (RunStopped, RunFailed,
RunNeedsInput) were delivered to the terminal spool
(`logs/notifications.log`), one line per event, formatted per content-design
("Agency: ses_… stopped / Codex was stopped in closeme.", "… failed / Runner
failed before Codex exited. Run agency status …"). Codex prompt detection was
observed firing for real (a delivered RunNeedsInput). One minor content polish
applied: notification bodies now use the friendly workspace label
(`content.WorkspaceLabel`, so `project_root` renders as "project root").
