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
and Codex transcripts locally, plus metadata from an OpenCode v2 local plugin,
and sends heartbeats with `actor` set. OpenCode needs the plugin installed;
its prompts and responses are not copied into the tracking spool. The README
covers setup, what it reads and its known limits.

## v1: evidence-backed labels and review

v1 adds a reviewable record of *how* the time was spent, without sending raw
prompts during normal tracking.

- **Local turn timeline.** The collector records each genuine human prompt as
  a turn, with the agent's tool calls, outputs, interruptions and child
  sessions, in `~/.strgl/evidence.db` (SQLite). Text is encrypted with
  AES-256-GCM; the key is protected by DPAPI on Windows, the login Keychain
  on macOS and an owner-only file on Linux. Content older than 30 days
  (`-retention`), or beyond 256 MB, is purged; its commitments stay.
  Repository snapshots each scan attribute file changes made while an agent
  turn ran to that turn, and changes made between turns to the person's next
  prompt, or, after 10 quiet minutes, to a standalone turn with no prompt that
  is flagged for review. `-local-only` records the timeline without sending anything.
- **Local preview.** `strgl-collector preview -project NAME` serves a page on
  127.0.0.1 only. It shows the redacted bundle byte for byte, lets the user
  leave items out, add context and extra words to redact, and uploads only
  the bytes whose SHA-256 it displayed. Redaction (`redact-v1.0`) replaces
  secret formats, emails, phone numbers, IP addresses, home and project
  paths with typed placeholders carrying a per-install salted hash. It fails
  closed: an item that still matches after redaction is withheld. It is not
  guaranteed anonymisation.
- **Per-turn 0/1 labels** (rule set `struggle-v1.0`, `collector/labels.go`).
  Each label has a reason, signals, evidence item IDs, coverage and a
  needs-review flag. Diagnosis, new constraints, review questions and the
  person's own edits earn a 1; a passing test, a diff or a long prompt alone
  do not. Bare re-prompts ("try again") get 0 with coaching. Uncertain or
  poorly observed turns are flagged for review, never treated as fraud.
  Proposed productive seconds use the same 2-minute window as the activity
  estimate, deduplicated across overlapping turns and sessions.
- **Frozen submissions** (`POST /api/v1/struggle/submissions`). The server
  re-derives every total, checks each included item against the submitted
  commitment chain and compares the chain with heads the collector anchored
  earlier (`POST /api/v1/struggle/anchors`, hashes only). Retrying the same
  bytes is idempotent. Each turn can be claimed by one live submission, so
  later ships cannot credit it again; overlapping time from another device
  is not claimable either. The bundle is stored with Active Storage.
- **Review** at `/review/struggle`. Users with `struggle_role` `reviewer`
  review normal submissions. Escalated submissions, including their evidence
  pages and downloads, are only for `fraud_squad` and SupaAdmins (superadmin
  and ultraadmin). Actions: approve, adjust (with deflation reason), reject,
  request relabelling (the user resubmits a bundle that supersedes the old
  one), escalate and return as not fraud. Every action records the reviewer,
  time, justification, bundle digest and rule set; every evidence read is
  logged. A SupaAdmin grants roles with
  `user.set_struggle_role("reviewer", changed_by_user: supa)`.
- **Justification export.** The Lookback block gains `[STRUGGLE REVIEW]`
  (estimated, proposed, claimable and approved hours per submission, chain
  status and bundle digest), filled `[DEFLATION JUSTIFICATION]`, v1 label
  counts and evidence links that require review permission. The user's own
  copy leaves out reviewers' internal justifications; reviewers get the full
  block on the submission page.

Not in v1: a stronger-model relabelling run (requesting relabelling returns
the submission to the user), an on-device NER redaction model, and direct
Unified DB integration.
