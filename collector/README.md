# strgl-collector

A small Go program that reads Claude Code and Codex transcripts on your
machine and sends heartbeats to a Hackatime-compatible server, each marked
`human` (you sent a prompt) or `agent` (the agent was working). One static
binary, no dependencies, for Windows, macOS and Linux.

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

The project is the name of the git repository that contains the session's
working directory, or the directory name when it is not in one. The branch
comes from `.git/HEAD`.

## What is sent and what stays local

Sent, per heartbeat: time, project, branch, harness name and version,
`actor`, model name, session ID, token counts, the length of a prompt as a
number, operating system and machine name.

Never sent: prompt text, model replies, tool calls and their output, file
paths and file contents. The collector only reads them to tell lines apart.

Agent activity is sent at most once every 30 seconds per session; tokens of
skipped lines are added to the next heartbeat. Every human prompt is sent.

## Known limits

- Both transcript formats are internal to their tools and can change in any
  release. The Codex format was checked against its source code (openai/codex,
  November 2026). The Claude Code format was checked against Anthropic's docs
  and published schema write-ups only; no real transcript was available.
- Human time comes only from prompts: each prompt credits up to 2 minutes,
  ending early at the next prompt or when the agent stops. Reading output for
  longer than that is not seen. Editor plugins still add human time while you
  edit files yourself. While the agent is still running 2 minutes after a
  prompt, the collector sends an extra agent heartbeat at exactly that point
  so the server's credit window ends on time.
- Not yet tested on a real Windows or macOS machine; the binaries were only
  cross-compiled.
