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
  the bytes whose SHA-256 it displayed. Redaction (`redact-v1.2`) replaces
  secret formats, emails, phone numbers (including UK numbers), UK postcodes, IP addresses, home and project
  paths, and names, schools, places, handles, street addresses and birth
  dates where the text announces them ("my name is", "Mr.", "Author:",
  "… High School", "@handle"), with typed placeholders carrying a
  per-install salted hash. It fails closed: an item that still matches after
  redaction is withheld. It is not guaranteed anonymisation: names with no
  such context are missed.
- **Redaction recall** is measured by `TestRedactionRecall` on two annotated
  corpora in `collector/testdata`: a development set the rules were written
  against and a held-out set. Both are written by us, not real user prompts,
  so the per-type floors guard against regressions rather than prove
  real-world recall.
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

## v2 (in progress)

- **Devlog drafts.** `strgl-collector devlog -project NAME`, and a box on
  the preview page, draft a devlog per day from the redacted bundle:
  problems investigated, the person's own edits, test runs and what did not
  work. No labels or proposed hours appear in it. It stays on the device;
  the user edits it and posts it to their program themselves.
- **Optional entity model.** `-ner CMD` (or `STRGL_NER_HELPER`) runs a
  user-installed named-entity model after the rules, through a separate
  helper process (`collector/ner`). It is off by default and never part of
  normal tracking. If it is enabled and fails, nothing is uploaded. With a
  helper set, `TestRedactionRecall` requires 99% recall for secrets and
  contact details and 90% for names, schools and places. The default,
  `dslim/distilbert-NER`, is accepted as opt-in although it reaches only
  80% for names and places on the newest held-out set (10 names, 5 places,
  synthetic); the test records that shortfall explicitly.
- **Model re-check (M4).** The preview has an unticked box, and
  `bundle`/`preview` take `-allow-model-relabel`, which sets
  `model_relabel_consent` in the frozen bundle. On a consented submission
  awaiting ordinary review (never an escalated one), a reviewer can press
  "Ask the model". The server sends a minimised copy of the redacted bundle,
  with no project, device, turn IDs, times or rule labels, after checking it
  again for leaks. The model labels each turn blind; its labels appear beside
  the rule labels as a second opinion and never change estimated, proposed,
  claimable or approved time. Every request is logged as a `model_relabel`
  evidence read.

### Self-hosting the re-check model

Any OpenAI-compatible endpoint works, but the intended setup is a small
model on your own server, so the payload never leaves it. Tested with
llama.cpp's `llama-server` and `Qwen2.5-1.5B-Instruct` (Q4_K_M, about
1.1 GB on disk and 2.2 GB of RAM):

```sh
llama-server -m qwen2.5-1.5b-instruct-q4_k_m.gguf --host 127.0.0.1 --port 8091 -c 8192 --alias qwen2.5-1.5b
```

```env
STRGL_RELABEL_PROVIDER=self-hosted
STRGL_RELABEL_URL=http://127.0.0.1:8091/v1
STRGL_RELABEL_MODEL=qwen2.5-1.5b
STRGL_RELABEL_MONTHLY_RUN_CAP=200
STRGL_RELABEL_INPUT_CENTS_PER_MTOK=0
STRGL_RELABEL_OUTPUT_CENTS_PER_MTOK=0
STRGL_RELABEL_MAX_OUTPUT_TOKENS=200
```

With both prices 0 the run cap is the only limit. For a hosted provider set
its prices, `STRGL_RELABEL_MONTHLY_CAP_CENTS`, `STRGL_RELABEL_RUN_CAP_CENTS`
and `STRGL_RELABEL_API_KEY`, and use an account that neither retains nor
trains on the data. Unset or zero caps keep the feature off. The answer is
constrained by a JSON schema, which llama.cpp enforces as a grammar. Each
turn is sent as its own request (`STRGL_RELABEL_MAX_OUTPUT_TOKENS` is per
turn), because with the whole list small models drifted towards giving
every later turn the same label.

Agreement with hand labels on 20 short synthetic turns (two sets of 10,
one turn per request, 4 ARM cores):

| Model (Q4_K_M) | Agreed | RAM | Time per turn |
| --- | --- | --- | --- |
| Qwen2.5-3B-Instruct | 18 of 20 | about 4.1 GB | about 5 s |
| Qwen2.5-1.5B-Instruct | 16 to 17 of 20 | about 2.2 GB | about 2.5 s |

The 1.5B model is the default because it needs half the RAM; its misses
were short diagnoses such as "no, off by one", labelled 0, so it can
under-credit a terse student. Use the 3B model where 4 GB of RAM is free. Qwen3 0.6B and 1.7B, Gemma 3 1B,
Llama 3.2 1B and Granite 4.0 1B scored 7 of 10 or less on the first set.
This is a smoke check, not a validation.

Not in v1: a stronger-model relabelling run (requesting relabelling returns
the submission to the user), an on-device NER redaction model, and direct
Unified DB integration.
