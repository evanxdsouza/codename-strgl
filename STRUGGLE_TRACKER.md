# Struggle tracker (v0)

This fork of Hackatime separates the time a person spends on a project from the
time an AI agent spends running for them. Everything else works like Hackatime.

## What v0 adds

- **`actor` on heartbeats.** Clients may send `"actor": "human"` or
  `"actor": "agent"` with any heartbeat on the WakaTime-compatible API
  (`POST /api/hackatime/v1/users/current/heartbeats`). Heartbeats without it,
  such as those from normal editor plugins and imports, count as human.
- **Human/agent split, behind a flag.** Enable the Flipper flag
  `human_agent_split` for a user (or everyone) at `/flipper`. Their dashboard
  then shows human time as the main total and agent-running time beside it.
  With the flag off, durations are exactly Hackatime's.
- **Lookback** at `/my/lookback`. Pick a day, see each session with human and
  agent time, and move a time range to another project. To split a session,
  move only part of it. Originals are soft-deleted, not destroyed, and every
  move is logged.
- **Justification block.** The Lookback page generates plain text for the YSWS
  "Override Hours Spent Justification" field, for chosen projects and dates.

## How time is counted

Hackatime's rule is unchanged: each heartbeat adds the gap since the previous
one, capped at 2 minutes. Human time is that rule applied to the human-credited
heartbeats: every heartbeat not marked `agent` (editor plugins and imports have
no actor and count as human), plus agent heartbeats up to 2 minutes after the
latest human one. Agent-running time is the total minus human time. So:

- each prompt credits the person with up to 2 minutes, ending early at the
  next prompt or when the agent stops;
- human + agent always equals the original total;
- when a person and an agent are both active, the time counts as human;
- one prompt followed by a 10 minute agent run gives 2 minutes human and
  8 minutes agent.

## What is sent and what stays local

Sent to the server: the same metadata as any WakaTime plugin (time, project
name, file or entity, language, editor, operating system, machine, branch)
plus `actor` and, if the client sends them, the existing AI fields
(`ai_model`, token counts, prompt length as a number).

Never sent by design: prompt text, model responses, file contents and diffs.
The server has no field for them.

## Collector

[`collector/`](collector/README.md) holds a Go program that reads Claude Code
and Codex transcripts locally and sends heartbeats with `actor` set. Its README
covers setup, what it reads and its known limits.
