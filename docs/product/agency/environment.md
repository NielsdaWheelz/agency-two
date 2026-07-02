# Agency Environment Contract

## Status

This document owns environment variables read by the `agency` implementation.
Configuration files remain the product configuration surface; these variables
exist for local installation, tests, and host wiring.

## Variables

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `AGENCY_STATE_DB` | no | `$XDG_STATE_HOME/agency/agency.db` or `~/.local/state/agency/agency.db` | Overrides the SQLite state database path. |
| `AGENCY_RUNNER_BIN` | no | `agency-runner` on `PATH` | Overrides the runner binary used when launching a session. |
| `AGENCY_TMUX_SOCKET` | no | tmux default socket | Uses an explicit tmux socket path, mainly for isolated tests and devbox wiring. |
| `AGENCY_SUPERVISOR_SOCKET` | no | `$XDG_RUNTIME_DIR/agency/supervisor.sock` or temp dir equivalent | Overrides the local supervisor API Unix socket path. |
| `XDG_STATE_HOME` | no | `~/.local/state` | Base directory for state when `AGENCY_STATE_DB` is unset. |
| `XDG_RUNTIME_DIR` | no | system temp directory for runner sockets | Base directory for runner control sockets. |

Provider credentials are not `agency` environment variables. Claude Code and
Codex read their own provider credentials on the host where `agency-runner`
executes.
