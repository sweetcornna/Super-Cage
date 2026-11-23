import assert from "node:assert/strict";
import test from "node:test";
import { answeredBytes, quietReplayAnswers } from "./terminal-replay-answers.mjs";

test("the answered part of a replay is the part shown before", () => {
  assert.equal(answeredBytes(1000, 500, 1200), 200);
  assert.equal(answeredBytes(1000, 500, 900), 0, "shown before the replay starts");
  assert.equal(answeredBytes(1000, 500, 9000), 500, "all of it shown");
  assert.equal(answeredBytes(1000, 500, undefined), 0, "a server that does not tell: answer all");
  assert.equal(answeredBytes(null, 500, 1200), 0, "no offsets: answer all");
  assert.equal(answeredBytes(1000, 0, 1200), 0);
});

// A stand-in for xterm.js: writes run in order; "?" in a chunk is a query
// that the newest handler registered for it may take, as xterm's parser does.
function fakeTerminal() {
  const handlers = [], queue = [], answered = [];
  const register = (kind) => (id, fn) => {
    const entry = { kind, id, fn };
    handlers.push(entry);
    return { dispose: () => handlers.splice(handlers.indexOf(entry), 1) };
  };
  const terminal = {
    parser: { registerCsiHandler: register("csi"), registerDcsHandler: register("dcs"), registerOscHandler: register("osc") },
    write: (data, callback) => queue.push([data, callback]),
  };
  const parse = (data) => {
    for (const byte of data) {
      if (byte !== 0x3f) continue;
      const da1 = handlers.find((h) => h.kind === "csi" && h.id.final === "c" && !h.id.prefix);
      if (!da1.fn([0])) answered.push("DA1");
    }
  };
  const run = () => { while (queue.length) { const [data, callback] = queue.shift(); parse(data); callback?.(); } };
  return { terminal, handlers, run, answered };
}

test("queries in the answered part stay unanswered; the rest and later output are answered", () => {
  const fake = fakeTerminal();
  const write = quietReplayAnswers(fake.terminal);
  const bytes = (text) => new TextEncoder().encode(text);
  write(bytes("live ? before"));
  let done = false;
  write(bytes("a?b?|c?"), 4, () => { done = true; });
  write(bytes("live ?"));
  fake.run();
  assert.deepEqual(fake.answered, ["DA1", "DA1", "DA1"], "one before, one after the answered part, one live");
  assert.equal(done, true);
  write(bytes("??"), 2, () => {});
  fake.run();
  assert.equal(fake.answered.length, 3, "a replay answered before in full");
  write(bytes("?"));
  fake.run();
  assert.equal(fake.answered.length, 4);
  const count = fake.handlers.length;
  assert.ok(count >= 12, "device attributes, status, modes, sizes, settings and colours");
  write.dispose();
  assert.equal(fake.handlers.length, 0);
});

test("only the window size reports of CSI t and colour queries of OSC are muted", () => {
  const fake = fakeTerminal();
  const write = quietReplayAnswers(fake.terminal);
  const t = fake.handlers.find((h) => h.kind === "csi" && h.id.final === "t");
  const osc11 = fake.handlers.find((h) => h.kind === "osc" && h.id === 11);
  // Muted only while an answered part is parsed.
  assert.equal(t.fn([18]), false);
  fake.terminal.write = (data, callback) => {
    if (data.byteLength) {
      assert.equal(t.fn([18]), true);
      assert.equal(t.fn([22, 0]), false, "pushing the title is not a query");
      assert.equal(osc11.fn("?"), true);
      assert.equal(osc11.fn("rgb:ffff/ffff/ffff"), false, "setting a colour is not a query");
    }
    callback?.();
  };
  write(new Uint8Array([1]), 1);
  assert.equal(t.fn([18]), false);
  write.dispose();
});
