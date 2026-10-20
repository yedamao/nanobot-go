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
| `nanobot/cli/gateway_runtime.py`, `nanobot/gateway` | `cmd/nanobot` (`gateway.go`, `runtime.go`) | Daemon wiring, health endpoint, shutdown |
| `nanobot/api`                            | `internal/api`                   | OpenAI-compatible HTTP API |
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
  - Done (phase 3): the gateway schedules Dream as the cron system job
    `dream` (`dream.intervalH`, `dream.cron`).
  - Next: `/dream-log` and `/dream-restore` style commands over the git
    history.
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

Phase 2 is complete; phase 3 is in progress.

### Phase 3: reach

- Gateway: long-running daemon.
  - Done: `nanobot gateway` (config `gateway.host`/`port`/
    `shutdownTimeout`, `--host`/`--port`), built by the same
    `buildRuntime` as `agent`; `Loop.Serve` (stop accepting while
    in-flight turns finish); `GET /healthz` (and `/health`); graceful
    shutdown on SIGINT/SIGTERM with a forced exit on the second signal;
    `status` rows for the gateway and channels.
  - Not ported: local triggers (`nanobot/triggers`), heartbeat
    (`HEARTBEAT.md`; Dream now runs as a gateway job), the readiness payload
    (`process`/`ready`/`websocket`), restart notices, config hot reload,
    the on-demand gateway client lease, WebUI.
- `internal/channels`: Telegram, Slack, and others, each self-contained.
  - Done: the `Channel` interface, `Registry` (factories keyed by name,
    built from config `channels.<name>` when `enabled`), `Manager`
    (start, outbound dispatch by channel name with retries, drain, stop),
    `Permanent` errors.
  - Next: Telegram (`internal/channels/telegram`).
- `internal/cron`: scheduler, heartbeat, automations bound to sessions.
  - Done: `Schedule` (`at`, `every`, 5-field `cron` + IANA `tz`, own
    parser, embedded tzdata), `Store` (`<workspace>/cron/jobs.json`,
    flock + atomic writes), `Service` (timer goroutine capped at 5
    minutes and woken by changes, injectable clock, run bookkeeping,
    delete-after-run, protected system jobs, `RunJob`), `BoundRunner`
    (session-bound turns through `Loop.ProcessMessage`, reply to the
    origin chat), the `cron` tool (add/list/remove) with the turn's chat
    taken from `tools.TurnFrom`, gateway wiring (start after the loop,
    stop with it, Dream system job), `nanobot cron list|remove|run`.
  - Not ported: heartbeat (`HEARTBEAT.md` and its target-chat picker),
    local triggers, run records (`cron/runs/`), run history, the WebUI
    automation views and `change_binding`.
- `internal/api`: an OpenAI-compatible API.

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
- **Dream is a CLI command and a gateway cron job.** Like nanobot, the
  gateway runs Dream from its cron service (see Cron below); `nanobot
  dream` also does one pass on demand. A non-waiting flock on
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

### Gateway and channels

- **Health at `/healthz`, plain liveness.** nanobot serves `GET /health`
  with a readiness payload (`status`, `process`, `ready`, `websocket`) and
  503 when the WebSocket channel is down. nanobot-go serves `/healthz`
  and `/health` with `{"status":"ok"}` while the process is up; readiness
  arrives with channels that need it. It uses `net/http` with 2-second
  timeouts instead of nanobot's hand-written HTTP/1.0 server and its
  64-connection cap. Port 0 (any free port) is accepted.
- **In-flight turns get a deadline.** nanobot cancels running tasks on
  shutdown (keeping checkpoints so a later gateway can offer to
  continue). nanobot-go stops consuming inbound messages, lets the turns
  in flight finish for `gateway.shutdownTimeout` seconds (new; default
  30), then cancels them; their replies are still delivered before the
  channels stop. Messages still queued on the bus are not processed.
  Delivering the last replies and stopping the channels and health
  endpoint is bounded by another 10 seconds. A second signal calls
  `os.Exit(1)` at once (nanobot cancels the remaining tasks and still runs
  its bounded cleanup).
- **Signals.** The first SIGTERM cancels the command's context (as
  before); `main` now forwards later SIGTERMs so the gateway can treat
  them as "force", like a second Ctrl-C. `agent` and `dream` behave as
  before.
- **Channels start, then run in the background.** nanobot's
  `BaseChannel.start` is a long-running coroutine. A Go `Channel.Start`
  returns once the channel runs and the channel owns its goroutines
  until `Stop`; `Send` takes a context. A channel that fails to start is
  logged and skipped, like nanobot, but is not retried.
- **One sender per channel.** nanobot schedules one task per message
  with FIFO per `(channel, chat_id)` and global limits (32 sends, 256
  pending). nanobot-go gives each channel one sender goroutine with a
  256-message queue: FIFO per channel, and a channel that is slow or
  retrying does not hold up the others (but does hold up its own chats).
- **Fixed retry policy.** Sends are attempted four times, waiting 1s, 2s
  and 4s in between (nanobot's delays). nanobot's default is three
  attempts via `channels.sendMaxRetries`, which is not ported. A channel
  marks errors not worth retrying with `channels.Permanent` (nanobot:
  `DeliveryRejectedError`/`should_retry_send_error`).
- **No delivery outbox, progress or streaming.** nanobot persists
  scheduled deliveries in `delivery/outbox.sqlite3`, streams deltas,
  sends progress/tool hints/reasoning, and suppresses duplicate replies.
  nanobot-go delivers only final replies; on a forced or timed-out stop,
  queued replies are dropped with a warning.
- **Channel config is a raw object per channel.** nanobot's `channels`
  section also has global keys (`sendProgress`, `sendToolHints`,
  `sendMaxRetries`, ...) next to the channel sections; nanobot-go only
  has channel sections, each a JSON object with `enabled`, decoded
  strictly by the channel's factory. Multi-instance channels and plugin
  discovery are not ported; channels register with a `channels.Registry`
  at startup. An enabled section with no registered channel fails the
  gateway's startup instead of logging a warning.
- **No local triggers.** nanobot's file-based trigger queue
  (`nanobot/triggers`) is skipped for now: it needs the trigger store,
  session admission and its own runner, so it is not a small addition.

### Cron

- **Own cron parser.** nanobot uses croniter; nanobot-go parses standard
  5-field expressions itself (std lib only): `*`, values, ranges, lists,
  steps, month/weekday names, `0`/`7` for Sunday, `@yearly`...`@hourly`,
  and Vixie's rule that when both day fields are restricted either may
  match. Not supported: a seconds or year field, `L`, `W`, `#`, `H`.
  In a daylight-saving fall-back, times in the repeated hour match on both
  passes; times skipped by a spring-forward never match that day.
- **Times on disk are RFC 3339.** `jobs.json` keeps nanobot's shape
  (`version`, `jobs[]` with `id`, `name`, `enabled`, `schedule`,
  `payload`, `state`, `deleteAfterRun`) but stores times as RFC 3339
  strings (`at`, `nextRun`, `lastRun`, `createdAt`, `updatedAt`) instead
  of `*AtMs` epochs; intervals stay `everyMs`. The chat lives in a
  `binding` object (`channel`, `chatId`, `sessionKey`, `metadata`) instead
  of the payload's `sessionKey`/`originChannel`/`originChatId`/
  `originMetadata`, and the legacy `deliver`/`channel`/`to` fields and
  their migration are not ported. The file is not compatible with
  nanobot's.
- **No action log; a lock instead.** nanobot's live service owns the store
  and other processes append to `cron/action.jsonl`, merged on the next
  load. nanobot-go has no owner: every change (from the gateway, `nanobot
  agent` or `nanobot cron`) re-reads `jobs.json`, edits it and replaces it
  atomically under an in-process mutex and an flock on `cron/.jobs.lock`
  (Unix; elsewhere only the mutex, so concurrent processes can lose an
  update). The gateway notices other processes' changes when it next
  wakes, within five minutes, as in nanobot. Nothing prevents two
  gateways on one workspace from both running the jobs.
- **A corrupt store stops the gateway.** nanobot renames an unreadable
  `jobs.json` to `.corrupt-<ts>` and refuses to start. nanobot-go leaves
  it in place, refuses to start the gateway (and every other change),
  and asks for it to be fixed or removed. A newer `version` is refused too.
- **Restarts keep the schedule.** nanobot recomputes every job's next run
  when the service starts, so an `every` job is pushed back by a restart
  and a one-time job missed while it was down never runs. nanobot-go keeps
  next runs that are still ahead, recomputes those that passed (missed
  `every`/`cron` runs are skipped, as in nanobot), and runs a missed `at`
  job once at startup. Re-registering the Dream job with an unchanged
  schedule keeps its next run.
- **Less bookkeeping.** State is `nextRun`, `lastRun`, `lastStatus`,
  `lastError` (500 characters) and `runCount`; there is no
  `runHistory` and no per-run records under `cron/runs/`. A run cut short
  by shutdown is not recorded, so the job stays due (nanobot re-raises the
  cancellation, with the same effect).
- **Unbound jobs are skipped when due**, recorded as `skipped`; nanobot
  also disables them when it loads the store. The tool cannot create one.
- **No defer-until-idle coordinator.** nanobot holds a cron turn back while
  its session is busy. nanobot-go submits it through
  `Loop.ProcessMessage`, which already queues it behind the session's
  turns. The rendered reminder prompt is saved to the session as the
  user message (nanobot saves "Scheduled cron job triggered: ..."), and
  the prompt drops nanobot's line about the message tool, which does not
  exist here. The trigger marker is `_cron_trigger` in the inbound
  metadata; the tool refuses `add` in such a turn, like nanobot.
- **Turn context through `context.Context`.** nanobot's tool reads a
  request-context variable; nanobot-go's Loop attaches `tools.Turn`
  (channel, chat, session key, metadata) to every turn's context. Dream's
  turns have none, and Dream's registry has no cron tool anyway. Binding
  metadata is copied through JSON, dropping values that do not encode.
- **Default zone is the host's.** nanobot defaults naive `at` times and
  `cron_expr` without `tz` to `agents.defaults.timezone` (UTC unless
  set). nanobot-go has no timezone setting yet: it uses the gateway
  host's local zone, stored as "no zone" so the job follows the host.
  Explicit `tz` values are stored as given; `"Local"` is rejected.
- **`at` parsing.** Accepted forms follow Python's `fromisoformat`
  (date, `T` or space, optional seconds and fraction, `Z` or offset).
- **Dream schedule.** `dream.intervalH` (default 2) and `dream.cron` as in
  nanobot; nanobot's `dream.enabled` is `intervalH: 0` without `cron`, and
  `dream.cron` uses the host's zone. `modelOverride` is not ported.
- **`nanobot agent` stores jobs, with a note.** As in nanobot, the tool is
  available outside the gateway and jobs land in the store for the
  gateway to run; nanobot-go adds a note to the result saying so. A job
  made in the CLI session is bound to channel `cli`, which the gateway
  does not run: the turn still runs in `cli:direct` and its reply is
  dropped with a warning.
- **`nanobot cron` is new.** nanobot has no cron subcommand (jobs are
  managed through the agent and the WebUI). `list` and `remove` need no
  provider; `run` runs the job's turn in the CLI process and prints the
  reply instead of delivering it.
- **No heartbeat.** nanobot's `gateway.heartbeat` (a system job that reads
  `HEARTBEAT.md`, picks a recently active chat and posts the result after
  an evaluator pass) is not ported yet.
