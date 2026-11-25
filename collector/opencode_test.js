// SPDX-License-Identifier: AGPL-3.0-only

import assert from "node:assert/strict";
import { mkdtemp, readFile, readdir, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setImmediate } from "node:timers/promises";
import test from "node:test";
import plugin from "./opencode.js";

const start = Date.parse("2026-11-21T04:00:00Z");

function barrier() {
  let resolve;
  const promise = new Promise((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function event(type, sessionID, sequence, seconds, data = {}) {
  return {
    id: `evt_${sessionID}_${sequence}`,
    created: start + seconds * 1000,
    type,
    durable: { aggregateID: sessionID, seq: sequence, version: "1" },
    location: { directory: "/project" },
    data: { sessionID, ...data },
  };
}

async function spool(t) {
  const directory = await mkdtemp(join(tmpdir(), "strgl-opencode-test-"));
  const previous = process.env.STRGL_OPENCODE_DIR;
  process.env.STRGL_OPENCODE_DIR = directory;
  t.after(async () => {
    if (previous === undefined) delete process.env.STRGL_OPENCODE_DIR;
    else process.env.STRGL_OPENCODE_DIR = previous;
    await rm(directory, { recursive: true, force: true });
  });
  return directory;
}

test("OpenCode records metadata, not compaction or child instructions as human prompts", async (t) => {
  const directory = await spool(t);
  const drained = barrier();
  const events = [
    event("session.inbox.enqueued", "ses_parent", 2, 0, {
      item: {
        type: "user",
        payload: { text: "fix 🦀", files: [{ uri: "file:///private-file" }] },
        delivery: "steer",
      },
    }),
    event("session.execution.started", "ses_parent", 3, 1),
    event("session.inbox.enqueued", "ses_parent", 4, 2, {
      item: { type: "compaction" },
    }),
    event("session.compaction.started", "ses_parent", 5, 3),
    event("session.compaction.ended", "ses_parent", 6, 4, {
      text: "secret-token-123 in summary",
      tokens: { input: 103, output: 17 },
    }),
    event("session.tool.called", "ses_parent", 7, 5, {
      input: { command: "echo secret-token-123" },
    }),
    event("session.inbox.enqueued", "ses_parent", 8, 6, {
      item: { type: "synthetic", payload: { text: "secret-token-123" } },
    }),
    event("session.execution.succeeded", "ses_parent", 9, 7),
    event("session.idle", "ses_parent", 10, 7),
    event("session.inbox.enqueued", "ses_child", 2, 8, {
      item: { type: "user", payload: { text: "secret-token-123 from parent" } },
    }),
    event("session.execution.started", "ses_child", 3, 9),
    event("session.step.failed", "ses_child", 4, 10, {
      tokens: { input: 211, output: 37 },
    }),
    event("session.execution.interrupted", "ses_child", 5, 11, {
      error: { message: "secret-token-123" },
    }),
  ];
  const cleanup = await plugin.setup({
    app: { version: "2.3.2" },
    session: {
      async get({ sessionID }) {
        return {
          parentID: sessionID === "ses_child" ? "ses_parent" : undefined,
          model: { id: "custom-local-model" },
          location: { directory: "/project" },
        };
      },
    },
    event: {
      async *subscribe({ types }) {
        for (const record of events) {
          if (types.includes(record.type)) yield record;
        }
        drained.resolve();
      },
    },
  });
  t.after(cleanup);
  await drained.promise;
  await cleanup();
  const files = (await readdir(directory)).filter((file) =>
    file.endsWith(".jsonl"),
  );
  const content = (
    await Promise.all(
      files.map((file) => readFile(join(directory, file), "utf8")),
    )
  ).join("");
  const records = content
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.equal(records.length, 10);
  assert.deepEqual(
    records
      .filter((record) => record.actor === "human")
      .map((record) => ({
        session: record.session_id,
        length: record.prompt_length,
      })),
    [{ session: "ses_parent", length: 5 }],
  );
  assert.deepEqual(
    records
      .filter((record) => record.end)
      .map((record) => record.session_id)
      .sort(),
    ["ses_child", "ses_parent"],
  );
  assert(
    records.every(
      (record) =>
        record.model === "custom-local-model" &&
        record.version === "2.3.2" &&
        record.cwd === "/project",
    ),
  );
  assert(
    records.some(
      (record) => record.input_tokens === 103 && record.output_tokens === 17,
    ),
  );
  assert(
    records.some(
      (record) => record.input_tokens === 211 && record.output_tokens === 37,
    ),
  );
  assert(!/secret-token|private-file|fix 🦀/.test(content));
  if (process.platform !== "win32") {
    assert.equal((await stat(join(directory, files[0]))).mode & 0o777, 0o600);
  }

  // The separate local evidence spool keeps text for the encrypted store.
  const evidenceDirectory = join(directory, "evidence");
  const evidence = (
    await Promise.all(
      (await readdir(evidenceDirectory)).map((file) =>
        readFile(join(evidenceDirectory, file), "utf8"),
      ),
    )
  )
    .join("")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.deepEqual(
    evidence
      .filter((record) => record.kind === "prompt")
      .map((record) => record.text),
    ["fix 🦀"],
  );
  const child = evidence.find(
    (record) => record.session_id === "ses_child" && record.text,
  );
  assert.equal(child.kind, "response");
  assert.equal(child.parent_id, "ses_parent");
  assert.equal(
    evidence.find((record) => record.kind === "tool_call").input.command,
    "echo secret-token-123",
  );
  assert(!evidence.some((record) => record.text === "secret-token-123"));
  assert.deepEqual(
    evidence
      .filter((record) => record.kind === "end")
      .map((record) => [record.session_id, record.interrupted])
      .sort(),
    [
      ["ses_child", true],
      ["ses_parent", false],
    ],
  );
});

test("OpenCode samples quiet active runs, stops at idle and stops if its event stream closes", async (t) => {
  const directory = await spool(t);
  t.mock.timers.enable({ apis: ["setInterval", "Date"], now: start });
  const active = barrier();
  const stop = barrier();
  const idle = barrier();
  const finish = barrier();
  const drained = barrier();
  const cleanup = await plugin.setup({
    app: { version: "2.3.2" },
    session: {
      async get() {
        return { location: { directory: "/project" } };
      },
    },
    event: {
      async *subscribe() {
        yield event("session.inbox.enqueued", "ses_parent", 2, 0, {
          item: { type: "user", payload: { text: "go" } },
        });
        yield event("session.execution.started", "ses_parent", 3, 1);
        active.resolve();
        await stop.promise;
        yield event("session.execution.failed", "ses_parent", 4, 31);
        idle.resolve();
        await finish.promise;
        yield event("session.execution.started", "ses_child", 2, 92);
        drained.resolve(); // disconnect while this session is still active
      },
    },
  });
  t.after(cleanup);
  await active.promise;
  t.mock.timers.tick(30_000);
  t.mock.timers.tick(1_000);
  stop.resolve();
  await idle.promise;
  t.mock.timers.tick(61_000);
  finish.resolve();
  await drained.promise;
  await setImmediate();
  t.mock.timers.tick(60_000);
  await cleanup();
  const parent = (await readFile(join(directory, "ses_parent.jsonl"), "utf8"))
    .trim()
    .split("\n")
    .map(JSON.parse);
  const child = (await readFile(join(directory, "ses_child.jsonl"), "utf8"))
    .trim()
    .split("\n")
    .map(JSON.parse);
  assert.deepEqual(
    parent.map((record) => ({
      actor: record.actor,
      seconds: (record.timestamp - start) / 1000,
      end: Boolean(record.end),
    })),
    [
      { actor: "human", seconds: 0, end: false },
      { actor: "agent", seconds: 1, end: false },
      { actor: "agent", seconds: 30, end: false },
      { actor: "agent", seconds: 31, end: true },
    ],
  );
  assert.equal(
    child.length,
    1,
    "the disconnected event stream must not keep manufacturing activity",
  );
});
