<script lang="ts">
  import { Form, Link } from "@inertiajs/svelte";
  import Button from "../../../components/Button.svelte";
  import FormField from "../../../components/FormField.svelte";
  import TextInput from "../../../components/TextInput.svelte";
  import { reviewStruggleSubmissions } from "../../../api";
  import { secondsToHours } from "../../../utils";

  type Item = {
    id: string;
    kind: string;
    status: string;
    occurred_at: number;
    tool?: string;
    command?: string;
    exit?: number;
    is_error?: boolean;
    text?: string;
    redactions?: { type: string }[];
  };
  type Turn = {
    id: string;
    harness: string;
    model: string;
    started_at: number;
    end_reason: string;
    backfilled: boolean;
    tool_calls: number;
    child_sessions: number;
    files_changed: string[];
    diff_observed: boolean;
    label: {
      value: number;
      reason: string;
      signals: string[];
      needs_review: boolean;
      coverage: string;
      productive_seconds: number;
    };
    items: Item[];
  };
  type RelabelRun = {
    id: number;
    status: string;
    model: string;
    prompt_version: string;
    requested_by: string;
    reason: string;
    error: string | null;
    cost_cents: number | null;
    at: string;
  };
  type ModelRelabel = {
    available: boolean;
    consent: boolean;
    proposals: Record<string, { value: number; reason: string }>;
    runs: RelabelRun[];
  };
  type Decision = {
    id: number;
    reviewer: string;
    action: string;
    status_before: string;
    status_after: string;
    approved_seconds: number | null;
    justification: string;
    user_explanation: string | null;
    rule_set: string;
    bundle_digest: string;
    at: string;
  };

  let {
    submission,
    turns,
    notes,
    justification,
    actions,
    decisions,
    evidence_reads,
    model_relabel,
    errors = {},
  }: {
    submission: Record<string, any>;
    turns: Turn[];
    notes: string[];
    justification: string;
    actions: string[];
    decisions: Decision[];
    evidence_reads: { user: string; scope: string; at: string }[];
    model_relabel: ModelRelabel;
    errors?: { decision?: string; relabel?: string };
  } = $props();

  let action = $state("");
  const time = (ms: number) =>
    new Date(ms).toISOString().replace("T", " ").slice(0, 19) + " UTC";
  const labels: Record<string, string> = {
    approve: "Approve claimable hours",
    adjust: "Approve adjusted hours",
    reject: "Reject",
    request_relabel: "Request relabelling",
    escalate: "Escalate to fraud squad",
    return_not_fraud: "Return as not fraud",
  };
</script>

<svelte:head>
  <title>Struggle review | Hackatime</title>
</svelte:head>

<div class="mx-auto w-full max-w-5xl space-y-6 py-8">
  <header>
    <Link
      href={reviewStruggleSubmissions.index.path()}
      class="text-sm text-muted underline">Back to queue</Link
    >
    <h1 class="mt-2 text-2xl font-bold text-surface-content">
      {submission.project} by {submission.user}
    </h1>
    <p class="mt-1 text-muted">
      {submission.range_start.slice(0, 10)} to {submission.range_end.slice(
        0,
        10,
      )}, status <b>{submission.status.replace("_", " ")}</b>
    </p>
  </header>

  <div
    class="grid grid-cols-2 gap-4 rounded-2xl border border-surface-200 bg-dark p-6 text-sm text-surface-content sm:grid-cols-4"
  >
    <div>
      <div class="text-muted">Estimated human activity</div>
      <div class="text-lg font-semibold">
        {secondsToHours(submission.estimated_human_seconds)}
      </div>
    </div>
    <div>
      <div class="text-muted">Proposed productive</div>
      <div class="text-lg font-semibold">
        {secondsToHours(submission.proposed_productive_seconds)}
      </div>
    </div>
    <div>
      <div class="text-muted">Claimable (not credited before)</div>
      <div class="text-lg font-semibold">
        {secondsToHours(submission.claimable_seconds)}
      </div>
    </div>
    <div>
      <div class="text-muted">Approved</div>
      <div class="text-lg font-semibold">
        {submission.approved_seconds === null
          ? "not decided"
          : secondsToHours(submission.approved_seconds)}
      </div>
    </div>
    <div class="col-span-2 text-muted sm:col-span-4">
      Agent running, not credited: {secondsToHours(submission.agent_seconds)}.
      Already credited elsewhere: {secondsToHours(
        submission.duplicate_seconds,
      )}. {submission.turns_count} turns, {submission.labelled_one_count}
      labelled 1, {submission.needs_review_count} need review, {submission.backfilled_count}
      backfilled. Chain {submission.chain_status} ({submission.chain_anchors_matched}
      anchors). Rule set {submission.rule_set}, redaction {submission.redaction_version},
      collector {submission.collector_version}.
      <a
        class="underline"
        href={reviewStruggleSubmissions.bundle.path({ id: submission.id })}
        >Download bundle</a
      >
      <span class="break-all">sha256 {submission.digest}</span>
    </div>
    {#if submission.user_context}
      <div class="col-span-2 sm:col-span-4">
        <div class="text-muted">User's context</div>
        <p class="whitespace-pre-wrap">{submission.user_context}</p>
      </div>
    {/if}
  </div>

  <div class="space-y-3">
    {#each turns as turn (turn.id)}
      <div
        class="rounded-2xl border border-l-8 border-surface-200 bg-dark p-4 text-sm text-surface-content {turn
          .label.value === 1
          ? 'border-l-success'
          : 'border-l-surface-300'}"
      >
        <div class="font-semibold">
          {time(turn.started_at)}
          {turn.harness}
          {turn.model}: label {turn.label.value}, {turn.label
            .productive_seconds}s proposed
          {#if turn.label.needs_review}<span class="text-warning"
              >(needs review)</span
            >{/if}
          {#if turn.backfilled}<span class="text-warning">(backfilled)</span
            >{/if}
        </div>
        <div>{turn.label.reason}</div>
        {#if model_relabel.proposals[turn.id]}
          <div
            class="mt-1 text-muted {model_relabel.proposals[turn.id].value !==
            turn.label.value
              ? 'text-warning'
              : ''}"
          >
            Model second opinion: label {model_relabel.proposals[turn.id]
              .value}{model_relabel.proposals[turn.id].value !==
            turn.label.value
              ? " (differs from the rules)"
              : ""}. {model_relabel.proposals[turn.id].reason}
          </div>
        {/if}
        <div class="text-muted">
          Signals: {turn.label.signals.join(", ") || "none"}. Coverage {turn
            .label.coverage}, ended {turn.end_reason}, {turn.tool_calls} tool calls,
          {turn.child_sessions} child sessions{turn.files_changed.length
            ? `, files changed: ${turn.files_changed.join(", ")}`
            : turn.diff_observed
              ? ", no files changed"
              : ", file changes not observed"}.
        </div>
        <details class="mt-2">
          <summary class="cursor-pointer text-muted"
            >{turn.items.length} evidence items</summary
          >
          {#each turn.items as item (item.id)}
            <div class="mt-2 rounded-md border border-surface-200 bg-input p-2">
              <div class="text-xs text-muted">
                {item.kind}
                {item.status}{item.tool ? `, ${item.tool}` : ""}{item.command
                  ? `: ${item.command}`
                  : ""}{item.exit !== undefined && item.exit !== null
                  ? `, exit ${item.exit}`
                  : ""}{item.is_error ? ", error" : ""}{item.redactions?.length
                  ? `, ${item.redactions.length} redacted`
                  : ""}
              </div>
              {#if item.text}<pre
                  class="max-h-64 overflow-auto whitespace-pre-wrap text-xs">{item.text}</pre>{/if}
            </div>
          {/each}
        </details>
      </div>
    {/each}
  </div>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6 text-sm">
    <h2 class="text-lg font-semibold text-surface-content">Model re-check</h2>
    <p class="mt-1 text-muted">
      Sends a minimised copy of this redacted bundle to the configured model for
      a second opinion on the labels. It never changes any hours.
      {model_relabel.consent
        ? "The user allowed this."
        : "The user did not allow this, so it cannot be sent."}
    </p>
    {#if errors.relabel}<p class="mt-2 text-error">{errors.relabel}</p>{/if}
    {#if model_relabel.available}
      <Form
        action={reviewStruggleSubmissions.requestModelRelabel.path({
          id: submission.id,
        })}
        method="post"
        class="mt-4 space-y-4"
      >
        <FormField inputId="relabel_reason" label="Why re-check (logged)">
          <TextInput id="relabel_reason" name="reason" required />
        </FormField>
        <Button type="submit" variant="primary">Ask the model</Button>
      </Form>
    {/if}
    {#if model_relabel.runs.length > 0}
      <ul class="mt-4 space-y-1 text-surface-content">
        {#each model_relabel.runs as run (run.id)}
          <li>
            {run.at.replace("T", " ").slice(0, 16)} UTC, {run.requested_by}:
            <b>{run.status}</b>, {run.model} ({run.prompt_version}){run.cost_cents !==
            null
              ? `, ${run.cost_cents}¢`
              : ""}.
            <span class="text-muted">{run.reason}</span>
            {#if run.error}<span class="text-warning">{run.error}</span>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6">
    <h2 class="text-lg font-semibold text-surface-content">Decision</h2>
    {#if actions.length === 0}
      <p class="mt-2 text-sm text-muted">No action is available to you.</p>
    {:else}
      <Form
        action={reviewStruggleSubmissions.decide.path({ id: submission.id })}
        method="post"
        class="mt-4 space-y-4"
      >
        <FormField
          inputId="decision_action"
          label="Action"
          error={errors.decision}
        >
          <select
            id="decision_action"
            name="decision_action"
            bind:value={action}
            required
            class="w-full rounded-md border border-surface-200 bg-input p-2 text-surface-content"
          >
            <option value="" disabled>Choose</option>
            {#each actions as a}
              <option value={a}>{labels[a] ?? a}</option>
            {/each}
          </select>
        </FormField>
        {#if action === "adjust"}
          <FormField inputId="approved_hours" label="Approved hours">
            <TextInput
              id="approved_hours"
              name="approved_hours"
              type="number"
              step="0.01"
              min="0"
              required
            />
          </FormField>
        {/if}
        <FormField
          inputId="justification"
          label="Justification (reviewer-facing; if hours were cut, why)"
        >
          <textarea
            id="justification"
            name="justification"
            rows="4"
            required
            class="w-full rounded-md border border-surface-200 bg-input p-2 text-surface-content"
          ></textarea>
        </FormField>
        <FormField
          inputId="user_explanation"
          label="Explanation shown to the user (optional)"
        >
          <textarea
            id="user_explanation"
            name="user_explanation"
            rows="2"
            class="w-full rounded-md border border-surface-200 bg-input p-2 text-surface-content"
          ></textarea>
        </FormField>
        <Button type="submit" variant="primary">Record decision</Button>
      </Form>
    {/if}
    <p class="mt-4 text-xs text-muted">
      Low effort or unclear evidence is not by itself fraud. Escalate only for
      suspected deliberate deception; a chain mismatch alone is a discrepancy to
      investigate, not proof. "Request relabelling" returns the submission to
      the user to resubmit; the model re-check above is only a second opinion.
    </p>
  </div>

  {#if decisions.length > 0}
    <div class="rounded-2xl border border-surface-200 bg-dark p-6 text-sm">
      <h2 class="text-lg font-semibold text-surface-content">History</h2>
      <ul class="mt-2 space-y-2 text-surface-content">
        {#each decisions as d (d.id)}
          <li>
            {d.at.replace("T", " ").slice(0, 16)} UTC, {d.reviewer}:
            <b>{d.action.replace("_", " ")}</b>
            ({d.status_before} to {d.status_after}{d.approved_seconds !== null
              ? `, ${secondsToHours(d.approved_seconds)}`
              : ""}), rule set {d.rule_set}, bundle {d.bundle_digest.slice(
              0,
              12,
            )}.
            <span class="whitespace-pre-wrap text-muted">{d.justification}</span
            >
          </li>
        {/each}
      </ul>
    </div>
  {/if}

  <div class="rounded-2xl border border-surface-200 bg-dark p-6 text-sm">
    <h2 class="text-lg font-semibold text-surface-content">
      Justification block
    </h2>
    <p class="mt-1 text-muted">
      For the program's "Override Hours Spent Justification" field. This
      reviewer copy includes your internal justification.
    </p>
    <pre
      class="mt-2 max-h-96 overflow-auto whitespace-pre-wrap rounded-md border border-surface-200 bg-input p-4 text-xs text-surface-content">{justification}</pre>
  </div>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6 text-sm">
    <h2 class="text-lg font-semibold text-surface-content">Evidence reads</h2>
    <ul class="mt-2 space-y-1 text-muted">
      {#each evidence_reads as read}
        <li>
          {read.at.replace("T", " ").slice(0, 16)} UTC, {read.user} ({read.scope})
        </li>
      {/each}
    </ul>
    {#each notes as note}<p class="mt-2 text-xs text-muted">{note}</p>{/each}
  </div>
</div>
