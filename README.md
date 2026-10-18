# nanobot-go

A Go rewrite of [nanobot](https://github.com/HKUDS/nanobot), a small,
always-on personal AI agent. It talks to Anthropic or any OpenAI-compatible
API, keeps persistent conversations, and can read and edit files, run shell
commands and fetch web pages inside a workspace directory.

It is a single static binary with no dependencies outside the Go standard
library.

## Status

**Phase 1 (core): usable from the command line.** Working today:

- Providers: Anthropic Messages API and OpenAI-compatible chat completions
  (OpenAI, DeepSeek, OpenRouter presets), with retry/backoff and fallback
  models behind a circuit breaker.
- Agent: model/tool loop with an iteration cap, concurrent read-only tools,
  and recovery from truncated or empty responses.
- Tools: `read_file`, `write_file`, `edit_file`, `list_dir`, `exec`,
  `web_fetch`.
- Persistent sessions (JSONL, atomic writes), `/new` and `/help`.
- CLI: `onboard`, `agent` (interactive or one-shot), `status`, `version`.

Not yet: long-term memory, MCP, chat channels (Telegram, Slack, ...), cron,
the gateway daemon, process sandboxing. See [docs/PORTING.md](docs/PORTING.md)
for the roadmap.

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
and `exit`, `quit` or Ctrl-D quit. `/new` starts a fresh conversation.
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
    "web": {"enable": true, "fetchMaxBytes": 2097152}
  }
}
```

With `"provider": "auto"` the provider is chosen from the model name: a
`<provider>/` prefix (`deepseek/deepseek-chat`), then a known model family
(`claude*` uses anthropic, `gpt-*`/`o1`/`o3`/`o4` use openai, `deepseek*`
uses deepseek), then a gateway (openrouter), each only if it has an API key.
`apiBase` points a preset at a proxy or any other OpenAI-compatible server.

The workspace may contain `AGENTS.md` (instructions), `SOUL.md`
(personality) and `USER.md` (about you); they are added to the system
prompt. Sessions are stored in `~/.nanobot-go/sessions/`, next to the config
file and outside the workspace.

## Architecture

```
            cmd/nanobot (onboard | agent | status | version)
                 |                       |
           internal/config         stdin / stdout
     (JSON, env, provider factory)       |
                 |                       v
                 |      +--------------------------------+   internal/bus
                 |      |  agent.Loop                    |<-- (channels, later)
                 |      |  - one worker per session key  |
                 +----->|  - ContextBuilder: system      |
                        |    prompt + history + message  |
                        |  - saves the session only      |
                        |    after a successful run      |
                        +---------------+----------------+
                                        |            \
                                        v             v
                        +---------------------+   internal/session
                        |  agent.Runner       |   (JSONL, atomic writes,
                        |  model <-> tools    |    history window)
                        |  loop, iteration cap|
                        +----+-----------+----+
                             |           |
                             v           v
                internal/providers   internal/tools (+ builtin)
                Anthropic, OpenAI-   registry, validation,
                compatible, retry,   fs / exec / web_fetch
                FallbackProvider          |
                                          v
                                  internal/security
                                  workspace containment,
                                  shell guard, SSRF checks
```

The split follows nanobot: the **Loop** owns the channel-facing turn
(session, context, delivery) and the **Runner** owns the model/tool loop.

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
- The config file and session files are written with mode `0600`.
  `nanobot status` reports which keys are set, never their values.
- A turn whose provider call fails or is canceled is not saved, so errors
  never poison the conversation history.

## Development

```sh
gofmt -l .
go build ./...
go vet ./...
go test -race ./...
```

Tests use fakes and `httptest`; they need no network access or API keys.
