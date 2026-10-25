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
| `nanobot/cli`                            | `cmd/nanobot`, `internal/cli`    | `agent`, `gateway`, `serve`, `onboard`, `webui` subcommands |
| `nanobot/cron`, `nanobot/triggers`       | `internal/cron`                  | `at`/`every`/`cron` schedules, session-bound jobs |
| `nanobot/channels`                       | `internal/channels`, `internal/channels/<name>` | `Channel` interface, manager, one package per channel |
| `nanobot/channels/telegram`              | `internal/channels/telegram`     | Bot API over `net/http`: long polling, allowFrom, plain-text replies |
| `nanobot/channels/weixin`                | `internal/channels/weixin`       | iLink bot API over `net/http`: QR login, long polling, context tokens, text replies |
| `nanobot/pairing`                        | `internal/pairing`               | Pairing codes and approved senders (`pairing.json`), `nanobot pairing` |
| `nanobot/security`, `agent/tools/sandbox.py` | `internal/security`          | Workspace containment, shell guard, SSRF, sandbox backends |
| `nanobot/cli/gateway_runtime.py`, `nanobot/gateway` | `cmd/nanobot` (`gateway.go`, `runtime.go`) | Daemon wiring, health endpoint, shutdown |
| `nanobot/api`                            | `internal/api`                   | OpenAI-compatible HTTP API |
| `nanobot/channels/websocket`             | `internal/channels/websocket`    | Browser chat over WebSocket, mounted on the gateway's HTTP server |
| `webui/`                                 | `webui/` (source), `internal/webui` (committed build, embedded via `embed`) | Lean Vite + React chat app served by the websocket channel (first slice) |

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
    the on-demand gateway client lease, `gateway --background`. (The
    WebUI and a simplified `nanobot webui` launcher landed in phase 4.)
- `internal/channels`: Telegram, Slack, and others, each self-contained.
  - Done: the `Channel` interface, `Registry` (factories keyed by name,
    built from config `channels.<name>` when `enabled`), `Manager`
    (start, outbound dispatch by channel name with retries, drain, stop),
    `Permanent` errors.
  - Done: Telegram (`internal/channels/telegram`): Bot API client over
    `net/http` (`getMe` at start, `deleteWebhook`, `getUpdates` long
    polling with offset, backoff and 429 `retry_after`, `sendMessage`,
    `sendChatAction`), nanobot's `is_allowed` (`*`, ID, username,
    `id|username`, pairing store), DM pairing codes, `groupPolicy`
    `mention|open` (mention entities, text mentions, replies to the bot,
    commands), `/cmd@bot` normalization, reply context, forum-topic
    sessions, plain-text chunking at 4096 UTF-16 units, typing indicator,
    token redaction; config `channels.telegram` (`token` or
    `NANOBOT_GO_TELEGRAM_TOKEN`, `allowFrom`, `apiBase`, `pollTimeout`,
    `proxy`, `groupPolicy`, `replyToMessage`), a `status` token row.
    `internal/pairing` (`pairing.json` next to the config, flock + atomic
    writes) and `nanobot pairing list|approve|deny|revoke`.
  - Not ported (Telegram): webhook mode, media in either direction
    (download, transcription, media groups, sending files), Markdown to
    HTML rendering, rich messages, streaming edits, reasoning/progress
    messages, inline keyboards and callback queries, reactions, the bot
    command menu (`setMyCommands`), the per-session reorder buffer,
    polling-stall detection, the WebUI setup validator.
  - Done: WeChat (`internal/channels/weixin`, personal WeChat through
    Tencent's iLink bot API, protocol of `@tencent-weixin/openclaw-weixin`
    2.4.6 as in nanobot): client with nanobot's headers (`X-WECHAT-UIN`,
    `iLink-App-*`, `SKRouteTag`) and `base_info`, `getupdates` long
    polling with the `get_updates_buf` cursor and the server-suggested
    timeout, `sendmessage` text replies (Markdown sanitizing, 1800-character
    chunks with balanced fences, deterministic `client_id`s), cached
    `context_token`s refreshed through `getconfig` after 60s, the local
    budget of 8 messages per token and held replies (3 per user) until
    the user writes again, typing tickets and `sendtyping`,
    `notifystart`/`notifystop`, message dedup, stale-token (-14)
    handling, `account.json` state (nanobot's layout, 0600, file lock,
    never overwriting a newer login), allowFrom + pairing; the QR login
    (`nanobot channels login weixin [--force]`: `get_bot_qrcode` with
    `local_token_list`, `get_qrcode_status` with redirect host,
    verification code, refresh and binding statuses); config
    `channels.weixin` (`token` or `NANOBOT_GO_WEIXIN_TOKEN`, `allowFrom`,
    `baseUrl`, `routeTag`, `stateDir`, `pollTimeout`) and a `status` login
    row.
  - Not ported (WeChat): media in either direction (CDN download and
    upload, AES-128-ECB, voice transcription), progress/tool-hint
    messages and structured progress items, block streaming, the WebUI
    connect flow, typing tickets in `account.json`, terminal QR rendering.
    WeCom (`nanobot/channels/wecom`, the enterprise WebSocket SDK) is a
    separate channel and not ported.
  - Next: other channels, each in its own package.
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
  - Done: `api.Server` (`GET /health`, `GET /v1/models`,
    `POST /v1/chat/completions` with SSE streaming, bearer token, refusal
    of a non-loopback host without a key, body cap, per-turn timeout,
    nanobot's error JSON, `X-Request-ID`), `Loop.Process` (the reply with
    the turn's token usage), config `api` (`enabled`, `host`, `port`,
    `timeout`, `apiKey`; `NANOBOT_GO_API_KEY`), `nanobot serve`
    (`--host`/`--port`/`--timeout`), the API inside the gateway when
    `api.enabled`, a `status` row; `security.IsLoopbackHost`.
  - Not ported: file uploads (multipart and base64 images), token
    streaming, the WebUI-managed API process (`nanobot/api/runtime.py`).

### Phase 4: hardening and UI

- Process sandboxes: bwrap on Linux, Seatbelt on macOS. A configured backend
  that cannot start fails closed.
  - Done: `internal/security/sandbox` (port of
    `nanobot/agent/tools/sandbox.py`): a `Backend` per sandbox producing
    an argv (no shell re-parsing), `Wrap`, bind normalization
    (`NormalizeBinds`: `~`/`$VAR`, symlinks, duplicates, dropping paths
    equal to or containing the workspace), `Host.Check`/`Available` (OS,
    binary, probe run); nanobot's bwrap mounts and Seatbelt profile
    (system subpaths and literals, traversable ancestors, parent deny
    before the workspace allow, RO-bind write/unlink protection,
    unrestricted network, `cd <cwd> || exit 1`). Config
    `tools.exec.sandbox`, `sandboxRoBinds`, `sandboxRwBinds`; exec wraps
    after the shell guard; bind roots pass the guard's path check while a
    sandbox is active; a sandbox forces `restrictToWorkspace` for the file
    tools; a `status` row.
  - Not ported: the media directory bind (no media yet), the
    `NANOBOT_WORKSPACE_SANDBOX_ENFORCED` marker and the
    `my(action="check", key="workspace_sandbox")` report, `pathPrepend`/
    `pathAppend`, the model-only access-restriction context (the exec tool
    description says commands are sandboxed instead).
- Full SSRF protection: DNS pinning, CGNAT/metadata ranges, whitelist.
  - Done: `security.Policy` (`ssrf.go`, port of `nanobot/security/network.py`):
    `ParseWhitelist`/`NewPolicy`, `Allow` (whitelist, else not
    `IsBlockedIP`), `ResolveAndValidate` (every answer must be allowed,
    mixed public/private rejected), `CheckURL`/`CheckTarget`, a pinned
    `DialContext`/`NewTransport` that dials only validated addresses,
    `CheckRedirect` re-checking each hop, and `WithPinnedDNS` sharing one
    lookup per host across check, redirects and dials. Config
    `tools.ssrfWhitelist`, validated at load. Wired into `web_fetch` and
    the MCP streamable HTTP and SSE clients.
  - Not ported: `trust_remote_dns`, `allow_loopback`, `env_proxy_*`
    helpers beyond MCP's proxy routing (see the deviations below).
- WebUI.
  - Done (first slice): `internal/channels/websocket`, a channel named
    `websocket` (config `{"enabled": bool}`, default off) that
    implements the new optional `channels.Mounter` and mounts `GET /ws`
    and the embedded page (`internal/webui`: `GET /` redirecting to
    `GET /ui/`) on the gateway's health server; a JSON wire protocol
    (`hello`, `chat` or plain text, `message`, `error`); one chat id per
    connection, random or resumed with `?chat_id=`, session
    `websocket:<chat id>`, replies fanned out to every socket of the
    chat; loopback-only (startup refusal, `Host` check, same-origin
    upgrades); WebUI-aware server timeouts; a `WebUI:` banner line.
  - Done: history on reload/reconnect. After `hello` the channel sends a
    `history` frame (user and assistant texts of the stored session,
    newest 200 messages / 256 KiB), loaded through
    `websocket.Options.Sessions` (the gateway's `session.Store`); the
    page redraws from it and shows connection status and an empty
    state.
  - Done: live tool progress and stop. Bus-driven turns publish a
    progress update per tool call (`OutboundMessage.Metadata["_progress"]`,
    `bus.IsProgress`) with a hint from `agent.ToolHint` (a small port of
    `utils/tool_hints.py`); the channel sends it as a `progress` frame and
    the page shows it in the thinking bubble. `Loop.Cancel` and `/stop`
    (also the `stop` frame and a Stop button) cancel a session's turn in
    flight.
  - Done: a chat list. `GET /ui/chats` (the channel's, loopback-gated)
    lists the stored `websocket:*` sessions, newest 50, each with
    `chat_id`, `updated_at`, a title (first user message) and a preview
    (last text); the page shows them in a sidebar, highlights the
    current chat and switches by reconnecting with `?chat_id=`.
  - Done: chat rename and delete. `PATCH /ui/chats/{chat_id}`
    (`{"title":"..."}`, at most 60 characters; empty clears it) stores a
    custom title in the session metadata (`title`, `title_user_edited`),
    which the list prefers over the first user message;
    `DELETE /ui/chats/{chat_id}` deletes the session (204, also when it
    is missing). Both are loopback-gated, same-origin only and limited
    to `websocket:*` sessions, and go through the `session.Manager` so
    the agent loop's cache stays in step. The sidebar has rename (a
    prompt) and delete (with a confirm) buttons per chat; deleting the
    current chat starts a new one.
  - Done: a read-only status panel. `GET /ui/status` (the channel's,
    loopback-gated, mounted only when `websocket.Options.Status` is set)
    returns version, Go version, start time and uptime, model, resolved
    provider name, workspace base name, configured channels (enabled,
    running), and API/exec/web_fetch flags; the gateway fills it from
    its config and `Manager.Running()`. The page shows it in a dialog.
  - Done: a safe settings whitelist. `GET`/`PATCH /ui/settings`
    (loopback-gated, PATCH same-origin only, mounted only when
    `websocket.Options.Settings` is set) read and save `model`,
    `provider`, `tools.exec`, `tools.web_fetch`, `tools.exec_sandbox`
    and `channels.<name>.enabled` (configured telegram/weixin/websocket
    sections only). The channel checks the body against an exact key
    schema and refuses secret-looking key names at any depth; the
    gateway's `webSettings` loads the config file afresh, mutates only
    those fields in place (a channel's `RawMessage` keeps its other
    keys), validates and saves it. Changes apply on restart: the view
    lists `restart_required` (saved differs from running) and
    `env_overrides`. The Status dialog has a form for them.
  - Done: token streaming. `ChatRequest.OnDelta` makes the
    OpenAI-compatible provider (and so DeepSeek) and the Anthropic
    provider send `"stream": true` and parse the SSE events; `RunSpec.Stream` passes each delta to
    `Hooks.OnDelta`, and for bus-driven turns `progressHooks` publishes
    it as an `OutboundMessage` marked `Metadata["_delta"]`
    (`bus.IsDelta`) holding only the new text. The channel sends it as a
    `delta` frame; the page appends it to a live assistant bubble, which
    the final `message` frame replaces. Deltas are never saved.
  - Done: React scaffold. `webui/` is a new Vite + React + TypeScript
    app (React its only runtime dependency) that replaces the plain
    page with the same protocols: the socket client (chat id in
    `localStorage`, backoff reconnect, `hello`/`history`/`progress`/
    `delta`/`message`/`error`, `chat` and `stop`), the live streaming
    bubble, the chat list with rename/delete and the Status and
    settings dialog. `npm run build` writes it to
    `internal/webui/static/`, which is committed and embedded, so Go
    needs no Node.
  - Done: markdown and code rendering. Assistant bubbles (and the
    streaming one, re-rendered per delta and memoized) go through
    `react-markdown` + `remark-gfm` + `remark-breaks`, where nanobot uses
    Streamdown. No `rehype-raw`: raw HTML is skipped; `urlTransform`
    keeps only http(s), mailto and relative URLs; images become links.
    Fences naming a language are highlighted by a small rehype plugin on
    `lowlight` with ~20 highlight.js grammars (not nanobot's
    react-syntax-highlighter), with a copy button; token colors come
    from the page's theme variables. User, error and progress bubbles
    stay plain. Not ported: KaTeX math, Mermaid, nanobot's remend-style
    repair of half-streamed markdown (an unclosed fence is already a code
    block under CommonMark), file chips and previews.
  - Done (simplified): the `nanobot webui` launcher (`cmd/nanobot/
    webuicmd.go`). It enables `channels.websocket` in the config
    (confirming on a terminal, or `--yes`; a missing config is created
    with its workspace), then attaches to a gateway already answering
    `/healthz` and `/ui/` on `gateway.host:gateway.port` (left running
    on exit) or starts `nanobot gateway --config … [--workspace …]
    --port N` as a child process in its own process group, waits up to
    20 seconds for it, prints the ready URL and opens the browser
    (`open`/`xdg-open`). Ctrl-C stops the child (interrupt, then kill
    after the shutdown timeout). A port held by something else, or a
    gateway without the WebUI, is a clear error. `--dev` runs `npm run
    dev` in `webui/` with `NANOBOT_GO_GATEWAY_URL` pointing Vite's proxy
    at the gateway and opens `http://127.0.0.1:5173/ui/`. Flags follow
    nanobot: `--port/-p`, `--gateway-port` (must equal `--port`),
    `--workspace/-w`, `--config/-c`, `--dev`, `--no-open`, `--yes/-y`,
    `--background` (deprecated, exits 1 with the replacement commands).
    Gaps against nanobot: no on-demand gateway lease, PID files or
    `gateway --background` daemon manager (the started gateway lives
    only as long as the command), no separate WebUI port (`--port`
    *is* the gateway port), no token or `bootstrapSecret` sign-in link
    (so a non-loopback `gateway.host` is refused rather than warned
    about), no WebUI bundle rebuild check (the build is committed), no
    log tail of a background gateway (the child's warnings and errors
    go to stderr), no WSL/Windows browser launcher.
  - Next WebUI slices: i18n, a richer
    layout and components (nanobot's Radix/Tailwind UI), file previews,
    the skills, models and channel pages listed below.
  - Not ported: nanobot's React app itself (its component tree, Tailwind,
    Radix, i18n, KaTeX/Mermaid), settings/models/
    channels/skills/apps/automations/cron/pairing pages (settings beyond
    the whitelist, the full model catalog and picker, provider key
    forms, channel tokens and allow lists, SSRF whitelist, MCP servers
    and OAuth, a restart button, hot-applying saved settings), topics list,
    chat archive, pins, search, bulk delete, generated titles,
    cleanup on delete (automations, cron jobs, memory tied to the chat),
    listing other channels' sessions,
    temporary chats, conversation groups,
    Anthropic `thinking_delta`, `StreamEndEvent` and the rest of the
    stream event taxonomy, `<think>` scrubbing of streamed text,
    structured tool events, reasoning, media and
    uploads, file previews, restart control, the
    on-demand gateway, LAN access, `token`/
    `tokenIssueSecret`/`websocketRequiresToken` auth, TLS
    (`sslCertfile`), the separate port 8765, Vite HMR.

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
- **Streaming is a request option.** nanobot's
  providers have a streaming chat method and its progress hook emits
  `StreamDeltaEvent`/`StreamEndEvent`. nanobot-go adds
  `ChatRequest.OnDelta`: the OpenAI-compatible provider then streams
  (`stream_options.include_usage` for the usage; keep-alives, a missing
  `[DONE]`, tool calls without `index` and servers that ignore `stream`
  are tolerated) and still returns the whole `ChatResponse`. Anthropic
  streams too, parsing the Messages SSE events over raw HTTP rather than
  through an SDK's `get_final_message`: `text_delta` pieces are the
  deltas, `tool_use` blocks are rebuilt from `input_json_delta` (never
  sent as deltas), `message_delta` gives `stop_reason` and usage, and
  the end of the reply is `message_stop` (an `error` event is an
  `APIError`). `thinking_delta` is dropped, as there is no thinking
  channel yet, and nanobot's idle timeout is left to ctx cancel.
  Only failures before the reply starts are retried; a stream cut off
  mid-reply is a connection error. `FallbackProvider` fails over a
  streamed request only while no delta has been passed on, since another
  provider's reply would be appended to the partial one. Only bus-driven
  turns stream (API and cron turns do not); later responses of a turn
  (after tool calls) start their deltas with a blank line, and the final
  reply, the only text saved, supersedes them. Streamed text is not
  scrubbed of `<think>` blocks (DeepSeek's `reasoning_content` is
  ignored).

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
- **SSRF filter, but no TCP probe on MCP URLs.** As in nanobot, HTTP and
  SSE servers are checked against the SSRF policy (localhost and private
  ranges blocked unless in `tools.ssrfWhitelist`), DNS is pinned and
  every request and redirect is validated; with an environment proxy the
  URL is checked locally and the proxy connects. nanobot also probes the
  port before connecting; nanobot-go's per-server connect timeout
  replaces the probe. (Before phase 4b nanobot-go trusted MCP URLs as
  configured; local HTTP servers now need a whitelist entry.)
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
  64-connection cap (with the WebUI mounted, only the header read keeps
  the 2-second bound; see [WebUI](#webui-and-the-websocket-channel)).
  Port 0 (any free port) is accepted.
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
- **No delivery outbox; minimal progress and streaming.** nanobot
  persists scheduled deliveries in `delivery/outbox.sqlite3`, streams
  deltas (editing Telegram messages as they grow), sends progress/tool
  hints/reasoning, and suppresses duplicate replies. nanobot-go delivers
  final replies plus one-string tool hints marked as progress
  (`bus.IsProgress`) and streamed deltas (`bus.IsDelta`), which are tried
  once and dropped quietly for unknown channels; only the websocket
  channel shows deltas, Telegram and WeChat drop them and send the final
  reply. On a forced or timed-out stop, queued replies are dropped with a
  warning.
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

### Telegram and pairing

- **No SDK.** nanobot uses python-telegram-bot; nanobot-go calls the Bot
  API with `net/http` and JSON bodies. Only long polling is supported
  (nanobot also has `mode: "webhook"`). Like PTB's `start_polling`, a
  webhook still set on the bot is deleted at startup (pending updates are
  kept). Polling asks only for `message` updates, so edits, channel posts
  and callbacks are never received.
- **Plain text out.** nanobot renders the model's Markdown to Telegram
  HTML (with a plain-text fallback on a parse error) or to Bot API 10.1
  rich messages. nanobot-go sends every reply as plain text with no
  `parse_mode`: Markdown markers show literally, but no model output can
  break delivery or inject markup. nanobot splits raw Markdown at 4000
  characters with fence balancing; nanobot-go splits plain text at
  Telegram's real limit, 4096 UTF-16 code units, at a newline, else a
  space, else between characters (never inside a surrogate pair). If a
  later chunk fails, the manager's retry resends the whole reply, so
  earlier chunks can appear twice (as in nanobot).
- **Text only in.** nanobot downloads photos, voice, audio, documents
  and video (transcribing voice), forwards locations and merges media
  groups. nanobot-go handles text and captions; a message with neither is
  skipped (a stranger's still gets a pairing code). Outbound media is
  not sent (logged).
- **Access check.** As in nanobot: `"*"`, then the sender ID
  (`id|username`, or `id`) and its ID or username parts in `allowFrom`,
  then the pairing store; an empty list denies everyone not paired.
  nanobot-go also accepts `@name` and matches usernames
  case-insensitively (Telegram usernames are case-insensitive), and
  accepts numbers in `allowFrom`. In groups the mention/reply check runs
  before the access check, so chatter not meant for the bot is not logged
  as "access denied" (nanobot checks access first; the outcome is the
  same: no reply).
- **Commands.** nanobot registers a command menu and handles `/start`
  and `/help` itself, forwarding the rest. nanobot-go answers `/start`
  with nanobot's greeting and forwards every other command (`/help`
  included) to the loop, which knows `/new`, `/compact` and `/help`.
  `/cmd@other_bot` is ignored instead of being treated as `/cmd`.
  Telegram-safe aliases (`/dream_log`, ...) are not mapped, since those
  commands are not ported.
- **Topic sessions only in topics.** nanobot scopes the session to
  `telegram:<chat>:topic:<thread>` whenever a message has a
  `message_thread_id`, which in a regular supergroup also happens for
  reply threads, splitting one group into many sessions. nanobot-go does
  it only for topic messages (`is_topic_message`), and only then sends
  replies with `message_thread_id`.
- **Typing indicator, no reaction.** Both show "typing…" every 4 seconds
  until the reply; nanobot-go also stops it after 10 minutes. nanobot's
  👀 reaction on received messages (`reactEmoji`) is not ported.
- **Retries.** `sendMessage` waits out 429 `retry_after` up to three
  attempts (nanobot: `_SEND_MAX_RETRIES`, also on timeouts); 400 and 403
  (chat not found, bot blocked) are `Permanent`, so the manager does not
  retry them. A failed `getUpdates` backs off 1s to 60s, or waits
  `retry_after`; nanobot restarts the whole application after a stall and
  backs off 5s to 300s on startup errors. A token rejected at startup
  (401/404 from `getMe`) fails the channel (logged, the gateway runs on),
  like nanobot's `InvalidToken`; when Telegram cannot be reached at
  startup, `Start` still succeeds and `getMe` is retried in the
  background with the polling backoff before polling begins (a later
  rejection stops the channel with an error in the log). There is no
  stall watchdog: each `getUpdates` has a deadline of `pollTimeout` plus
  15 seconds instead.
- **Token handling.** The token is validated as `<digits>:<secret>` before
  it is used in a URL, never logged, and replaced with `<redacted>` in
  every error (including `*url.Error`, which quotes the URL, and error
  descriptions). Errors returned by the client keep only context errors
  in their chain. `NANOBOT_GO_TELEGRAM_TOKEN` is new; nanobot resolves
  `${VAR}` references in the config instead.
- **Pairing.** The store keeps nanobot's `pairing.json` layout
  (`approved` per channel, `pending` by code with Unix-second times,
  10-minute codes like `ABCD-EFGH`), next to the config file instead of
  in `~/.nanobot`. Differences: approvals are stored by numeric user ID
  rather than `id|username` (so a username change keeps access; old
  `id|username` entries still match); codes match case-insensitively and
  without the dash; a corrupt file is an error and is never overwritten
  (nanobot resets it, losing approvals); updates take a file lock, so the
  gateway and the CLI can use it at once. Approval is through `nanobot
  pairing approve` on the host; nanobot's in-chat `/pairing` command (any
  allowed user could approve) and WebUI page are not ported, nor is
  `delivery_status` bookkeeping. The reply text points at the CLI.
- **Config.** Ported keys: `enabled`, `token`, `allowFrom`, `proxy`,
  `groupPolicy`, `replyToMessage`. New: `apiBase`, `pollTimeout`
  (seconds; PTB's default is 10, nanobot-go's 30). Not ported: `mode`,
  `webhook*`, `reactEmoji`, `connectionPoolSize`, `poolTimeout`,
  `streaming`, `inlineKeyboards`, `richMessages`, `streamEditInterval`;
  as everywhere, unknown keys are an error. Proxies are `http`, `https`
  or `socks5` (nanobot also takes `socks5h`).

### WeChat (weixin)

- **No QR code in the gateway.** nanobot's channel starts an interactive
  QR login when it has no token, from inside `start()`. A gateway is
  unattended, so nanobot-go's channel fails to start instead (logged, the
  gateway runs on) and points at `nanobot channels login weixin`. The
  login is a CLI command only; nanobot's WebUI connect flow
  (`connect.py`) is not ported.
- **The login is printed as a link.** nanobot draws the QR code with the
  `qrcode` package and falls back to printing the URL; nanobot-go always
  prints the link (with a `qrencode` example), since a QR encoder is not
  a small std-lib addition. The flow is nanobot's: `local_token_list`
  with the known tokens (empty with `--force`) and a retry without them on
  -3, `scaned_but_redirect` switching the polling host, `need_verifycode`
  prompting on stdin, `verify_code_blocked` and `expired` replacing the
  code up to three times, `binded_redirect` succeeding only without
  `--force` and with a local token (nanobot-go also counts a configured
  token). It also prints "Scanned" once for the `scaned` status.
- **Token only in account.json.** nanobot also writes the QR token and
  base URL back into `config.json` (`_persist_connect_credentials`, for
  the WebUI). nanobot-go keeps it only in `account.json`, under
  `state/weixin` next to the config file (nanobot: `~/.nanobot/weixin`),
  in nanobot's layout (`token`, `get_updates_buf`, `context_tokens`,
  `base_url`, `replaced_config_token_sha256`, plus `ilink_bot_id` and
  `ilink_user_id`). The file is written atomically with mode 0600 under a
  file lock; a corrupt file is an error, not an empty state. The guard
  against overwriting a newer login (`_save_state`) is kept. A configured
  token also restores the cursor and context tokens saved for that same
  token (nanobot starts it with empty state). `typing_tickets` are not
  persisted, and are dropped from a file nanobot wrote.
- **Stale token.** nanobot stops the channel on -14 unless a replacement
  login is already saved. nanobot-go keeps the channel up: sends fail
  with a `Permanent` error, and the saved state is checked every 30
  seconds, so a `nanobot channels login weixin --force` takes effect
  without restarting the gateway.
- **Held replies are kept by the channel.** As in nanobot, a reply that
  has no context token, hits iLink's -2 or the local budget (8 messages
  per token, nanobot's `contextMessageBudget` default, not configurable)
  is held, up to three per user, and sent after the user's next message.
  nanobot raises as well (its manager retries, and its durable outbox owns
  scheduled results); nanobot-go's `Send` returns nil, since retrying
  within seconds cannot help. A held reply that then fails otherwise is
  dropped, like nanobot's.
- **Partial deliveries.** nanobot tracks the chunks of each delivery
  already sent so a retry skips them, with `client_id`s derived from the
  delivery. nanobot-go does the same keyed by chat and content for two
  minutes, since bus messages carry no delivery ID.
- **Typing.** "typing…" starts when a message arrives (ticket from
  `getconfig`, cached for a random part of a day with backoff on failure,
  as nanobot), is refreshed every 5 seconds and cancelled after the reply,
  or after 10 minutes. nanobot also restarts it around each send; that
  extra `sendtyping` traffic is skipped.
- **Text only.** Inbound images, files, video and voice without WeChat's
  transcript become placeholders (`[image]`, `[file: name]`, ...), the
  same text nanobot produces when a download fails; nothing is
  downloaded or decrypted. Outbound media are not sent (logged). Progress
  messages and tool hints are dropped (the typing indicator keeps
  running); structured progress items, block streaming and run IDs are
  not ported.
- **Errors.** Business errors (non-zero `ret` or `errcode`) and 4xx other
  than 408/425/429 are `Permanent`; network errors, 5xx, 408/425/429 and
  invalid JSON are retried by the manager. A `getupdates` timeout polls
  again at once; other failures wait 2s, and 30s after three in a row
  (nanobot's `monitor.ts` constants). Like nanobot, the client ignores
  the proxy environment. The token goes only in the `Authorization`
  header and is replaced with `<redacted>` in errors and error messages.
- **Config.** Ported keys: `enabled`, `token`, `allowFrom`, `baseUrl`,
  `routeTag` (string or number), `stateDir`, `pollTimeout`. New:
  `NANOBOT_GO_WEIXIN_TOKEN`. Not ported (an error, like every unknown
  key): `cdnBaseUrl` (no media), `sendProgress`, `sendToolHints`,
  `replyProgressMessages`, `replyProgressMaxMessages`,
  `contextMessageBudget`, `streaming`, `blockStreaming*`.
- **Not ported: WeCom.** `nanobot/channels/wecom` (enterprise WeChat over
  the WeCom WebSocket SDK) is a different channel and remains a gap.

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

### Exec sandbox

- **Fails closed everywhere.** nanobot logs a warning on Windows and runs
  the command unsandboxed. nanobot-go checks the backend when the exec
  tool is built: an unknown name (already a config error), a backend for
  another OS (bwrap off Linux, Seatbelt off macOS, either on Windows), a
  missing binary, or a probe run that fails (e.g. bwrap without
  unprivileged user namespaces) is an error, and `agent`, `gateway` and
  `serve` refuse to start. A command is never run unsandboxed in its
  place; if the sandbox cannot start a command later, the model gets a
  tool error. To run without a sandbox, set `tools.exec.sandbox` to `""`.
- **Host OS checked at startup, not at load.** Config validation only
  checks the name, so a config for another machine still parses; the
  host check happens when the exec tool is built, and `status` reports it.
- **Bind paths must be absolute.** nanobot silently skips relative
  entries; nanobot-go rejects them (after `~` and `$VAR` expansion) when
  the config loads. Paths equal to or containing the workspace are
  ignored in both, also when they only match by file identity (e.g. a
  differently-cased path on a case-insensitive volume).
- **Workspace placement.** bwrap refuses a workspace directly under `/`
  (a tmpfs over `/` would hide the system mounts; nanobot would produce a
  broken command) and both backends refuse `/` itself.
- **bwrap is resolved once.** NewExec resolves `bwrap` through PATH and
  then runs that absolute path; Seatbelt always uses
  `/usr/bin/sandbox-exec` as in nanobot.
- **`TMPDIR=/tmp` under bwrap.** nanobot leaves the host `TMPDIR`, which
  usually does not exist inside the sandbox; nanobot-go points it at the
  sandbox's private `/tmp`. Seatbelt sets `HOME` and `TMPDIR` to the
  workspace, as in nanobot.

### SSRF

- **Pinning per dial, not by patching the resolver.** nanobot pins DNS by
  swapping the process-global `socket.getaddrinfo` under a lock for the
  duration of each request. Go has no such hook, and does not need one:
  the transport's `DialContext` resolves the host through the policy and
  connects only to the validated addresses. A context pin set
  (`WithPinnedDNS`) makes the pre-flight check, every redirect hop and
  every dial of a `web_fetch` share one lookup per host, so a rebinding
  answer is never even seen; requests are not serialized. A
  `net.Dialer.Control` check on the dialled address stays as defense in
  depth.
- **No global whitelist.** nanobot's `configure_ssrf_whitelist` sets
  module state at config load; nanobot-go passes a `*security.Policy` to
  the tools that need it.
- **Invalid whitelist entries are config errors.** nanobot silently skips
  them. A bare address is accepted as a single host, and host bits are
  ignored (`10.1.2.3/8` is `10.0.0.0/8`), as with nanobot's
  `strict=False`.
- **Wider blocked set.** `IsBlockedIP` also blocks multicast,
  `192.0.0.0/24`, `198.18.0.0/15` (benchmarking), `240.0.0.0/4` and NAT64
  `64:ff9b::/96`, which nanobot's list does not.
- **Redirects fail closed.** nanobot's `validate_resolved_url` lets a
  redirect through when its host does not resolve; nanobot-go refuses
  it (the dial would fail anyway, but the error is clearer).
- **`trust_remote_dns` is not ported.** nanobot uses it only for image
  generation behind a trusted proxy that resolves names itself. nanobot-go
  has no such caller, and a direct pinned dial cannot connect to a name
  it could not resolve, so there is no `ssrfTrustRemoteDNS` setting.
- **`allow_loopback` is not ported.** It has no caller here; whitelist
  `127.0.0.1/32` (or `::1/128`) instead. Tests use the whitelist too, so
  they exercise the production path.
- **`web_fetch` ignores environment proxies**, so the checked
  destination is the peer actually dialled. MCP clients honour them
  (see MCP above).
- **Shell guard unchanged.** The exec guard still refuses commands
  containing `localhost` or a literal internal IP URL without resolving
  names, and does not consult `tools.ssrfWhitelist` (nanobot's
  `contains_internal_url` resolves and honours the whitelist).

### OpenAI-compatible API

- **Exactly one user message (same as nanobot).** Like nanobot, each
  request must contain exactly one user message, since the server keeps
  the history: any other `messages` (none, several, or a lone system or
  assistant message) is a 400 "Only a single user message is supported".
  Unlike nanobot, an empty or whitespace-only message is a 400 too
  (nanobot runs it).
- **Text only.** nanobot saves base64 `image_url` parts and multipart
  uploads (images, PDF, Office, text; 10 MB per file) under its media
  directory and passes them to the turn. The Go loop has no media input
  yet, so image/audio/file parts and `multipart/form-data` are a 400, any
  other Content-Type than JSON too, and the body cap is 4 MiB instead of
  20 MiB. Remote image URLs are a 400 in both.
- **Streaming sends the whole reply as one chunk.** nanobot forwards
  token deltas as the model produces them. nanobot-go's providers do not
  stream, so the reply is one `chat.completion.chunk` (with
  `role: "assistant"`) when the turn is done, followed by the stop chunk
  and `[DONE]`; `: keep-alive` SSE comments every 15 seconds keep proxies
  from giving up meanwhile. As in nanobot, headers go out at once and a
  failure ends the stream without the stop chunk and `[DONE]`.
- **The timeout includes the session wait.** nanobot starts
  `api.timeout` after taking the session lock. nanobot-go submits the turn
  to the loop's per-session queue under the request's deadline, so time
  spent behind earlier turns of the same session counts.
- **Disconnecting cancels the turn.** nanobot only cancels a streaming
  turn when a write fails; a non-streaming one runs on. nanobot-go runs
  every turn under the request's context, so a client that goes away
  cancels it in both modes (a canceled turn is not saved). A turn
  canceled by shutdown answers 503 (new). 504 uses type `server_error`
  (nanobot: `invalid_request_error`).
- **Inside the gateway, with `api.enabled` (new).** nanobot runs the API
  only as `nanobot serve` (or a WebUI-managed `serve` process, whose
  process control in `nanobot/api/runtime.py` is not ported). The
  gateway's API shares its loop, sessions and MCP servers, and its
  requests get `gateway.shutdownTimeout` on shutdown like the channels'
  turns (`serve` uses the same setting).
- **Remote hosts are checked at startup only.** nanobot also fails config
  validation for `0.0.0.0`/`::` without `api.apiKey`; nanobot-go checks
  any non-loopback host when the API starts, so `nanobot agent` still runs
  with such a config.
- **Small additions.** Unknown routes and wrong methods answer 404/405 in
  the error JSON; 401s carry `WWW-Authenticate: Bearer`; `session_id`
  is at most 256 characters without control characters; the key is
  compared as SHA-256 digests so its length does not leak through timing;
  `--timeout` takes whole seconds. Request bodies are never logged
  (nanobot logs the first 80 characters).
- **Scheduled jobs from the API.** As in nanobot, the `cron` tool works in
  API turns. A job is bound to channel `api`, which no channel serves, so
  when the gateway runs it the turn lands in the API session and its
  reply is dropped with a warning; `serve` adds a note saying so to the
  tool's result.

### WebUI and the websocket channel

- **Same address as the gateway.** nanobot's WebSocket channel runs its
  own server on port 8765 (`channels.websocket.host`/`port`) and serves
  the WebUI there; the gateway's 18790 is health only. nanobot-go mounts
  `/ws` and the page on the gateway's server through `channels.Mounter`
  (one listener, one `--host`/`--port`); the section has no `host`,
  `port` or `path` keys and rejects them.
- **Loopback only, no auth.** nanobot allows a non-loopback `host` when
  `token` or `tokenIssueSecret` is set and lets same-machine clients in
  without one. nanobot-go has no tokens yet, so an enabled channel on a
  non-loopback gateway fails the startup. It also rejects requests whose
  `Host` header is not a loopback name (DNS rebinding) and upgrades whose
  `Origin` names another host (cross-site WebSocket hijacking); nanobot's
  `allowFrom` for the channel is not ported (every browser is sender
  `webui`).
- **Own, small protocol.** nanobot's envelope (topics, `chat_id`
  attach/new, streaming deltas, progress and tool events, media, control
  commands, session lists) is not used. Frames are `hello`, `history`,
  `chat` (or plain text), `stop`, `progress`, `delta`, `message` and
  `error`; see README. A `delta` frame is only text to append; there is
  no stream-end event, the `message` frame with the whole reply ends the
  stream.
- **Progress is one string per tool call.** nanobot's progress hook
  sends `ProgressEvent`s (content plus `tool_hint`), tool-event payloads
  with arguments and results, reasoning bubbles and streamed deltas, and
  formats all calls of an iteration as one hint (`read a × 2, $ ls`).
  nanobot-go publishes one plain hint per call as it starts (from
  `OnToolStart`, so parallel read-only calls each send one), nothing on
  tool end, no reasoning; assistant text, between tool calls too, comes
  only as `delta` frames. The hint covers nanobot's common formats (`read`,
  `write`, `edit`, `ls`, `$ cmd`, `fetch`, `search`, `mcp server::tool`,
  `name("arg")`) without home-directory folding or path abbreviation
  inside commands. Progress is a marked `OutboundMessage`
  (`Metadata["_progress"]`), not a separate event type: channels without
  a status display drop it, the manager never retries it or warns about
  it, and only bus-driven turns publish it (API and cron turns, which
  deliver their own replies, do not).
- **Stop.** nanobot's `/stop` cancels the session's active tasks and
  subagents and reports how many it stopped. nanobot-go cancels the one
  turn in flight (Serve handles `/stop` before queuing it), answers it
  "Stopped.", and lets queued messages run; with nothing in flight the
  reply is "Nothing to stop.". Through `Loop.Process` (API) `/stop`
  waits behind the session's turn, so it does not stop it there.
- **Chat ids.** nanobot clients pick topics and attach to chats by id;
  nanobot-go gives each connection a random id (`crypto/rand.Text`) or
  the `?chat_id=` it asks for, and the page stores it in
  `localStorage`.
- **Chat list over HTTP, not the socket.** nanobot's WebUI reads
  `GET /api/sessions` (every channel's sessions, with previews) and
  lists them in its ChatList. nanobot-go's channel serves
  `GET /ui/chats` next to the page instead of adding a WebSocket frame
  pair: it needs no open socket, and no other channel sees a new frame
  type. It lists only `websocket:*` sessions (at most 50), loads each
  for its title and preview rather than storing them, and leaves out
  chats with no user or assistant text yet.
- **Rename and delete over HTTP, through the Manager.** nanobot renames
  through its session metadata (`title`, `title_user_edited`) and
  deletes with a `session.delete` mutation. nanobot-go keeps the same
  metadata keys but serves `PATCH` and `DELETE /ui/chats/{chat_id}` next
  to the list, taking only chat ids (so only `websocket:*` keys) and
  refusing cross-origin requests (`http.CrossOriginProtection`) besides
  the loopback gate. An empty title clears the custom one; titles are
  never generated. The channel gets the agent loop's `session.Manager`,
  not its store: renames load a fresh copy and save it with
  `Manager.Save`, which replaces the cached session, and deletes go
  through `Manager.Delete`, which drops it, so the next turn neither
  saves over a new title nor brings a deleted chat back from the cache.
  A rename or delete racing a turn of the same chat is still unsafe (the
  turn's save fails on a SQLite generation conflict, or with JSONL
  overwrites the title or recreates the chat), so the page refuses both
  while its current chat's reply is running; another tab's running turn
  is not detected.
- **Status and a settings whitelist.** nanobot's WebUI settings read
  and write the whole config (models, providers and their keys,
  channels, MCP, OAuth) and can restart the gateway. nanobot-go serves a
  read-only overview at `GET /ui/status`, with the intent of
  `nanobot status`: names and
  flags, never key material, tokens, allow lists, environment values or
  absolute paths (the workspace is its base name). The handler lives on
  the websocket channel next to `/ui/chats`, so it shares the loopback
  gate and exists only while the WebUI is mounted; the channel gets a
  small `Status` DTO callback from the gateway rather than importing
  the config package. The config part is a snapshot taken at startup;
  uptime and running channels are read per request.
  `/ui/settings` edits only six kinds of field and never sees a secret:
  the channel decodes the body against an exact key schema (no
  case-insensitive matching, no nulls, secret-looking names refused at
  any depth) into a `SettingsPatch` DTO, and the gateway applies it to a
  config freshly loaded from disk, never to the running config, which
  has the environment's keys applied. Channel sections stay
  `json.RawMessage` and only their `enabled` key is replaced, so tokens
  and allow lists survive. Nothing is hot-applied: tools, provider and
  channels are built once at startup, so a saved change waits for a
  restart, and the page never restarts the gateway. Enabling a channel
  whose section is not in the file is refused, since it would lack its
  token, and so is another OS's sandbox, either of which would stop the
  gateway from starting.
- **History is pushed, not fetched.** nanobot's WebUI reads a chat's
  messages from the saved session. nanobot-go pushes them as one
  `history` frame after every `hello` and the page replaces its list, so
  a reply that raced the reconnect is not shown twice (in a narrow race
  it can still appear twice until the next reload). The frame carries the
  full transcript filtered to user/assistant text (not
  `Session.History`'s replay window, which starts after the
  consolidation marker), capped to the newest 200 messages and 256 KiB.
  It is read with `session.Manager.Load`, a fresh copy from the store,
  not the cached `*Session` that belongs to the agent loop's worker
  (`GetOrCreate`). Tool calls, progress
  and media are not replayed; a reconnect during a turn shows its
  progress again from the next tool call.
- **Undeliverable replies are dropped.** A reply for a chat with no open
  socket is a `channels.Permanent` error, so the manager logs it instead
  of retrying (a retrying reply would hold up every other chat's replies
  for 7 seconds). It is still in the session, so the next connect's
  `history` frame shows it.
- **Timeouts.** The shared server keeps `ReadHeaderTimeout` at 2 seconds
  but drops `ReadTimeout`/`WriteTimeout` while the WebUI is mounted (they
  would cut sockets after the upgrade), sets a 2-minute `IdleTimeout` and
  64 KiB of headers (localhost cookies); the channel pings every 30
  seconds, bounds writes to 10 seconds and frames to 64 KiB.
- **Committed build, no Node at Go time.** The page is a lean React app
  of its own in `webui/` (not a copy of nanobot's), built by Vite into
  `internal/webui/static/` and committed, so `go build`/`go test` need
  no npm; run `cd webui && npm install && npm run build` after changing
  it. The build has no inline scripts or styles and is served with the
  same strict CSP (`default-src 'self'`). The gateway never proxies a
  Vite dev server (`npm run dev` proxies to the gateway instead, at
  `$NANOBOT_GO_GATEWAY_URL` or 127.0.0.1:18790). `nanobot webui` is a
  thin launcher over `nanobot gateway`, owning the gateway child it
  starts rather than a shared background instance.
