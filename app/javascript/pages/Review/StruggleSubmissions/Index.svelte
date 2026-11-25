<script lang="ts">
  import { Link } from "@inertiajs/svelte";
  import { reviewStruggleSubmissions } from "../../../api";
  import { secondsToHours } from "../../../utils";

  type Submission = {
    id: number;
    user: string;
    project: string;
    range_start: string;
    range_end: string;
    status: string;
    estimated_human_seconds: number;
    proposed_productive_seconds: number;
    claimable_seconds: number;
    approved_seconds: number | null;
    turns_count: number;
    needs_review_count: number;
    chain_status: string;
    submitted_at: string;
  };

  let {
    status,
    statuses,
    can_view_escalated,
    submissions,
  }: {
    status: string | null;
    statuses: string[];
    can_view_escalated: boolean;
    submissions: Submission[];
  } = $props();

  const visibleStatuses = $derived(
    statuses.filter((s) => can_view_escalated || s !== "escalated"),
  );
</script>

<svelte:head>
  <title>Struggle review | Hackatime</title>
</svelte:head>

<div class="mx-auto w-full max-w-5xl space-y-6 py-8">
  <header>
    <h1 class="text-2xl font-bold text-surface-content">Struggle review</h1>
    <p class="mt-1 text-muted">
      Submitted evidence bundles. Proposed hours are the tracker's labels;
      approved hours are yours to decide. Opening a submission is logged.
      {#if !can_view_escalated}Escalated submissions are only visible to the
        fraud squad and SupaAdmins.{/if}
    </p>
  </header>

  <nav class="flex flex-wrap gap-2 text-sm">
    <Link
      href={reviewStruggleSubmissions.index.path()}
      class="rounded-full border border-surface-200 px-3 py-1 {status
        ? 'text-muted'
        : 'text-surface-content'}">All</Link
    >
    {#each visibleStatuses as s}
      <Link
        href={reviewStruggleSubmissions.index.path({ query: { status: s } })}
        class="rounded-full border border-surface-200 px-3 py-1 {status === s
          ? 'text-surface-content'
          : 'text-muted'}">{s.replace("_", " ")}</Link
      >
    {/each}
  </nav>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6">
    {#if submissions.length === 0}
      <p class="text-sm text-muted">No submissions.</p>
    {:else}
      <table
        class="w-full text-left text-sm text-surface-content [&_td]:pr-4 [&_th]:pr-4"
      >
        <thead class="text-muted">
          <tr>
            <th class="py-2">Submitted</th>
            <th>User</th>
            <th>Project</th>
            <th>Status</th>
            <th>Estimated</th>
            <th>Proposed</th>
            <th>Claimable</th>
            <th>Approved</th>
            <th>Turns</th>
            <th>Chain</th>
          </tr>
        </thead>
        <tbody>
          {#each submissions as submission (submission.id)}
            <tr class="border-t border-surface-200">
              <td class="py-2">
                <Link
                  href={reviewStruggleSubmissions.show.path({
                    id: submission.id,
                  })}
                  class="underline">{submission.submitted_at.slice(0, 10)}</Link
                >
              </td>
              <td>{submission.user}</td>
              <td>{submission.project}</td>
              <td>{submission.status.replace("_", " ")}</td>
              <td>{secondsToHours(submission.estimated_human_seconds)}</td>
              <td>{secondsToHours(submission.proposed_productive_seconds)}</td>
              <td>{secondsToHours(submission.claimable_seconds)}</td>
              <td
                >{submission.approved_seconds === null
                  ? "-"
                  : secondsToHours(submission.approved_seconds)}</td
              >
              <td
                >{submission.turns_count}{submission.needs_review_count
                  ? ` (${submission.needs_review_count} to check)`
                  : ""}</td
              >
              <td>{submission.chain_status}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
</div>
