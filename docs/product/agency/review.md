# Agency Spec Adversarial Review Record

## Status

This is the audit record for the 2026-07-01 adversarial review of
[spec.md](spec.md), [schema.md](schema.md), and [content-design.md](content-design.md).
It documents what was checked, which findings were accepted and fixed, which were
rejected, what was deferred, and the residual risks. It is not a product
document; it exists for traceability.

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
