import assert from "node:assert/strict";
import test from "node:test";
import { inputChunk, takeInput } from "./terminal-input-chunks.mjs";

// The answers a terminal gives to a program's queries, as the server reads
// them (termfilter): each must reach it whole, in one message.
const answers = [
  "\x1b[12;145R", "\x1b[?12;145R", "\x1b[?62;22c", "\x1b[>0;276;0c", "\x1b[0n", "\x1b[?2004;1$y",
  "\x1b]11;rgb:ffff/ffff/ffff\x1b\\", "\x1b]10;rgb:0/0/0\x07", "\x1b]4;1;rgb:cdcd/0000/0000\x1b\\",
  "\x1bP1$r0m\x1b\\", "\x1bP0$r\x1b\\", "\x1b[I", "\x1b[<35;10;5M", "\x1b[M !!",
];

function chunks(text) {
  const out = [];
  while (text) {
    const chunk = takeInput(text);
    assert.ok(chunk.length > 0 && chunk.length <= inputChunk);
    out.push(chunk);
    text = text.slice(chunk.length);
  }
  return out;
}

test("answers queued past a chunk are never cut", () => {
  const stream = "\x1b[12;145R".repeat(5000);
  const sent = chunks(stream);
  assert.equal(sent.join(""), stream);
  for (const chunk of sent) assert.match(chunk, /^(\x1b\[12;145R)+$/);
  for (const answer of answers) {
    for (let i = 1; i < answer.length; i++) {
      const before = "x".repeat(inputChunk - i);
      assert.deepEqual(chunks(before + answer + "y"), [before, answer + "y"], JSON.stringify([answer, i]));
    }
  }
});

test("a chunk is cut at its size otherwise", () => {
  const typed = "x".repeat(inputChunk - 9) + "\x1b[12;145R" + "y".repeat(10);
  assert.equal(takeInput(typed).length, inputChunk);
  const long = "x".repeat(inputChunk - 200) + "\x1b]" + "a".repeat(1000);
  assert.equal(takeInput(long).length, inputChunk);
  const emoji = "a".repeat(inputChunk - 1) + "\u{1f600}";
  assert.equal(takeInput(emoji).length, inputChunk - 1);
  assert.equal(takeInput("short\x1b["), "short\x1b[");
});
