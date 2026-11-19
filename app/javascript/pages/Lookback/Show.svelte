<script lang="ts">
  import { Form, router } from "@inertiajs/svelte";
  import Button from "../../components/Button.svelte";
  import FormField from "../../components/FormField.svelte";
  import TextInput from "../../components/TextInput.svelte";
  import { myLookback } from "../../api";
  import { secondsToDisplay } from "../../utils";

  type Session = {
    project: string;
    start_time: string;
    end_time: string;
    human_seconds: number;
    agent_seconds: number;
    harness: string;
    prompts: number;
  };

  type Correction = {
    id: number;
    start_time: string;
    end_time: string;
    from_project: string | null;
    to_project: string;
    heartbeats_count: number;
  };

  let {
    date,
    timezone,
    sessions,
    projects,
    corrections,
    justification,
    errors = {},
  }: {
    date: string;
    timezone: string;
    sessions: Session[];
    projects: string[];
    corrections: Correction[];
    justification: {
      from: string;
      to: string;
      projects: string[];
      text: string | null;
    };
    errors?: { correction?: string };
  } = $props();

  // Editable form state, filled by "Reassign" on a session row.
  let startTime = $state("");
  let endTime = $state("");
  let fromProject = $state("");
  let toProject = $state("");

  function pick(session: Session) {
    startTime = session.start_time;
    endTime = session.end_time;
    fromProject = session.project;
    toProject = "";
  }

  function changeDate(value: string) {
    router.get(myLookback.show.path({ query: { date: value } }));
  }

  let copied = $state(false);
  async function copyText() {
    if (!justification.text) return;
    await navigator.clipboard.writeText(justification.text);
    copied = true;
  }
</script>

<svelte:head>
  <title>Lookback | Hackatime</title>
</svelte:head>

<div class="mx-auto w-full max-w-4xl space-y-6 py-8">
  <header>
    <h1 class="text-2xl font-bold text-surface-content">Lookback</h1>
    <p class="mt-1 text-muted">
      Review a day afterwards. Move a time range to the project it belonged to.
      Times are in {timezone}.
    </p>
  </header>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6">
    <FormField inputId="lookback-date" label="Day">
      <TextInput
        id="lookback-date"
        type="date"
        value={date}
        onchange={(e) => changeDate((e.target as HTMLInputElement).value)}
      />
    </FormField>

    {#if sessions.length === 0}
      <p class="mt-4 text-sm text-muted">No project activity on this day.</p>
    {:else}
      <table class="mt-4 w-full text-left text-sm text-surface-content">
        <thead class="text-muted">
          <tr>
            <th class="py-2">Time</th>
            <th>Project</th>
            <th>Human</th>
            <th>Agent</th>
            <th>Harness</th>
            <th>Prompts</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {#each sessions as session}
            <tr class="border-t border-surface-200">
              <td class="py-2"
                >{session.start_time.slice(11)}-{session.end_time.slice(11)}</td
              >
              <td>{session.project}</td>
              <td>{secondsToDisplay(session.human_seconds)}</td>
              <td>{secondsToDisplay(session.agent_seconds)}</td>
              <td>{session.harness}</td>
              <td>{session.prompts}</td>
              <td class="text-right">
                <Button
                  size="xs"
                  variant="surface"
                  onclick={() => pick(session)}>Reassign</Button
                >
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6">
    <h2 class="text-lg font-semibold text-surface-content">
      Reassign or split a time range
    </h2>
    <p class="mt-1 text-sm text-muted">
      To split a session, narrow the range to the part that belongs elsewhere.
      The original attribution is kept and the change is logged.
    </p>
    <Form
      action={myLookback.createCorrection.path()}
      method="post"
      class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2"
      options={{ preserveScroll: true }}
    >
      <input type="hidden" name="date" value={date} />
      <FormField inputId="start_time" label="From">
        <TextInput
          id="start_time"
          name="start_time"
          type="datetime-local"
          bind:value={startTime}
          required
        />
      </FormField>
      <FormField inputId="end_time" label="To">
        <TextInput
          id="end_time"
          name="end_time"
          type="datetime-local"
          bind:value={endTime}
          required
        />
      </FormField>
      <FormField inputId="from_project" label="Only move project (blank: all)">
        <TextInput
          id="from_project"
          name="from_project"
          list="lookback-projects"
          bind:value={fromProject}
        />
      </FormField>
      <FormField
        inputId="to_project"
        label="Move to project"
        error={errors.correction}
      >
        <TextInput
          id="to_project"
          name="to_project"
          list="lookback-projects"
          bind:value={toProject}
          required
        />
      </FormField>
      <datalist id="lookback-projects">
        {#each projects as project}
          <option value={project}></option>
        {/each}
      </datalist>
      <div class="sm:col-span-2">
        <Button type="submit" variant="primary">Move time</Button>
      </div>
    </Form>

    {#if corrections.length > 0}
      <h3 class="mt-6 text-sm font-semibold text-surface-content">
        Recent corrections
      </h3>
      <ul class="mt-2 space-y-1 text-sm text-muted">
        {#each corrections as correction (correction.id)}
          <li>
            {correction.start_time.replace("T", " ")} to {correction.end_time.slice(
              11,
            )}: {correction.from_project ?? "any project"} to {correction.to_project}
            ({correction.heartbeats_count} heartbeats)
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <div class="rounded-2xl border border-surface-200 bg-dark p-6">
    <h2 class="text-lg font-semibold text-surface-content">
      Justification for program reviewers
    </h2>
    <p class="mt-1 text-sm text-muted">
      Plain text for the "Override Hours Spent Justification" field. Human and
      agent-running hours are shown separately.
    </p>
    <form
      method="get"
      action={myLookback.show.path()}
      class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-3"
    >
      <input type="hidden" name="date" value={date} />
      <input type="hidden" name="justify" value="1" />
      <FormField inputId="from" label="From">
        <TextInput
          id="from"
          name="from"
          type="date"
          value={justification.from}
        />
      </FormField>
      <FormField inputId="to" label="To">
        <TextInput id="to" name="to" type="date" value={justification.to} />
      </FormField>
      <FormField
        inputId="projects"
        label="Projects (comma separated, blank: all)"
      >
        <TextInput
          id="projects"
          name="projects"
          value={justification.projects.join(", ")}
        />
      </FormField>
      <div class="sm:col-span-3">
        <Button type="submit" variant="primary">Generate</Button>
      </div>
    </form>

    {#if justification.text}
      <pre
        class="mt-4 max-h-96 overflow-auto whitespace-pre-wrap rounded-md border border-surface-200 bg-input p-4 text-xs text-surface-content">{justification.text}</pre>
      <div class="mt-2">
        <Button size="sm" variant="surface" onclick={copyText}
          >{copied ? "Copied" : "Copy"}</Button
        >
      </div>
    {/if}
  </div>
</div>
