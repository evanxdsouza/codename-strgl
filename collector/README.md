# strgl-collector

A small Go program that reads Claude Code and Codex transcripts and OpenCode
plugin metadata on your machine. It sends heartbeats to a Hackatime-compatible
server, each marked `human` (you sent a prompt) or `agent` (the agent was
working). One static collector binary, no dependencies, for Windows, macOS
and Linux. OpenCode additionally needs the local plugin below.

## Build

```sh
cd collector
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o strgl-collector.exe .
```

Use `GOOS=darwin` or `GOOS=linux` and `GOARCH=arm64`, `amd64` or `386` for
other machines. Go 1.22 or later.

## Set up

It reads `api_url` and `api_key` from `~/.wakatime.cfg`, the file editor
plugins already use, or from `STRGL_API_URL` and `STRGL_API_KEY`:

```ini
[settings]
api_url = https://your-server/api/hackatime/v1
api_key = your-key
```

Then run it and leave it running:

```sh
strgl-collector            # scans every minute
strgl-collector -once      # scan once and exit
strgl-collector -dry-run   # print what would be sent, send nothing
```

On first run it reads the last 7 days (`-backfill`). Read positions are kept
in `~/.strgl/state.json`, so restarting does not send anything twice. Start it
at login with Task Scheduler, a launchd agent or a systemd user service.

## Where it reads

| Harness | Files |
| --- | --- |
| Claude Code | `$CLAUDE_CONFIG_DIR/projects/**/*.jsonl`, default `~/.claude/projects` (subagent transcripts included) |
| Codex | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl`, default `~/.codex/sessions` |
| OpenCode v2 | `$STRGL_OPENCODE_DIR/*.jsonl`, default `~/.strgl/opencode`, written by the local plugin |

The project is the name of the git repository that contains the session's
working directory, or the directory name when it is not in one. The branch
comes from `.git/HEAD`.

### OpenCode setup

Install [OpenCode v2](https://opencode.ai/v2/docs/plugins), then copy
`collector/opencode.js` into the `plugins` folder under the config directory
shown by `opencode debug paths`. With the default config location, from the
repository root on macOS or Linux:

```sh
mkdir -p ~/.config/opencode/plugins
cp collector/opencode.js ~/.config/opencode/plugins/strgl.js
```

On Windows, copy the same file to that config directory's `plugins` folder
as `strgl.js`. Restart OpenCode's server (`opencode service restart` for the
shared service) and leave the collector running on the same machine. Restart
an explicit `opencode serve` or standalone process instead if you use one.
The plugin uses OpenCode's bundled runtime; no separate Node installation or
npm package is required. It works with whichever model the session uses.

The plugin observes the public server event stream, not OpenCode's internal
SQLite database. Root-session user prompts count as human; compaction,
synthetic messages and child-agent instructions do not. Running sessions are
sampled every 30 seconds. Completion, failure and interruption write a final
heartbeat and stop sampling. Only activity after installation is available
to backfill.

For a different spool location, set `STRGL_OPENCODE_DIR` to the same absolute
directory in both OpenCode's and the collector's environments. Files are
created with mode `0600` and the directory with `0700` on POSIX systems;
Windows uses the account's filesystem permissions.

## What is sent and what stays local

Sent, per heartbeat: time, project, branch, harness name and version,
`actor`, model name, session ID, token counts, the length of a prompt as a
number, operating system and machine name.

Never sent: prompt text, model replies, tool calls and their output, file
paths and file contents. Claude Code and Codex transcripts are read locally
to tell lines apart. The OpenCode plugin copies no text at all: its local
spool contains only timestamps, actors, numeric lengths and token counts,
session/model/version identifiers and the working directory for project
attribution. Project names, branches and machine names can still identify
you; this is metadata minimisation, not guaranteed PII anonymisation.

Agent samples are downsampled to 30-second intervals per session; tokens of
skipped lines are added to the next heartbeat. OpenCode completion events
always flush the last heartbeat and pending tokens. Every human prompt is
sent. Durable OpenCode event sequences are deduplicated across collector
restarts.

## Checks

From `collector/`:

```sh
go test ./...
go vet ./...
node --test opencode_test.js
```

Node.js 22 or later is only needed for the plugin's tests.

## Known limits

- Both transcript formats are internal to their tools and can change in any
  release. The Codex format was checked against its source code (openai/codex,
  November 2026). The Claude Code format was checked against Anthropic's docs
  and published schema write-ups only; no real transcript was available.
- The OpenCode plugin targets the v2 public server API and was exercised with
  OpenCode 2.3.2 on Linux. OpenCode v1's plugin API is not supported.
- Human time comes only from prompts: each prompt credits up to 2 minutes,
  ending early at the next prompt or when the agent stops. Reading output for
  longer than that is not seen. Editor plugins still add human time while you
  edit files yourself. While the agent is still running 2 minutes after a
  prompt, the collector sends an extra agent heartbeat at exactly that point
  so the server's credit window ends on time.
- Not yet tested on a real Windows or macOS machine; the binaries were only
  cross-compiled.
