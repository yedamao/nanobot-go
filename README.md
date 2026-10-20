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
  `web_fetch`.
- Persistent sessions (SQLite, one database per workspace, conflict
  detection between processes), `/new`, `/compact` and `/help`.
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
  endpoint and graceful shutdown. The channel interface, registry and
  outbound dispatcher are in place; no actual channel is implemented yet.
- Scheduled jobs (phase 3, part 2): the agent's `cron` tool schedules
  reminders and recurring tasks (once, every N seconds, or a cron
  expression in a time zone) bound to the chat they were created in; the
  gateway runs them and delivers the replies there. The gateway also runs
  Dream every two hours.
- CLI: `onboard`, `agent` (interactive or one-shot), `gateway`, `cron`
  (`list`, `remove`, `run`), `dream`, `status` (`--mcp` to probe MCP
  servers), `version`.

Not yet: chat channels (Telegram first, then others), heartbeat
(`HEARTBEAT.md`), the OpenAI-compatible API, process sandboxing. See
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
# NANOBOT_GO_MODEL, NANOBOT_GO_PROVIDER, NANOBOT_GO_WORKSPACE
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
    "exec": {"enable": true, "timeout": 60, "denyPatterns": [], "allowPatterns": [], "allowedEnvKeys": []},
    "web": {"enable": true, "fetchMaxBytes": 2097152},
    "mcpServers": {}
  },
  "channels": {},
  "gateway": {"host": "127.0.0.1", "port": 18790, "shutdownTimeout": 30}
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
nanobot-go does not know is an error at startup. No channel is
implemented yet (Telegram is next), so for now the gateway runs with
none.

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
enabled (never their settings, which hold tokens).

A systemd unit, for example:

```ini
[Service]
ExecStart=/usr/local/bin/nanobot gateway
Restart=on-failure
TimeoutStopSec=45
```

## Architecture

```
   cmd/nanobot (onboard | agent | gateway | cron | dream | status | version)
                 |                       |
           internal/config         stdin / stdout (agent)
                 |                internal/cron Service (gateway): due job ->
                 |                Loop.ProcessMessage in the job's chat -> bus
     (JSON, env, provider factory)       |
                 |                       v                 internal/channels
                 |      +--------------------------------+  Manager: start, route
                 |      |  agent.Loop                    |<-> outbound by name,
                 |      |                                |   retry, stop
                 |      |                                |  (over internal/bus)
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
service and the health endpoint around it. Tools learn which chat a turn
belongs to from its context (`tools.TurnFrom`), which is how the `cron`
tool binds a job to its chat.

## Safety notes

nanobot-go runs commands and edits files on your machine on the model's
behalf. Treat it like giving someone a shell.

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
  get around it. Use `tools.exec.enable: false` or run nanobot-go in a
  container or VM if that matters to you. Process sandboxing is planned.
- `web_fetch` refuses loopback, private, link-local and cloud-metadata
  addresses, checked at connect time so DNS rebinding and redirects are
  covered. Fetched content is marked as untrusted data for the model.
- The config file and session database are written with mode `0600`
  (its directory `0700`).
  `nanobot status` reports which keys are set, never their values.
- MCP servers are code you chose to run, with your privileges; nanobot-go
  does not sandbox them. Stdio servers get a minimal environment (`HOME`,
  `PATH`, `SHELL`, `TERM`, `USER`, locale and temp variables) plus their
  `env`, never the agent's API keys. `nanobot status` shows MCP `env` and
  `headers` names but never their values, counts `args` instead of
  printing them, and shows only the scheme and host of URLs; connection
  errors have configured secrets replaced with `[redacted]`. MCP URLs are
  trusted as configured (localhost servers work); they are not subject to
  `web_fetch`'s private-address checks. Tool results from MCP servers go
  to the model like any other tool output.
- The gateway's health endpoint has no authentication and listens on
  `127.0.0.1` unless `gateway.host` says otherwise. It reveals only that
  the process is up. Channel sections may hold bot tokens; `status` never
  prints them.
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
