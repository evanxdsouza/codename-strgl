// SPDX-License-Identifier: AGPL-3.0-only

import { appendFile, mkdir } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

// OpenCode v2 local server plugin. The metadata spool holds no text and is
// what heartbeats are built from. A separate local evidence spool holds the
// prompt, response and tool text the collector encrypts into its evidence
// store; it never leaves the device. Set STRGL_OPENCODE_EVIDENCE=0 to turn
// the evidence spool off.
export default {
  id: "strgl-collector",
  async setup(ctx) {
    const directory =
      process.env.STRGL_OPENCODE_DIR || join(homedir(), ".strgl", "opencode");
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const evidenceOn = process.env.STRGL_OPENCODE_EVIDENCE !== "0";
    const evidenceDirectory =
      process.env.STRGL_OPENCODE_EVIDENCE_DIR ||
      (process.env.STRGL_OPENCODE_DIR
        ? join(process.env.STRGL_OPENCODE_DIR, "evidence")
        : join(homedir(), ".strgl", "opencode-evidence"));
    if (evidenceOn) {
      await mkdir(evidenceDirectory, { recursive: true, mode: 0o700 });
    }
    const sessions = new Map();
    const controller = new AbortController();
    let pending = Promise.resolve();

    // Timer samples and public events share a queue so files stay ordered.
    function enqueue(action) {
      pending = pending.then(action).catch(() => {
        console.error(
          "strgl-collector: could not record local OpenCode activity",
        );
      });
      return pending;
    }

    async function write(sessionID, session, fields) {
      const record = {
        schema: 1,
        session_id: sessionID,
        cwd: session.directory,
        model: session.model,
        version: ctx.app.version,
        actor: "agent",
        ...fields,
      };
      await appendFile(
        join(directory, encodeURIComponent(sessionID) + ".jsonl"),
        JSON.stringify(record) + "\n",
        {
          encoding: "utf8",
          mode: 0o600,
        },
      );
    }

    // One file per session per UTC day, so the collector can delete fully
    // read plaintext files once they are old.
    async function evidence(sessionID, session, event, fields) {
      if (!evidenceOn) return;
      const record = {
        schema: 1,
        session_id: sessionID,
        parent_id: session.parentID,
        cwd: session.directory,
        model: session.model,
        version: ctx.app.version,
        timestamp: event.created,
        sequence: event.durable?.seq,
        ...fields,
      };
      const day = new Date(event.created).toISOString().slice(0, 10);
      await appendFile(
        join(
          evidenceDirectory,
          `${day}-${encodeURIComponent(sessionID)}.jsonl`,
        ),
        JSON.stringify(record) + "\n",
        { encoding: "utf8", mode: 0o600 },
      );
    }

    function toolText(content) {
      return (content || [])
        .filter((part) => part.type === "text")
        .map((part) => part.text)
        .join("\n");
    }

    async function recordEvidence(event, sessionID, session) {
      const { data } = event;
      switch (event.type) {
        case "session.inbox.enqueued":
          if (data.item.type !== "user") return;
          // A child's user-role text is an agent's instruction, not a prompt.
          return evidence(sessionID, session, event, {
            kind: session.parentID ? "response" : "prompt",
            text: data.item.payload.text,
          });
        case "session.text.ended":
          return evidence(sessionID, session, event, {
            kind: "response",
            text: data.text,
          });
        case "session.tool.called":
          return evidence(sessionID, session, event, {
            kind: "tool_call",
            tool: session.tools.get(data.id),
            tool_id: data.id,
            input: data.input,
          });
        case "session.tool.input.started":
          session.tools.set(data.id, data.name);
          return;
        case "session.tool.success":
        case "session.tool.failed":
          return evidence(sessionID, session, event, {
            kind: "tool_output",
            tool: session.tools.get(data.id),
            tool_id: data.id,
            text: toolText(data.content) || data.error?.message || "",
            exit: data.metadata?.exit,
            failed: event.type === "session.tool.failed",
          });
        case "session.execution.succeeded":
        case "session.execution.failed":
        case "session.execution.interrupted":
          return evidence(sessionID, session, event, {
            kind: "end",
            interrupted: event.type === "session.execution.interrupted",
          });
        case "session.step.ended":
        case "session.step.failed":
          return evidence(sessionID, session, event, {
            kind: "agent",
            input_tokens: data.tokens?.input,
            output_tokens: data.tokens?.output,
          });
      }
    }

    async function observe(event) {
      const { sessionID } = event.data;
      if (!sessionID) return;
      let session = sessions.get(sessionID);
      if (!session) {
        const info = await ctx.session.get({ sessionID });
        session = {
          directory: info.location?.directory,
          parentID: info.parentID,
          model: info.model?.id,
          running: false,
          tools: new Map(),
        };
        sessions.set(sessionID, session);
      }
      session.directory =
        event.data.location?.directory ||
        event.location?.directory ||
        session.directory;
      session.model = event.data.model?.id || session.model;
      const fields = {
        timestamp: event.created,
        event_id: event.id,
        sequence: event.durable?.seq,
      };
      await recordEvidence(event, sessionID, session);
      if (
        event.type === "session.text.ended" ||
        event.type === "session.tool.input.started"
      ) {
        return; // evidence only
      }

      switch (event.type) {
        case "session.inbox.enqueued": {
          const { item } = event.data;
          // Compaction and synthetic inbox items are not user prompts. A
          // child session's user-role instructions were supplied by an agent.
          if (item.type !== "user") return;
          if (!session.parentID) {
            fields.actor = "human";
            fields.prompt_length = Array.from(item.payload.text).length;
          }
          break;
        }
        case "session.execution.succeeded":
        case "session.execution.failed":
        case "session.execution.interrupted":
        case "session.idle":
          if (!session.running) return;
          session.running = false;
          fields.end = true;
          break;
        case "session.moved":
        case "session.model.selected":
          return; // attribution/model updates are not activity
        default:
          session.running = true;
          if (
            event.type === "session.step.ended" ||
            event.type === "session.step.failed" ||
            event.type === "session.compaction.ended"
          ) {
            fields.input_tokens = event.data.tokens?.input;
            fields.output_tokens = event.data.tokens?.output;
          }
      }
      await write(sessionID, session, fields);
    }

    const running = (async () => {
      for await (const event of ctx.event.subscribe({
        signal: controller.signal,
        types: [
          "session.inbox.enqueued",
          "session.execution.started",
          "session.execution.succeeded",
          "session.execution.failed",
          "session.execution.interrupted",
          "session.step.started",
          "session.step.ended",
          "session.step.failed",
          "session.tool.input.started",
          "session.tool.called",
          "session.tool.success",
          "session.tool.failed",
          "session.compaction.started",
          "session.compaction.ended",
          "session.compaction.failed",
          "session.idle",
          "session.text.ended",
          "session.moved",
          "session.model.selected",
        ],
      })) {
        await enqueue(() => observe(event));
      }
    })()
      .catch(() => {
        if (!controller.signal.aborted) {
          console.error(
            "strgl-collector: OpenCode event stream stopped; restart OpenCode to resume tracking",
          );
        }
      })
      .finally(() => clearInterval(timer));

    const timer = setInterval(() => {
      const timestamp = Date.now();
      void enqueue(async () => {
        for (const [sessionID, session] of sessions) {
          if (session.running) await write(sessionID, session, { timestamp });
        }
      });
    }, 30_000);
    timer.unref();

    return async () => {
      clearInterval(timer);
      controller.abort();
      await running;
      await pending;
    };
  },
};
