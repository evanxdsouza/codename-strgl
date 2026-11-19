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
one, capped at 2 minutes. Human time is that rule applied to human heartbeats
only. Agent-running time is the total minus human time. So:

- human + agent always equals the original total;
- when a person and an agent are both active, the time counts as human;
- a 10 minute agent run where the person only typed for 30 seconds gives
  roughly 0.5 minutes human and 9.5 minutes agent, not 10 minutes human.

## What is sent and what stays local

Sent to the server: the same metadata as any WakaTime plugin (time, project
name, file or entity, language, editor, operating system, machine, branch)
plus `actor` and, if the client sends them, the existing AI fields
(`ai_model`, token counts, prompt length as a number).

Never sent by design: prompt text, model responses, file contents and diffs.
The server has no field for them.

## Collector

The Go collector that reads Claude Code and Codex transcripts and sends
heartbeats with `actor` is not part of this change. Until it exists, `actor`
only arrives from clients that set it themselves, and all other time counts as
human, as in Hackatime.
