# Porting nanobot to Go

nanobot-go is a Go rewrite of [HKUDS/nanobot](https://github.com/HKUDS/nanobot).
This document maps the Python packages to Go packages and records the order
in which we port them.

## Package mapping

| nanobot (Python)                         | nanobot-go (Go)                  | Notes |
|------------------------------------------|----------------------------------|-------|
| `nanobot/bus` (`events.py`, `queue.py`)  | `internal/bus`                   | Inbound/outbound messages, `MessageBus` on channels |
| `nanobot/agent/loop.py`                  | `internal/agent` (`Loop`)        | Channel-facing turn: session key, context, delivery |
| `nanobot/agent/runner.py`, `hook.py`     | `internal/agent` (`Runner`, `Hook`) | Model-facing loop: provider calls, tools, iteration caps |
| `nanobot/agent/context.py`               | `internal/agent/prompt`          | System prompt assembly |
| `nanobot/agent/tools`                    | `internal/tools`, `internal/tools/builtin` | `Tool` interface, `Registry`, built-in tools |
| `nanobot/agent/tools/mcp.py`, `mcp_oauth.py` | `internal/mcp`               | MCP client, tools wrapped as `mcp_<server>_<tool>` |
| `nanobot/agent/memory.py` (Consolidator, Dream) | `internal/memory`         | history.jsonl archive, long-term files, Dream job |
| `nanobot/agent/subagent.py`              | `internal/agent/subagent`        | Reuses `Runner` with a restricted registry |
| `nanobot/providers`                      | `internal/providers`             | `Provider` interface, OpenAI-compatible + Anthropic over `net/http`, retry, `Fallback` |
| `nanobot/session`                        | `internal/session`               | `Store` interface; JSONL first, SQLite later |
| `nanobot/config`                         | `internal/config`                | JSON config, defaults, validation |
| `nanobot/cli`                            | `cmd/nanobot`, `internal/cli`    | `agent`, `gateway`, `onboard` subcommands |
| `nanobot/cron`, `nanobot/triggers`       | `internal/cron`                  | `at`/`every`/`cron` schedules, session-bound jobs |
| `nanobot/channels`                       | `internal/channels`, `internal/channels/<name>` | `Channel` interface, manager, one package per channel |
| `nanobot/security`, `agent/tools/sandbox.py` | `internal/security`          | Workspace containment, shell guard, SSRF, sandbox backends |
| `nanobot/gateway`, `nanobot/api`         | `internal/gateway`, `internal/api` | Daemon wiring, OpenAI-compatible HTTP API |
| `webui/`                                 | `web/` (embedded via `embed`)    | Served by the gateway |

## Phased plan

### Phase 1: core

- `internal/bus`: messages and `MessageBus`.
- `internal/tools`: `Tool`, `Registry`, built-ins (`read_file`, `write_file`,
  `edit_file`, `list_dir`, `exec`, `web_fetch`).
- `internal/security`: `Workspace` path resolution, `ShellGuard`, basic SSRF check.
- `internal/providers`: `Provider` interface, OpenAI-compatible and Anthropic
  backends, retry with backoff, `Fallback` with a circuit breaker.
- `internal/session`: JSONL session store with atomic writes.
- `internal/agent`: `Runner` (model/tool loop, concurrent read-only tools,
  iteration limit with a final no-tools request) and `Loop` (one worker per
  session key, ordered within a session).
- `internal/config`: JSON config file.
- `cmd/nanobot`: interactive and one-shot CLI going through the bus.

### Phase 2: memory and durability

- `internal/memory`: Consolidator (summaries to `memory/history.jsonl`) and
  Dream (ephemeral agent run with a restricted registry, cursor advanced only
  on success).
- SQLite session store outside the workspace, with a generation check.
- `internal/mcp`: stdio and streamable HTTP transports, owned by the
  application rather than the loop.

### Phase 3: reach

- `internal/channels`: Telegram, Slack, and others, each self-contained.
- `internal/cron`: scheduler, heartbeat, automations bound to sessions.
- `internal/gateway` and `internal/api`: long-running daemon and an
  OpenAI-compatible API.

### Phase 4: hardening and UI

- Process sandboxes: bwrap on Linux, Seatbelt on macOS. A configured backend
  that cannot start fails closed.
- Full SSRF protection: DNS pinning, CGNAT/metadata ranges, whitelist.
- WebUI.

## Design decisions we keep

- **Loop vs Runner split.** The `Loop` owns the channel-facing turn
  (sessions, context, delivery); the `Runner` owns the provider/tool loop.
  Features that can live in a tool, channel or MCP server stay out of both.
- **Registry-based extension.** Tools, providers and channels register with
  a registry instead of being wired by hand. Tool definitions are sorted
  stably so the prompt prefix stays cacheable.
- **History is durable, poisonable data.** Validate at the edge, write
  atomically, never persist error turns, and advance cursors only after
  success.
- **Errors are results.** Tool failures go back to the model as error results;
  they never crash a turn.
- **Security is a feature.** Workspace containment and the shell guard live
  in the tools from day one; the shell guard is best-effort and not a sandbox.
- **No package-level mutable state.** Everything is wired explicitly with
  `context.Context` for cancellation.
