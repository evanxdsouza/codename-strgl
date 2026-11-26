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
other machines. Go 1.26 or later (for the pure-Go SQLite driver; still no cgo).

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

It also records a local turn timeline with encrypted evidence (see
"Evidence and submissions" below); `-evidence=false` turns that off and
`-local-only` records it without sending heartbeats or anchors.

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

## Evidence and submissions

Each scan also adds what the harnesses wrote to `~/.strgl/evidence.db`: one
turn per human prompt, with the prompt, replies, tool calls and outputs, and
the diffs the collector saw in the repository. Text is encrypted at rest.
Turns read from transcripts more than 10 minutes after the fact are marked
backfilled. Each stored item gets a salted commitment in a hash chain, and
the newest chain head (a hash only) is sent to the server once per scan.

```sh
strgl-collector timeline -project NAME            # turns and labels, no text
strgl-collector bundle -project NAME > b.json     # exact upload bytes, not sent
strgl-collector devlog -project NAME              # devlog draft to edit, not sent
strgl-collector preview -project NAME -from 2026-11-01 -to 2026-11-07
```

`preview` prints a `http://127.0.0.1:…/?t=…` link that only works on this
device. Inspect each item, leave items out, add context or words to
redact, then press Submit; only the bytes shown are uploaded. The unticked
box (or `-allow-model-relabel`) lets a reviewer send that redacted bundle to
an external AI model to re-check the labels; it never changes your hours. To answer a
relabelling request, run preview again with `-supersedes DIGEST`.

Redaction uses deterministic rules. An optional on-device entity model can
also remove names the rules miss: pass `-ner CMD` or set
`STRGL_NER_HELPER`. It is off by default, runs only for these commands, and
withholds everything if it fails. See [ner/README.md](ner/README.md); no
model meets the name target on every test set yet; the default is accepted
as an opt-in layer below it.

`devlog` (and the bottom of the preview page) drafts a devlog per day from
the same redacted items the preview shows, leaving out anything you
excluded: problems you investigated, files you changed yourself, test runs
and what did not work. It shows no labels or proposed hours, only the
activity estimate. The collector never uploads it; edit it, check it for
names the redaction missed, and post it to your program yourself.

The OpenCode plugin writes the text the evidence store needs to a separate
spool (`~/.strgl/opencode-evidence`, or `$STRGL_OPENCODE_DIR/evidence`);
the collector deletes those files two days after reading them. Set
`STRGL_OPENCODE_EVIDENCE=0` in OpenCode's environment to stop it.

## What is sent and what stays local

Sent, per heartbeat: time, project, branch, harness name and version,
`actor`, model name, session ID, token counts, the length of a prompt as a
number, operating system and machine name.

Also sent: one evidence chain head (a SHA-256 hash) per scan.

Sent only when you press Submit in `preview`: the redacted bundle the
preview showed.

Never sent otherwise: prompt text, model replies, tool calls and their
output, file paths and file contents. Claude Code and Codex transcripts are read locally
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
- Not yet tested on a real Windows or macOS machine; the binaries, including
  the DPAPI and Keychain key storage, were only cross-compiled.
- File changes are attributed per scan (one minute by default), not per
  keystroke. Changes made while no agent runs go to your next prompt in that
  repository if it comes within 10 minutes. Otherwise, once the repository
  has been quiet for 10 minutes, they become a standalone "editor" turn that
  credits at most the two-minute estimate, rests on the file changes alone,
  is labelled with partial coverage and always needs a reviewer. Only
  repositories with an agent turn in the last 24 hours (at most 8) are
  watched, so editing in a repository you have never prompted an agent in is
  not captured. Reading, research and thinking leave no trace; changes made
  by an AI assistant inside your editor look like your own.
- Claude Code does not record exit codes; a failing tool result counts as an
  error, a successful one as passing.
