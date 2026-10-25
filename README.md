# nanobot-go

A Go rewrite of [nanobot](https://github.com/HKUDS/nanobot), a small,
always-on personal AI agent. It talks to Anthropic or any OpenAI-compatible
API, keeps persistent conversations, can read and edit files, run shell
commands and fetch web pages inside a workspace directory, and can use the
tools of any [MCP](https://modelcontextprotocol.io) server.

It is a single static binary. Its only dependencies outside the Go
standard library are a pure-Go SQLite driver and the official MCP Go SDK.

## Status

**Phase 1 (core): usable from the command line.** Working today:

- Providers: Anthropic Messages API and OpenAI-compatible chat completions
  (OpenAI, DeepSeek, OpenRouter presets), with retry/backoff and fallback
  models behind a circuit breaker.
- Agent: model/tool loop with an iteration cap, concurrent read-only tools,
  and recovery from truncated or empty responses.
- Tools: `read_file`, `write_file`, `edit_file`, `list_dir`, `exec`,
  `web_fetch`. `exec` can run commands in an OS sandbox (phase 4):
  bubblewrap on Linux, Seatbelt on macOS.
- Persistent sessions (SQLite, one database per workspace, conflict
  detection between processes), `/new`, `/compact` and `/help`; in chat
  channels `/stop` stops the reply in progress.
- History consolidation (phase 2, part 1): when a conversation outgrows
  `maxHistoryMessages` or the context window, its oldest turns are
  summarized into `memory/history.jsonl` instead of being dropped.
- Long-term memory (phase 2, part 2): `nanobot dream` folds those
  summaries into `SOUL.md`, `USER.md`, `memory/MEMORY.md` and `skills/`,
  commits the change to git and trims the archive.
- MCP client (phase 2, part 3): tools of stdio, streamable HTTP and SSE
  servers appear as `mcp_<server>_<tool>`.
- Gateway (phase 3, part 1): `nanobot gateway` runs the agent as a
  long-running process behind the enabled chat channels, with a health
  endpoint and graceful shutdown.
- Scheduled jobs (phase 3, part 2): the agent's `cron` tool schedules
  reminders and recurring tasks (once, every N seconds, or a cron
  expression in a time zone) bound to the chat they were created in; the
  gateway runs them and delivers the replies there. The gateway also runs
  Dream every two hours.
- OpenAI-compatible API (phase 3, part 3): `nanobot serve` (or the
  gateway with `api.enabled`) answers `POST /v1/chat/completions` with a
  full agent turn, tools included, in a server-side session; SSE
  streaming, optional bearer token.
- Telegram (phase 3, part 4): the first chat channel. The gateway
  long-polls the Bot API; senders are allowed by `allowFrom` or paired
  with a one-time code approved through `nanobot pairing approve`.
- WeChat (phase 3, part 5): personal WeChat through Tencent's iLink bot
  API. `nanobot channels login weixin` logs in with a QR code; the
  gateway long-polls for messages and replies as text, with the same
  `allowFrom`/pairing access control as Telegram.
- CLI: `onboard`, `agent` (interactive or one-shot), `gateway`, `serve`,
  `cron` (`list`, `remove`, `run`), `pairing` (`list`, `approve`, `deny`,
  `revoke`), `channels login weixin`, `dream`, `status` (`--mcp` to probe
  MCP servers), `version`.

- WebUI (phase 4, first slice): with `channels.websocket.enabled`, the
  gateway serves a plain browser chat page and a WebSocket on its own
  loopback address; each chat runs a full agent turn, and a reload shows
  the chat's earlier messages again. The page shows the tool being run
  while a turn is in progress and can stop it, and a read-only status
  panel (version, model, provider, channels).

Not yet: other chat channels (Slack, Discord, WeCom, ...), heartbeat
(`HEARTBEAT.md`), the full WebUI (settings, apps, automations, ...). See
[docs/PORTING.md](docs/PORTING.md) for the roadmap.

## Quickstart

Requires Go 1.26+.

```sh
go build -o bin/nanobot ./cmd/nanobot

bin/nanobot onboard          # writes ~/.nanobot-go/config.json and the workspace
$EDITOR ~/.nanobot-go/config.json   # add an API key (or use an env var, below)
bin/nanobot status           # check what will be used (keys are never printed)

bin/nanobot agent -m "What files are in my workspace?"   # one-shot
bin/nanobot agent                                        # interactive
```

In interactive mode, Ctrl-C cancels the current reply (or exits when idle),
and `exit`, `quit` or Ctrl-D quit. `/new` starts a fresh conversation;
`/compact` summarizes the conversation so far and continues from the
summary.
Use `-s <key>` to keep several separate conversations (default `cli:direct`).

API keys can also come from the environment, which overrides the file:

```sh
export NANOBOT_GO_ANTHROPIC_API_KEY=sk-ant-...
# also NANOBOT_GO_{OPENAI,DEEPSEEK,OPENROUTER}_API_KEY, NANOBOT_GO_<P>_API_BASE,
# NANOBOT_GO_MODEL, NANOBOT_GO_PROVIDER, NANOBOT_GO_WORKSPACE,
# NANOBOT_GO_API_KEY (the bearer token of the OpenAI-compatible API),
# NANOBOT_GO_TELEGRAM_TOKEN (the Telegram bot token),
# NANOBOT_GO_WEIXIN_TOKEN (a WeChat bot token, instead of the QR login)
```

nanobot-go deliberately does not read `ANTHROPIC_API_KEY` or
`OPENAI_API_KEY`, so it never picks up credentials meant for other tools.

## Configuration

`~/.nanobot-go/config.json` (override with `--config` or `NANOBOT_GO_CONFIG`).
Missing fields keep their defaults; unknown keys are an error.

```json
{
  "agents": {
    "defaults": {
      "workspace": "~/.nanobot-go/workspace",
      "model": "claude-opus-4-5",
      "provider": "auto",
      "maxTokens": 8192,
      "temperature": 0.1,
      "maxToolIterations": 200,
      "maxHistoryMessages": 100,
      "contextWindowTokens": 200000,
      "consolidation": {"enable": true, "summaryMaxTokens": 2048},
      "dream": {"batchEntries": 20, "maxHistoryEntries": 1000, "intervalH": 2, "cron": ""},
      "fallbackModels": [
        {"provider": "openrouter", "model": "anthropic/claude-sonnet-4-5"}
      ]
    }
  },
  "providers": {
    "anthropic":  {"apiKey": "sk-ant-...", "apiBase": ""},
    "openai":     {"apiKey": "", "apiBase": ""},
    "deepseek":   {"apiKey": "", "apiBase": ""},
    "openrouter": {"apiKey": "", "apiBase": ""}
  },
  "tools": {
    "restrictToWorkspace": true,
    "exec": {"enable": true, "timeout": 60, "denyPatterns": [], "allowPatterns": [], "allowedEnvKeys": [],
             "sandbox": "", "sandboxRoBinds": [], "sandboxRwBinds": []},
    "web": {"enable": true, "fetchMaxBytes": 2097152},
    "ssrfWhitelist": [],
    "mcpServers": {}
  },
  "channels": {},
  "gateway": {"host": "127.0.0.1", "port": 18790, "shutdownTimeout": 30},
  "api": {"enabled": false, "host": "127.0.0.1", "port": 8900, "timeout": 120, "apiKey": ""}
}
```

With `"provider": "auto"` the provider is chosen from the model name: a
`<provider>/` prefix (`deepseek/deepseek-chat`), then a known model family
(`claude*` uses anthropic, `gpt-*`/`o1`/`o3`/`o4` use openai, `deepseek*`
uses deepseek), then a gateway (openrouter), each only if it has an API key.
`apiBase` points a preset at a proxy or any other OpenAI-compatible server.

The workspace may contain `AGENTS.md` (instructions), `SOUL.md`
(personality), `USER.md` (about you) and `memory/MEMORY.md` (long-term
memory); they are added to the system prompt, except `AGENTS.md`,
`USER.md` and `MEMORY.md` while they are still the untouched templates
`nanobot onboard` creates. Sessions are stored in a SQLite database,
`~/.nanobot-go/sessions/<workspace id>/sessions.sqlite3` (the ID is a hash
of the workspace path; `nanobot status` prints the full path), next to the
config file and outside the workspace. Session files from older versions
(`~/.nanobot-go/sessions/*.jsonl`) are imported the first time the
database is opened and renamed to `*.jsonl.migrated`. If another process
saved the same session in the meantime, the turn fails instead of
overwriting it.

### History consolidation

Sessions keep their full transcript, but only part of it is replayed to the
model. Before each turn, if more than `maxHistoryMessages` unsummarized
messages would be replayed, or the estimated prompt (about four bytes per
token) plus `maxTokens` would not fit in `contextWindowTokens`, the oldest
complete turns are summarized by the model. The newest turns that fit in
half of each limit stay verbatim. The summary merges the previous one, is
appended to `<workspace>/memory/history.jsonl` as one
`{"cursor", "timestamp", "content", "session_key"}` line, and then replaces
the archived turns in the prompt (at the end of the system prompt). The
session records the boundary (`last_consolidated`) and the summary.

If summarization fails, the error is logged and that turn simply replays
the last `maxHistoryMessages` messages, as with
`"consolidation": {"enable": false}`; nothing is lost and the next turn
tries again. The shell guard refuses commands that write to
`memory/history.jsonl` or `memory/.dream_cursor`.

### Long-term memory (Dream)

`nanobot dream` turns the archived summaries into durable memory, like
nanobot's Dream job. Each run reads up to `dream.batchEntries` entries of
`memory/history.jsonl` newer than `memory/.dream_cursor` and gives them to
one agent turn (not saved as a session) whose only tools are `read_file`
and `list_dir` inside the workspace and `write_file`/`edit_file` limited to
`SOUL.md`, `USER.md`, `memory/MEMORY.md` and files under `skills/`. The
instructions can be replaced by writing `<workspace>/prompts/dream.md`.

- The cursor moves only if the turn finishes normally; a failed or
  interrupted run exits with status 1 and the same entries are retried
  next time.
- Then the changed memory files are committed with `git` as
  `dream: <timestamp>` (author `nanobot`), listing the changed files in
  the body. A workspace that is not in a repository gets one, with a
  `.gitignore` that versions only long-term memory; a workspace inside
  another repository is not committed to; without `git` on `PATH` the step
  is skipped. Only the memory paths are committed, never anything else you
  have staged. Use `git log`/`git revert` in the workspace to audit or undo
  Dream's edits.
- Finally `history.jsonl` is trimmed to the newest
  `dream.maxHistoryEntries` entries (0 disables this). Entries Dream has
  not processed yet are always kept.

`nanobot gateway` runs Dream on a schedule, as the protected cron job
`dream`: every `dream.intervalH` hours (default 2, like nanobot), or at
the times of `dream.cron` (a 5-field cron expression in the host's local
time zone) when that is set. `"intervalH": 0` with no `cron` turns the
schedule off. A pass with no new history entries does not call the
model. Without the gateway, run `nanobot dream` from your system's cron
or a timer instead.

Runs that overlap are refused, and history appends from a running agent
are safe while Dream trims the file.

### Scheduled jobs (cron)

The agent has a `cron` tool, so you can just ask: "remind me to stretch
every 90 minutes", "every weekday at 9:00 Berlin time, summarize my
calendar file", "at 18:30 tell me to leave". The tool takes the same
arguments as nanobot's:

| Argument | Meaning |
|----------|---------|
| `action` | `add`, `list` or `remove`. |
| `message` | What to do when the job fires (required for `add`). |
| `every_seconds` | Run every N seconds, counted from the previous run. |
| `cron_expr`, `tz` | Run at the times of a 5-field cron expression (`minute hour day month weekday`, with ranges, lists, steps, `jan`-`dec`, `sun`-`sat` and `@daily`-style macros) in the IANA zone `tz` (default: the gateway host's local zone). |
| `at` | Run once at an ISO date-time (`2026-02-12T10:30:00`, optionally with an offset; without one it is local time). The job is deleted after it runs. |
| `name` | A label (default: the start of the message). |
| `job_id` | The job to `remove`. |

`add` needs exactly one of `every_seconds`, `cron_expr` and `at`.

A job belongs to the chat it was created in. When it fires, the gateway
runs `message` as a turn in that chat's session (the model sees that it
is a scheduled job, and cannot schedule new jobs from it) and sends the
reply to the same chat, thread included. Jobs are stored in
`<workspace>/cron/jobs.json`; the gateway checks it at least every five
minutes, so jobs added by another process are picked up too. Jobs that
came due while the gateway was down are skipped, except one-time (`at`)
jobs, which run once at startup. If a run fails, the error is recorded
on the job and nothing is sent.

`nanobot agent` offers the tool as well, but only the gateway runs jobs.
A job created from the CLI runs in the `cli:direct` session; there is no
channel to send its reply to, so it only shows up in that session's
history.

Manage jobs from the shell:

```sh
nanobot cron list [--all]      # --all includes disabled jobs
nanobot cron remove <id>
nanobot cron run <id> [--force]
```

`list` and `remove` only read and write `jobs.json` and need no API key.
`run` runs the job's turn right away in this process and prints the reply
(it does not send it to the chat). The `dream` job cannot be removed;
change `dream.intervalH`/`dream.cron` instead.

### MCP servers

`tools.mcpServers` connects the agent to
[Model Context Protocol](https://modelcontextprotocol.io) servers. The
keys mirror nanobot's:

```json
{
  "tools": {
    "mcpServers": {
      "filesystem": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/me/notes"]
      },
      "github": {
        "url": "https://api.githubcopilot.com/mcp/",
        "headers": {"Authorization": "Bearer ${GITHUB_TOKEN}"},
        "enabledTools": ["search_repositories", "get_file_contents"],
        "toolTimeout": 60
      },
      "local": {
        "type": "stdio",
        "command": "/usr/local/bin/my-mcp-server",
        "env": {"API_KEY": "${MY_SERVER_KEY}"},
        "cwd": "~/my-server",
        "enabled": false
      }
    }
  }
}
```

| Key | Meaning |
|-----|---------|
| `type` | `stdio`, `streamableHttp` or `sse`. Optional: `command` means stdio, a `url` ending in `/sse` means SSE, any other `url` streamable HTTP. |
| `command`, `args`, `env`, `cwd` | Start a stdio server. Set exactly one of `command` and `url`. |
| `url`, `headers` | Reach an HTTP server; headers go with every request. |
| `toolTimeout` | Seconds before a tool call is abandoned (default 30). |
| `enabledTools` | Tools to expose, by their name on the server (`search_repositories`) or the agent's name (`mcp_github_search_repositories`). Default `["*"]`, all tools; `[]` exposes none. |
| `enabled` | `false` keeps the entry but does not connect (default `true`). |

`${VAR}` in `args`, `env`, `url` and `headers` is replaced with the
environment variable when connecting, so tokens can stay out of the file;
an unset variable fails that server only.

Each tool becomes `mcp_<server>_<tool>`: characters other than
`[A-Za-z0-9_-]` turn into `_`, and a name longer than 64 characters keeps
its start and ends in a short hash. The description starts with
`[MCP <server>]`. Tools that declare `readOnlyHint` may run alongside other
read-only tools. Text results come back as text; images, audio and binary
resources as a short placeholder such as `[image: image/png, 5120 bytes]`.

`nanobot agent` connects to every enabled server at startup, concurrently
and with a 30-second limit each. A server that fails is skipped; the rest
work. Interactive mode prints a summary to stderr, for example
`MCP: filesystem connected (11 tools), github failed: connect: ...`
(one-shot mode only with `--verbose`, which also shows the servers'
stderr). If a server dies mid-session, the next call to one of its tools
reconnects once and retries. `nanobot status` lists the configured
servers without connecting; `nanobot status --mcp` connects, reports
each server's state and tool count or error, and disconnects.
`nanobot dream` never uses MCP tools.

MCP resources and prompts are not exposed yet, nor is OAuth; use a header
with a token instead.

HTTP and SSE servers are subject to the same private-address block as
`web_fetch` (see below): a server on `localhost`, your LAN or a tailnet
needs its address in `tools.ssrfWhitelist`, e.g. `"127.0.0.1/32"` for
`http://127.0.0.1:8000/mcp`. Stdio servers are not affected.

### Network access (SSRF protection)

`web_fetch` and MCP HTTP/SSE clients refuse loopback, private (RFC 1918,
`fc00::/7`), link-local and cloud-metadata (`169.254.169.254`), CGNAT
(`100.64.0.0/10`), NAT64 and other reserved addresses. A host name is
resolved once per request and every address it resolves to must be
allowed (a name with both a public and a private address is refused);
the connection then goes only to those addresses, so a DNS answer that
changes in between (DNS rebinding) cannot redirect it. Every redirect is
checked the same way before it is followed, and only `http`/`https` are
allowed.

To reach a specific internal service, whitelist its address. Prefer a
single host (`/32` for IPv4, `/128` for IPv6) over whole ranges:

```json
{
  "tools": {
    "ssrfWhitelist": ["127.0.0.1/32", "100.101.102.103/32", "fd7a:115c:a1e0::1/128"]
  }
}
```

A bare address (`"192.168.1.20"`) means that host only. An entry that is
not an address or CIDR range is a config error. `web_fetch` connects
directly and ignores `HTTP(S)_PROXY`; MCP clients honour an environment
proxy, in which case the URL is checked locally and the proxy connects.

### Gateway

`nanobot gateway` is the always-on mode: it builds the agent exactly as
`nanobot agent` does (same config, workspace, sessions and MCP servers),
starts the chat channels enabled under `channels`, and answers their
messages until it is stopped. Each chat is its own session
(`<channel>:<chat id>`); chats run in parallel, messages within a chat in
order.

```json
{
  "channels": {
    "telegram": {"enabled": true, "...": "channel-specific settings"}
  },
  "gateway": {"host": "127.0.0.1", "port": 18790, "shutdownTimeout": 30}
}
```

Each key of `channels` is a channel name with an object holding at least
`"enabled"`; the channel itself checks the other keys. An enabled channel
nanobot-go does not know is an error at startup. The channels so far are
[Telegram](#telegram), [WeChat](#wechat) and `websocket`, the
[WebUI](#webui).

- **Health endpoint.** `GET http://<host>:<port>/healthz` (also
  `/health`, nanobot's path) returns `{"status":"ok"}` while the process
  is up. It listens on loopback by default; a non-loopback `host` prints
  a warning, since the endpoint is unauthenticated. `--host` and `--port`
  override the config; port 0 picks a free port. A port already in use
  fails at startup.
- **Delivery.** Replies are routed to the channel named in the message.
  A failed send is retried after 1, 2 and 4 seconds (four attempts in
  all) unless the channel reports a permanent error; replies for a channel
  that is not running are dropped with a warning. A channel that fails to
  start is logged and skipped; the others keep running.
- **Shutdown.** On SIGINT (Ctrl-C) or SIGTERM the gateway stops taking
  new messages and starting scheduled jobs, gives the turns in flight (a
  running job's included) up to `shutdownTimeout` seconds to finish (0
  cancels them at once), delivers their replies, then stops
  the channels, the health endpoint, the MCP servers and the session store
  in that order and exits with status 0. Sending the signal again exits
  immediately with status 1.

`nanobot status` shows the health address and which channels are
enabled (never their settings, which hold tokens; for Telegram it says
whether the token is set and where from, for WeChat whether it is logged
in).

A systemd unit, for example:

```ini
[Service]
ExecStart=/usr/local/bin/nanobot gateway
Restart=on-failure
TimeoutStopSec=45
```

### Telegram

Create a bot with [@BotFather](https://t.me/BotFather), then enable the
channel and run `nanobot gateway`:

```json
{
  "channels": {
    "telegram": {
      "enabled": true,
      "token": "123456:ABC-your-bot-token",
      "allowFrom": ["123456789", "your_username"],
      "groupPolicy": "mention",
      "replyToMessage": false,
      "pollTimeout": 30,
      "proxy": ""
    }
  }
}
```

Only `enabled` and the token are required. `NANOBOT_GO_TELEGRAM_TOKEN`
overrides `token`, so the token can stay out of the file. `apiBase`
(default `https://api.telegram.org`) points at another Bot API server.
`proxy` takes an `http://`, `https://` or `socks5://` URL (a bare
`host:port` means http); without it the usual `HTTPS_PROXY` variables
apply.

- **Who may talk to the bot.** `allowFrom` lists numeric user IDs and/or
  usernames (with or without `@`, any case); `"*"` allows everyone. With
  an empty list nobody is allowed until paired: a stranger who writes to
  the bot in a private chat gets a pairing code (valid 10 minutes, the
  same code if they write again), and the code is also logged by the
  gateway. Approve it on the host:

  ```sh
  nanobot pairing list            # pending codes and approved senders
  nanobot pairing approve ABCD-EFGH
  nanobot pairing deny ABCD-EFGH
  nanobot pairing revoke telegram 123456789
  ```

  Approvals are stored by user ID in `pairing.json` next to the config
  file and take effect at once, without restarting the gateway. Strangers
  in groups get no code; their messages are dropped with a log line.
- **Groups.** With `groupPolicy: "mention"` (default) the bot answers in
  groups only when it is @-mentioned, when someone replies to one of its
  messages, or for a `/command`; `"open"` answers every message from an
  allowed sender. In privacy mode (BotFather's default) Telegram delivers
  only some group messages to bots, such as commands and replies to
  them; turn it off with `/setprivacy` for `"open"` and @-mentions to
  work reliably.
- **Sessions.** Each chat is a session `telegram:<chat id>`; a forum topic
  is its own session `telegram:<chat id>:topic:<thread id>`, and replies
  go back to the topic. `/new`, `/compact` and `/help` work as in the CLI
  (`/new@your_bot` too); `/start` answers with a greeting; `/stop` stops
  the reply in progress (answered "Stopped."; the stopped turn is not
  saved).
- **Messages.** Text messages and captions are handled; a reply quotes
  the message it answers (`[Reply to bot: ...]`). Photos, voice and other
  media without a caption are ignored. Replies are sent as **plain
  text**, so Markdown from the model shows literally but can never break
  delivery; long replies are split at newlines or spaces into messages of
  at most 4096 characters. "typing…" shows while the agent works.
- **Reliability.** The bot token is checked with `getMe` at startup (a
  rejected token keeps Telegram from starting; the gateway runs on
  without it; if Telegram cannot be reached yet, the channel keeps
  trying in the background) and a webhook left over from another
  deployment is removed. Polling backs off from 1 second to a minute after errors and
  waits out Telegram's flood control (`retry_after`), which sending also
  honors.

### WeChat

The `weixin` channel connects a personal WeChat account through Tencent's
iLink bot API (`https://ilinkai.weixin.qq.com`), the same way nanobot's
does: HTTP long polling, no WeChat client on the host, and a bot token
obtained by scanning a QR code. (WeCom, the enterprise variant, is not
supported.)

Enable the channel, log in, then run the gateway:

```json
{
  "channels": {
    "weixin": {
      "enabled": true,
      "allowFrom": []
    }
  }
}
```

```sh
nanobot channels login weixin    # prints a QR code link; scan it with WeChat
nanobot gateway
```

The login prints the link of a QR code. nanobot-go does not draw QR codes
in the terminal; turn the link into one (for example
`qrencode -t ansiutf8 '<link>'`) and scan it with WeChat, then confirm on
the phone. If WeChat shows a number to type in, the command asks for it.
An expired code is replaced up to three times. The bot token and the API
host WeChat assigned to the account are saved to
`state/weixin/account.json` next to the config file (mode `0600`, so
`~/.nanobot-go/state/weixin/account.json` by default), never to
`config.json`. Running the login again does nothing while a login is
saved; `--force` logs in anew and keeps the saved login until the new one
is confirmed.

All settings are optional: `token` (a bot token to use instead of the
saved login; `NANOBOT_GO_WEIXIN_TOKEN` overrides it), `allowFrom`,
`baseUrl` (default `https://ilinkai.weixin.qq.com`), `routeTag` (sent as
the `SKRouteTag` header), `stateDir` (where `account.json` lives) and
`pollTimeout` (seconds, default 35; the server may suggest another). A
configured token wins over the saved login, unless a later QR login
replaced exactly that token.

- **Who may talk to the bot.** As for Telegram: `allowFrom` lists iLink
  user IDs (as the gateway logs them, `sender=...`) or `"*"`; with an
  empty list a stranger who writes in a private chat gets
  a pairing code to approve with `nanobot pairing approve <code>`
  (`nanobot pairing revoke weixin <user id>` undoes it). Group chats
  (`...@chatroom`) get no code.
- **Sessions.** Each user is a session `weixin:<user id>`.
- **Messages.** Text is handled, with quoted messages prefixed as
  `[引用: ...]` like nanobot; voice messages pass WeChat's own transcript
  (`[voice] ...`). Images, files, video and voice without a transcript
  arrive as placeholders (`[image]`, `[file: name]`, ...): media are not
  downloaded or sent. Replies go out as text, with Markdown WeChat
  renders badly (images, strikethrough, bare `<`) cleaned up, in messages
  of at most 1800 characters with code fences kept balanced. "typing…"
  shows while the agent works.
- **Replying needs a fresh message.** iLink accepts a reply only with the
  `context_token` of the user's latest message, which expires after a few
  minutes of inactivity and allows only a few messages. The channel keeps
  each user's latest token (also in `account.json`), refreshes it before
  sending once it is a minute old, and sends at most 8 messages per
  token. A reply it cannot send that way (a scheduled job for a user who
  has not written yet, or a long answer that used up the quota) is held,
  up to three per user, and delivered when the user writes again.
- **Reliability.** Without a token the channel does not start (the
  gateway runs on without it). Polling resumes where it left off after a
  restart, waits 2 seconds after an error and 30 seconds after three in a
  row. When WeChat reports the login expired, sending fails, the log says
  to run `nanobot channels login weixin --force`, and polling resumes by
  itself once that login is saved.

### WebUI

A minimal browser chat page (phase 4, first slice), served by the
`websocket` channel on the gateway's own address. Enable it and start the
gateway:

```json
{
  "channels": {
    "websocket": {"enabled": true}
  },
  "gateway": {"host": "127.0.0.1", "port": 18790}
}
```

```sh
bin/nanobot gateway
# nanobot-go gateway ...
# Health endpoint: http://127.0.0.1:18790/healthz
# WebUI: http://127.0.0.1:18790/
```

Open `http://127.0.0.1:18790/` (it redirects to `/ui/`), type a message
and press Enter. Each reply is a full agent turn, tools included.

- **One address.** The page (`GET /`, `GET /ui/`), the chat list
  (`GET /ui/chats`), the status (`GET /ui/status`) and the WebSocket (`GET /ws`) are mounted on the gateway's HTTP server next to
  `/healthz`; there is no second port. With the channel disabled (the
  default) those paths are 404 and the server is health-only, as before.
- **Sessions.** Each chat is the session `websocket:<chat id>`. The page
  keeps its chat id in the browser's `localStorage` and reconnects with
  it, so a reload continues the same session; **New chat** starts a new
  id (and an empty page). Two tabs on the same id both get the replies.
- **Chat list.** A sidebar (a **Chats** overlay on narrow screens) lists
  the stored WebUI chats, newest first: the first user message as the
  title, the last message as a preview, and when it was last updated.
  The current chat is highlighted; one with nothing stored yet shows as
  "New chat" on top until its first turn is saved. Clicking a chat
  reconnects with its id, so its history loads as on a reload. The list
  is `GET /ui/chats` (loopback only, like `/ws`), refetched after each
  `history` and `message` frame and when the tab becomes visible:
  `{"chats":[{"chat_id":"...","updated_at":"2026-10-25T12:00:00Z","title":"...","preview":"..."}]}`.
  It holds only `websocket:*` sessions, at most 50, with titles cut to
  60 and previews to 80 characters. Chats cannot be renamed, deleted or
  searched yet.
- **History on reload.** On every connect, after `hello`, the server
  sends a `history` frame with the chat's stored transcript and the page
  redraws from it, so a reload or reconnect shows the earlier messages
  without duplicates. It holds only user and assistant texts, in stored
  order: tool calls, tool results and system messages are left out, as
  are assistant messages with no text. It is the full transcript,
  including turns that consolidation has summarized for the model, cut
  to the newest 200 messages and 256 KiB of text. A message still being
  answered when the socket dropped appears once its turn is saved (on
  the next reload); its reply arrives as usual if the page is connected.
- **Status.** The header shows Connecting… / Connected / Disconnected
  (with the retry delay); sending is enabled once the history has
  arrived.
- **Status panel.** The header's **Status** button opens a read-only
  overview of the gateway, fetched from `GET /ui/status` (loopback only)
  when it opens and on **Refresh**:
  `{"version":"0.1.0-dev","go":"go1.26.0","started_at":"2026-10-25T09:00:00Z","uptime_seconds":3600,"model":"claude-opus-4-5","provider":"anthropic","workspace":"workspace","channels":[{"name":"websocket","enabled":true,"running":true}],"api_enabled":false,"tools":{"exec":true,"web_fetch":true}}`.
  `provider` is the resolved preset's name, `workspace` the directory's
  base name, and `channels` every configured channel with its `enabled`
  flag and whether it is running. Like `nanobot status` it never holds
  API keys, tokens, `allowFrom` lists, environment values or full paths.
  Nothing can be changed from the page: edit `config.json` and restart
  the gateway.
- **Progress.** While a reply is due the page shows a muted "nanobot is
  thinking…" bubble. As each tool call starts the server sends a
  `progress` frame with a short hint (`read …/notes/todo.md`,
  `$ go test ./...`, `fetch https://…`, `github::list_issues("a/b")`) and
  the bubble shows it; the reply, a disconnect or a reconnect's history
  clears it. Progress is never stored, so it is not in the history.
  Other chat channels (Telegram, WeChat) ignore progress; their "typing…"
  indicator keeps running instead.
- **Stop.** While a reply is due a **Stop** button sends a `stop` frame
  (typing `/stop` does the same): the turn in flight is canceled and
  answered "Stopped.", and it is not saved to the session. Messages sent
  after it still run.
- **Loopback only.** There is no browser login yet, so the gateway
  refuses to start with the channel enabled unless `gateway.host` (or
  `--host`) is a loopback address. The channel also refuses requests
  whose `Host` header is not `localhost`/`127.0.0.1`/`[::1]` (DNS
  rebinding) and WebSocket upgrades from another origin (other web
  pages). Anyone who can reach the port on this machine can chat with
  the agent, which can run commands. The page holds no secrets.
- **Timeouts.** With the WebUI mounted the server bounds only the request
  header read (2 seconds) and closes idle keep-alive connections after 2
  minutes, so a socket can stay open through long turns; the channel
  pings each socket every 30 seconds and bounds each write to 10 seconds.
  Health-only gateways keep the 2-second read and write timeouts.
- **Delivery.** A reply for a chat with no open socket (the tab was
  closed) is dropped with a log line, not retried; it is in the session,
  so the next connect's history shows it.

Wire protocol (JSON text frames; any other frame type gets an `error`):

| Direction | Frame |
|-----------|-------|
| server → client, on connect | `{"type":"hello","chat_id":"..."}` |
| server → client, right after `hello` | `{"type":"history","messages":[{"role":"user","content":"..."},{"role":"assistant","content":"..."}]}` (`[]` for a new chat) |
| client → server | `{"type":"chat","content":"..."}`, or plain text |
| client → server | `{"type":"stop"}` (same as sending `/stop`) |
| server → client, while a turn runs | `{"type":"progress","content":"..."}` (transient status; the next `message` supersedes it) |
| server → client | `{"type":"message","role":"assistant","content":"..."}` |
| server → client | `{"type":"error","content":"..."}` (bad frame) |

`ws://127.0.0.1:18790/ws?chat_id=<id>` joins an existing chat (1-64
letters, digits, `-`, `_`); without it the server picks a random id.

### OpenAI-compatible API

nanobot-go can put the agent behind a minimal OpenAI-compatible endpoint,
like nanobot's `nanobot serve`, so any OpenAI client library can talk to
it. Each request is a full agent turn (tools, MCP servers, memory), not a
bare model call.

```sh
nanobot serve                          # 127.0.0.1:8900
nanobot serve --port 9000 --timeout 300
```

`nanobot serve` runs only the API (no chat channels, no scheduled jobs).
To run it inside the gateway instead, set `"api": {"enabled": true}`;
the gateway then serves it next to the channels and the health endpoint.

```json
{
  "api": {"enabled": true, "host": "127.0.0.1", "port": 8900, "timeout": 120, "apiKey": ""}
}
```

| Key | Meaning |
|-----|---------|
| `enabled` | Also run the API inside `nanobot gateway` (default `false`). `nanobot serve` ignores it. |
| `host`, `port` | Where the API listens (default `127.0.0.1:8900`, nanobot's). `serve --host/--port` override them; port 0 picks a free port. |
| `timeout` | Seconds one request's turn may take (default 120; `serve --timeout`). |
| `apiKey` | When set, every route but `/health` requires `Authorization: Bearer <apiKey>`. `NANOBOT_GO_API_KEY` overrides it. |

Endpoints:

- `GET /health`: `{"status":"ok"}`, never authenticated.
- `GET /v1/models`: one model, `agents.defaults.model`.
- `POST /v1/chat/completions`: runs the **last** message (it must be
  `role: "user"`; text, or text content parts) as one turn and answers
  with a `chat.completion` including `usage` (tokens of every model
  request in the turn). `model` may be omitted or must equal the listed
  one. Other request fields (`temperature`, `tools`, ...) are ignored.

```sh
curl http://127.0.0.1:8900/v1/chat/completions \
  -H "Authorization: Bearer $NANOBOT_GO_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "hi"}], "session_id": "my-session"}'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8900/v1", api_key="local-no-key", max_retries=0)
model = client.models.list().data[0].id
resp = client.chat.completions.create(
    model=model,
    messages=[{"role": "user", "content": "hi"}],
    extra_body={"session_id": "my-session"},
)
print(resp.choices[0].message.content)
```

- **Sessions.** The server keeps the conversation: `"session_id": "x"`
  selects the session `api:x`, and without it every request shares
  `api:default`. As in nanobot, each request must contain exactly one
  user message: `messages` with any other message (a system prompt, the
  resent conversation) is a 400 "Only a single user message is
  supported". A session ID only separates history;
  every caller shares the workspace, memory and tools. Requests for one
  session run one at a time, in order; the timeout includes the wait.
  `/new`, `/compact` and `/help` work as messages.
- **Streaming.** `"stream": true` answers with Server-Sent Events:
  `chat.completion.chunk` events, then `data: [DONE]`. The agent does not
  stream tokens, so the whole reply arrives in one chunk when the turn is
  done; until then the server sends `: keep-alive` comments every 15
  seconds. A failed or timed-out turn ends the stream without the final
  chunk and `[DONE]`; treat a stream without `[DONE]` as incomplete.
- **Errors** are `{"error": {"message", "type", "code"}}`: 400 for an
  invalid request (bad JSON, not exactly one user message, empty text,
  images or file uploads, another model), 401 for a missing or wrong key,
  413 for a body
  over 4 MiB, 504 when the turn times out, 503 when it is canceled (the
  client disconnected or the server is shutting down), 500 when it
  fails. A 200 can still describe a failed tool action in its text.
- **Retries repeat actions.** There is no idempotency key; a retried
  request runs its turn (and its tool calls) again, so turn off automatic
  client retries (`max_retries=0` above). Disconnecting
  cancels the turn, but tool actions already done stay done.
- **Shutdown.** Like the gateway: on SIGINT/SIGTERM the API stops taking
  requests, gives those in flight `gateway.shutdownTimeout` seconds, then
  cancels them (503). A second signal exits at once.
- Text only: image parts and multipart file uploads are rejected.
  Scheduled jobs created through the API are bound to the `api` channel,
  which has no chat to deliver to; their replies only land in the
  session's history.

## Architecture

```
   cmd/nanobot (onboard | agent | gateway | serve | cron | pairing | channels | dream | status | version)
                 |                       |
           internal/config         stdin / stdout (agent)
                 |                internal/cron Service (gateway): due job ->
                 |                Loop.ProcessMessage in the job's chat -> bus
     (JSON, env, provider factory)       |
                 |                       v                 internal/channels
                 |      +--------------------------------+  Manager: start, route
                 |      |  agent.Loop                    |<-> outbound by name,
                 |      |                                |   retry, stop
                 |      |                                |  (over internal/bus);
                 |      |                                |  telegram: Bot API
                 |      |                                |  polling, allowFrom +
                 |      |                                |  internal/pairing;
                 |      |                                |  weixin: iLink polling,
                 |      |                                |  QR login state
                 |      |  - one worker per session key  |
                 +----->|  - ContextBuilder: system      |
                        |    prompt + history + message  |
                        |  - saves the session only      |
                        |    after a successful run      |
                        |  - memory.Consolidator:        |
                        |    old turns -> history.jsonl  |
                        +---------------+----------------+
                                        |            \
                                        v             v
                        +---------------------+   internal/session
                        |  agent.Runner       |   (SQLite, generations,
                        |  model <-> tools    |    history window)
                        |  loop, iteration cap|
                        +----+-----------+----+
                             |           |
                             v           v
                internal/providers   internal/tools (+ builtin)  <-- internal/mcp
                Anthropic, OpenAI-   registry, validation,        Manager (owned by
                compatible, retry,   fs / exec / web_fetch        cmd/nanobot): MCP
                FallbackProvider          |                       sessions, mcp_* tools
                                          v
                                  internal/security
                                  workspace containment,
                                  shell guard, SSRF checks
```

The split follows nanobot: the **Loop** owns the channel-facing turn
(session, context, delivery) and the **Runner** owns the model/tool loop.
MCP connections belong to the application, not the loop: the command
creates an `mcp.Manager` on the shared registry, connects it before the
first turn and closes it at exit. `agent` and `gateway` build all of this
with the same code; the gateway adds the bus, the channels, the cron
service and the health endpoint around it, and `internal/api` (in
`serve`, or the gateway with `api.enabled`) turns HTTP requests into
`Loop.Process` calls in `api:<session>` sessions. Tools learn which chat a turn
belongs to from its context (`tools.TurnFrom`), which is how the `cron`
tool binds a job to its chat.

## Safety notes

nanobot-go runs commands and edits files on your machine on the model's
behalf. Treat it like giving someone a shell.

- The [WebUI](#webui) has no login: it runs only on a loopback gateway
  and answers only loopback `Host` names and same-origin sockets, but any
  local user or process that can reach the port can drive the agent.

- `restrictToWorkspace` (default `true`) confines file tools to the
  workspace, including through symlinks and `..`, and makes the shell guard
  reject commands that reference paths outside it. File tools do their I/O
  through an `os.Root` opened on the workspace, so a symlink swapped in
  after a path was checked still cannot lead outside. They only read
  regular files (never FIFOs or devices).
- `tools.exec.denyPatterns` adds regular expressions to the built-in deny
  list. `allowPatterns`, when non-empty, is an allowlist: every command
  segment must match one, and an allowed command skips the deny list.
- Commands run with a minimal environment (`HOME`, `PATH`, `LANG`, `TERM`,
  `TMPDIR`, `USER`, ...), so provider API keys and other secrets in the
  agent's environment are not visible to them. List extra variables to pass
  through in `tools.exec.allowedEnvKeys`.
- The shell guard blocks well-known destructive commands (`rm -rf /`,
  `mkfs`, fork bombs, `shutdown`, ...) and requests to internal URLs. It is
  **best-effort pattern matching, not a sandbox**; a determined model can
  get around it. Turn on the process sandbox below, set
  `tools.exec.enable: false`, or run nanobot-go in a container or VM if
  that matters to you.
- `tools.exec.sandbox` runs every command in an OS sandbox after the shell
  guard has passed it: `"seatbelt"` on macOS (`/usr/bin/sandbox-exec`,
  built in) or `"bwrap"` on Linux (install bubblewrap, e.g.
  `apt install bubblewrap`). Inside, the workspace is read-write, system
  directories (`/usr`, `/bin`, certificates, ...) are read-only, and the
  workspace's parent directory, which holds the config, keys and
  sessions in the default layout (`~/.nanobot-go`), is hidden. `HOME`
  points at the workspace (on macOS `TMPDIR` too; under bwrap `/tmp` is a
  private tmpfs). Network access is not restricted. A sandbox also turns
  on `restrictToWorkspace` for the file tools.

  ```json
  "tools": {"exec": {"sandbox": "seatbelt",
                     "sandboxRoBinds": ["~/.local/bin", "/opt/homebrew"],
                     "sandboxRwBinds": []}}
  ```

  Tools installed outside the system directories (Homebrew,
  `~/.local/bin`, `~/.cargo/bin`, ...) are invisible until you list them in
  `sandboxRoBinds`; `sandboxRwBinds` adds writable roots for trusted
  caches. Both take absolute paths (`~` and `$VAR` are expanded); a path
  equal to or containing the workspace is ignored, since it would uncover
  the hidden parent. Absolute paths under these binds also pass the shell
  guard. The sandbox **fails closed**: if the backend is unknown, built
  for another OS, missing, or cannot start a sandboxed process (for
  example without unprivileged user namespaces), nanobot-go refuses to
  start instead of running commands unsandboxed. `nanobot status` shows
  whether the configured sandbox is usable.
- `web_fetch` and MCP HTTP/SSE clients refuse loopback, private,
  link-local, CGNAT and cloud-metadata addresses unless they are in
  `tools.ssrfWhitelist`. Each host is resolved once per request and only
  the validated addresses are dialled, so DNS rebinding cannot slip past
  the check; every redirect hop is re-checked before it is followed.
  Fetched content is marked as untrusted data for the model.
- The config file and session database are written with mode `0600`
  (its directory `0700`).
  `nanobot status` reports which keys are set, never their values.
- MCP servers are code you chose to run, with your privileges; nanobot-go
  does not sandbox them. Stdio servers get a minimal environment (`HOME`,
  `PATH`, `SHELL`, `TERM`, `USER`, locale and temp variables) plus their
  `env`, never the agent's API keys. `nanobot status` shows MCP `env` and
  `headers` names but never their values, counts `args` instead of
  printing them, and shows only the scheme and host of URLs; connection
  errors have configured secrets replaced with `[redacted]`. MCP HTTP URLs
  get the same private-address checks as `web_fetch`; whitelist a local
  server's address to use it. Tool results from MCP servers go
  to the model like any other tool output.
- The gateway's health endpoint has no authentication and listens on
  `127.0.0.1` unless `gateway.host` says otherwise. It reveals only that
  the process is up. Channel sections may hold bot tokens; `status` never
  prints them.
- The Telegram bot token is part of every Bot API URL. nanobot-go never
  logs request URLs and replaces the token with `<redacted>` in every
  error, so it does not show up in the gateway's log. Anyone allowed to
  talk to the bot (by `allowFrom`, `"*"` or an approved pairing) gets the
  agent's full tool set in your workspace; keep `allowFrom` narrow and
  approve only codes you expect. `pairing.json` is written with mode
  `0600`.
- The WeChat bot token is account access: it goes only into the
  `Authorization` header and `state/weixin/account.json` (mode `0600`, in
  a `0700` directory), never into `config.json`, the log, `status` or the
  login's output, and errors have it replaced with `<redacted>`. Like
  nanobot, the iLink client ignores `HTTPS_PROXY` and the other proxy
  variables.
- The OpenAI-compatible API gives every caller the agent's full tool set
  in the workspace, so treat its key like a shell login. It listens on
  `127.0.0.1` by default and refuses to start on any other host without
  `api.apiKey`. The key is compared in constant time and never printed or
  logged; nor are request bodies. Use a TLS-terminating reverse proxy if
  it must cross a network.
- Scheduled jobs run unattended with the agent's full tool set, as if
  their message had been sent in their chat. Anyone who can talk to the
  agent can schedule jobs (and list or remove anyone's), so only expose
  channels to people you trust. `jobs.json` is written with mode `0600`.
- A turn whose provider call fails or is canceled is not saved, so errors
  never poison the conversation history. A history summary is written to
  `memory/history.jsonl` (fsynced, refusing symlinks and special files)
  before the session's consolidation marker moves.
- Dream's write tools check the target after resolving symlinks, so a link
  planted at `SOUL.md` or `skills/` cannot redirect a write to
  `history.jsonl` or out of the workspace. Dream has no `exec` or
  `web_fetch`, and its prompt tells the model to treat the history as
  data. Its git commands run with a timeout, skip hooks and signing, and
  ignore `GIT_DIR`-style variables.

## Development

```sh
gofmt -l .
go build ./...
go vet ./...
go test -race ./...
```

Tests use fakes, `httptest` and in-memory MCP servers (the stdio test
re-runs the test binary as a server); they need no network access or API
keys.
