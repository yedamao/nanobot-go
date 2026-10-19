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
| `nanobot/agent/context.py`               | `internal/agent` (`ContextBuilder`) | System prompt assembly |
| `nanobot/agent/tools`                    | `internal/tools`, `internal/tools/builtin` | `Tool` interface, `Registry`, built-in tools |
| `nanobot/agent/tools/mcp.py`, `mcp_oauth.py` | `internal/mcp`               | MCP client, tools wrapped as `mcp_<server>_<tool>` |
| `nanobot/agent/memory.py` (Consolidator, Dream) | `internal/memory`         | history.jsonl archive, long-term files, Dream job |
| `nanobot/agent/subagent.py`              | `internal/agent/subagent`        | Reuses `Runner` with a restricted registry |
| `nanobot/providers`                      | `internal/providers`             | `Provider` interface, OpenAI-compatible + Anthropic over `net/http`, retry, `Fallback` |
| `nanobot/session`                        | `internal/session`               | `Store` interface; SQLite (default) and JSONL |
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
  iteration limit with a final request that forbids tool calls) and `Loop`
  (one worker per session key, ordered within a session).
- `internal/config`: JSON config file.
- `cmd/nanobot`: interactive and one-shot CLI going through the bus.

### Phase 2: memory and durability

- `internal/memory`: Consolidator (summaries to `memory/history.jsonl`) and
  Dream (ephemeral agent run with a restricted registry, cursor advanced only
  on success).
  - Done: `Store` (append-only `history.jsonl`), `Consolidator`, the
    session marker (`Session.LastConsolidated`, `Session.Summary`), the
    Loop trigger and `/compact`, config `contextWindowTokens` and
    `consolidation`. The shell guard already denied writes to
    `history.jsonl` and `.dream_cursor`.
  - Done: long-term files (`memory/MEMORY.md` created on onboard and
    loaded by `ContextBuilder`; `SOUL.md`/`USER.md` already were),
    `Store.DreamCursor`/`Compact`, `DreamTools` (the restricted
    registry), the `Dream` job with git commits, `agent.DreamRun`, the
    `nanobot dream` subcommand and config `dream`.
  - Next: schedule Dream from `internal/cron` (phase 3); `/dream-log` and
    `/dream-restore` style commands over the git history.
- SQLite session store outside the workspace, with a generation check.
  - Done: the `session.Store` interface, `SQLiteStore` (one database per
    workspace at `<sessions>/<workspace id>/sessions.sqlite3`, WAL, a
    generation per session so a stale `Save` fails with
    `ErrSessionConflict`, schema versioning), a one-time import of the
    flat JSONL files in the sessions directory (renamed to
    `*.jsonl.migrated` afterwards), and `session.Open`, which the CLI
    uses. `JSONLStore` stays as the import source and for tests.
- `internal/mcp`: stdio and streamable HTTP transports, owned by the
  application rather than the loop.
  - Done: config `tools.mcpServers` (`type`, `command`, `args`, `env`,
    `cwd`, `url`, `headers`, `toolTimeout`, `enabledTools`, plus
    `enabled`); `mcp.Manager` on the official Go SDK
    (`github.com/modelcontextprotocol/go-sdk`) with stdio, streamable HTTP
    and SSE, concurrent connects with a per-server timeout, per-server
    status, `mcp_<server>_<tool>` wrappers with schema normalization,
    per-call timeouts and one reconnect-and-retry on a dead session;
    `Registry.RegisterEach` and a variadic `Unregister` so a server's tools
    appear and disappear atomically; CLI wiring in `agent` (connect before
    the first turn, close after the loop) and `status [--mcp]`.
  - Not ported: OAuth (`auth: "oauth"`), resources and prompts as tools,
    hot reload (`MCPProvider.reload`), `tools/list_changed`.

Phase 2 is complete; phase 3 starts next.

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
- **Errors are results.** Tool failures go back to the model as error results
  (flagged `is_error` on tool messages, sent as `tool_result.is_error` to
  Anthropic); they never crash a turn.

## Deliberate deviations from nanobot

- **Forced final answer keeps tools defined.** After the iteration budget is
  spent, nanobot sends the finalization request with `tools=None`. Anthropic
  rejects a request whose history contains `tool_use`/`tool_result` blocks
  but defines no tools, so nanobot-go keeps the definitions and sends
  `tool_choice: none` (supported by both Anthropic and OpenAI). Tool calls in
  that response are still ignored and replaced by the fallback message.
- **exec passes PATH.** Like nanobot, commands get a minimal environment
  plus `tools.exec.allowedEnvKeys`, never the agent's API keys. nanobot
  relies on a login shell to rebuild PATH; nanobot-go runs `sh -c`, so
  PATH (and a few locale/temp variables) are passed through as well.
- **Security is a feature.** Workspace containment and the shell guard live
  in the tools from day one; the shell guard is best-effort and not a sandbox.
  Where nanobot resolves a path and then opens it by name, the Go file tools
  resolve with `Workspace.ResolveRead/ResolveWrite` and then do every
  operation (open, mkdir, temp file, rename) through the `os.Root` returned
  by `Workspace.OpenRoot`, which closes the check-then-open race. Note that
  `os.Root` refuses absolute symlink targets, so callers pass it resolved
  (symlink-free) paths. Workspace membership also falls back to comparing
  file identity, so differently-cased paths on case-insensitive filesystems
  are not rejected.
- **No package-level mutable state.** Everything is wired explicitly with
  `context.Context` for cancellation.
- **Consolidation runs before the turn, in the Loop.** nanobot's
  `ContextGovernor` can compact inside a running request. nanobot-go keeps
  the Runner unaware of memory: before each turn the Loop asks its
  `memory.Consolidator` to `Plan` (more than `maxHistoryMessages`
  unsummarized messages, or an estimated prompt over
  `contextWindowTokens` minus `maxTokens` minus 1024) and `Consolidate`.
  A single turn that outgrows the window mid-run is not compacted.
- **Consolidation keeps recent turns verbatim.** nanobot's checkpoint
  archives the whole accepted prefix. nanobot-go archives up to the start
  of a user turn and keeps the newest turns that fit in half of each limit
  (at least the latest turn), so consolidation does not repeat every turn
  and the replay always starts with a user message.
- **The summarizer sees a plain-text transcript.** nanobot replays the
  messages (with tool definitions) and asks for a checkpoint. nanobot-go
  renders them as text (`USER: ...`, `[tool call] ...`, each message capped
  at 4000 characters, the middle dropped if the window is too small), so the
  request has no tool_use/tool_result pairing constraints and no tools.
- **No hidden marker message.** nanobot inserts a hidden user message at
  the boundary; nanobot-go stores only the index (`last_consolidated` in
  the session file's metadata line) and the summary (`last_summary`). A
  marker out of range on load is reset to 0, replaying everything.
- **No raw-archive fallback.** When summarization fails, nanobot dumps the
  raw messages into `history.jsonl` and advances. nanobot-go leaves the
  marker where it is, logs the error, and replays the plain
  `maxHistoryMessages` window for that turn; the next turn retries. Nothing
  is written to `history.jsonl` and no message is lost.
- **`history.jsonl` cursors come from a full scan.** nanobot keeps a cursor
  counter file; nanobot-go scans the file for the highest valid cursor on
  each append (appends are rare). Appends and compaction are serialized
  within a process by a mutex and across processes by an flock on
  `memory/.history.lock` (Unix only), so an append can neither reuse a
  cursor nor land in a file compaction is replacing. The file is opened
  without following symlinks.
- **Dream is a CLI command, not a cron callback.** nanobot runs Dream from
  the gateway's cron service. Until `internal/cron` exists, `nanobot
  dream` does one pass and is scheduled externally; a non-waiting flock on
  `memory/.dream.lock` refuses overlapping runs. The turn goes straight to
  the `Runner` (`agent.DreamRun`) rather than through the `Loop`, so there
  is no `dream:<timestamp>` session to prune. The `memory` package takes
  the turn as a `DreamRunFunc` so it does not import `agent`.
- **Dream sees longer entries.** nanobot cuts each history entry in the
  Dream prompt to 1000 characters; once the cursor moves, the rest is
  never seen again. nanobot-go cuts at 8000 characters, about the default
  summary limit. Entries also show their session key.
- **A corrupt `.dream_cursor` is an error.** nanobot reads it as 0, which
  replays the whole archive. nanobot-go refuses to run (or to compact) and
  asks for the file to be fixed or deleted.
- **Dream's write restriction is a wrapper.** nanobot gives its file tools
  an allowed directory plus extra allowed files. nanobot-go wraps the
  ordinary `write_file`/`edit_file` in a guard that resolves the target
  through symlinks and accepts only `SOUL.md`, `USER.md`,
  `memory/MEMORY.md` and `skills/...`. The tool resolves the path again,
  so only a symlink swapped in between by another process could redirect
  the write (still within the workspace). There is no `apply_patch` tool.
- **Git through the `git` binary.** nanobot uses dulwich. nanobot-go runs
  `git` (30s timeout per command, `GIT_DIR`-style variables removed,
  hooks and signing skipped, author `nanobot <nanobot@dream>`). A missing
  binary is a logged no-op. Like nanobot, a workspace inside another
  repository is left alone, and a new repository gets a `.gitignore` that
  versions only the memory files; unlike nanobot, an existing `.gitignore`
  is not merged into, and there is no initial commit. Only the memory
  paths are committed (`git commit -- <paths>`), the cursor is not
  tracked, and the body lists `git diff --name-status` instead of a diff
  summary. Edits from an incomplete run are committed too, as in nanobot.
- **Trimming runs after every pass**, including one with nothing to
  process, like nanobot's `finally` block.
- **The workspace ID comes from its path.** nanobot stores a random ID in
  the workspace to locate its session database. nanobot-go hashes the
  canonical workspace path instead, so the workspace is never written to;
  a moved workspace starts with an empty database (the old one stays on
  disk under the previous ID).
- **The JSONL import reads one directory.** nanobot also imports
  `<workspace>/sessions/*.jsonl` from older layouts; nanobot-go never kept
  sessions in the workspace, so it imports only the flat files in its
  sessions directory. As in nanobot, one unreadable or duplicate file
  aborts the whole import and leaves the sources unchanged.

### MCP

- **No OAuth yet.** nanobot's `auth: "oauth"` (browser authorization,
  tokens stored outside the config) is not ported; the key is rejected as
  unknown. Put a token in `headers`, ideally as `${VAR}`.
- **Tools only.** With `enabledTools: ["*"]` nanobot also registers each
  resource as a read-only `mcp_<server>_resource_<name>` tool and each
  prompt as `mcp_<server>_prompt_<name>`. nanobot-go skips both: each needs
  its own wrapper, naming, result rendering and reconnect handling, and
  most servers are used for their tools. Planned as a follow-up.
- **Images are described, not stored.** nanobot saves image results as
  artifacts and tells the model to deliver them with the message tool.
  nanobot-go has no artifacts or message tool yet, so images, audio and
  blobs become a placeholder (`[image: image/png, 5120 bytes]`); text
  resources embedded in a result are inlined.
- **SSE is supported** through the SDK, inferred like nanobot from a URL
  ending in `/sse`.
- **No SSRF filter or TCP probe on MCP URLs.** nanobot validates MCP URLs
  against its SSRF rules (blocking localhost and private ranges unless
  whitelisted), pins DNS and probes the port before connecting.
  nanobot-go treats the URL as trusted operator configuration, so local
  servers work without a whitelist; the per-server connect timeout
  replaces the probe.
- **Concurrent connects with a timeout.** nanobot connects servers one by
  one with no overall limit. nanobot-go connects them concurrently, each
  within 30 seconds (10 in `status --mcp`). A failed server is not retried
  in the background (nanobot retries on its next readiness check); calling
  one of its tools, if it ever registered any, reconnects at most once per
  30 seconds.
- **Reconnect keeps the registrations.** On a terminated session nanobot
  unregisters the server's tools, reconnects and registers them again.
  nanobot-go's wrappers look up the server's current session on every
  call, so a reconnect touches no registry state and the tool list (and
  prompt cache) stays stable mid-run. Tools added or removed on the server
  after the first connection appear only after a restart. nanobot's
  separate "retry a transient error once after 1s" is folded into the
  same reconnect-once-and-retry, triggered by connection-level errors
  (closed connection, EOF, broken pipe, reset, missing HTTP session) and
  never by the server rejecting a call. Concurrent calls share one
  reconnect.
- **Close cancels in-flight calls.** The SDK's session `Close` waits for
  outstanding calls; nanobot-go cancels them first, so shutdown does not
  wait for a slow tool. Stdio servers get their stdin closed, then
  SIGTERM, then SIGKILL, 2 seconds apart.
- **Collisions are skipped, not overwritten.** nanobot's registry replaces
  a tool registered under the same name. nanobot-go keeps the first tool
  (built-ins always win) and logs the skipped one. Sanitizing matches
  nanobot (invalid characters to `_`, runs of `_` collapsed); a name over
  64 characters ends in 8 hex digits of the SHA-1 of the *unsanitized*
  name (nanobot hashes the sanitized one), so two tools that differ only
  in punctuation still get distinct long names.
- **`readOnlyHint` is honored.** nanobot treats every MCP tool as having
  side effects. nanobot-go marks a tool read-only when the server
  annotates it `readOnlyHint: true`, which only lets it run alongside other
  read-only tools in the same batch.
- **Schemas: refs are inlined.** nanobot hoists arbitrary local JSON
  pointers into `$defs` and keeps `$ref`. nanobot-go inlines local refs
  (better for providers without `$ref` support) and drops `$defs`/
  `definitions` when nothing refers to them; recursive or unresolvable
  refs stay as `$ref` with their definitions. Nullable unions are
  flattened with `"nullable": true` as in nanobot.
- **`${VAR}` only in the MCP entry, at connect time.** nanobot resolves
  `${VAR}` across the whole config when loading it. nanobot-go expands
  `args`, `env`, `url` and `headers` of each MCP server when it connects;
  an unset variable fails only that server, and `nanobot status` works
  without the variables set.
- **`enabled` switch.** New; nanobot has no way to keep a disabled entry.
- **Stdio environment.** Like the Python SDK's default, stdio servers get
  `HOME`, `LOGNAME`, `PATH`, `SHELL`, `TERM`, `USER` (plus `TMPDIR`,
  locale and Windows essentials) and their `env`. nanobot's Windows
  wrapping of `npx`/`.cmd` launchers in `cmd /c` is not ported. Server
  stderr is inherited by nanobot; nanobot-go keeps its last 2 KB for the
  connection error and forwards it only with `--verbose`.
- **Secrets are redacted from errors.** Configured env and header values,
  the URL query and password are replaced with `[redacted]` in connection
  and call errors, which `status --mcp` prints.
