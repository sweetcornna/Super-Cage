// The phone's terminal queues what the owner types and sends it in chunks.
// A chunk never ends inside an escape sequence an answer to a query could
// be: the server tells the terminal's answers from keys message by message
// (termfilter), so an answer cut in two would pass its rest off as typed.

// Up to 8000 UTF-16 units stays under the 32 KiB input limit.
export const inputChunk = 8000;

// The longest answer the server takes for one (termfilter's replyLimit):
// a sequence that starts further back than this is no answer.
const answerLimit = 128;

// takeInput is the next chunk of the queued text to send.
export function takeInput(text) {
  if (text.length <= inputChunk) return text;
  let end = inputChunk;
  for (let at = text.indexOf("\x1b"); at >= 0 && at < end; ) {
    const seq = sequenceEnd(text, at);
    if (seq > end && end - at < answerLimit && at > 0) {
      end = at;
      break;
    }
    at = text.indexOf("\x1b", Math.max(at + 1, seq));
  }
  const code = text.charCodeAt(end - 1);
  if (end === inputChunk && code >= 0xd800 && code <= 0xdbff) end--;
  return text.slice(0, end);
}

// sequenceEnd measures the escape sequence at text[at] by its structure, as
// the server's input reader does; past the text when it ends first.
function sequenceEnd(text, at) {
  const past = text.length + 1;
  const kind = text[at + 1];
  if (kind === undefined) return past;
  if (kind === "[") {
    let i = at + 2;
    while (i < text.length && text.charCodeAt(i) >= 0x20 && text.charCodeAt(i) <= 0x3f) i++;
    if (i === text.length) return past;
    const final = text.charCodeAt(i);
    if (final < 0x40 || final > 0x7e) return i; // broken off
    if (final === 0x4d && i === at + 2) return at + 6 <= text.length ? at + 6 : past; // X10 mouse
    return i + 1;
  }
  if (kind === "]" || kind === "P") {
    for (let i = at + 2; i < text.length; i++) {
      const c = text.charCodeAt(i);
      if (c === 0x07 && kind === "]") return i + 1;
      if (c === 0x1b) {
        if (i + 1 === text.length) return past;
        return text[i + 1] === "\\" ? i + 2 : i;
      }
      if (c < 0x20 || c >= 0x7f) return i;
    }
    return past;
  }
  if (kind === "O") return at + 3 <= text.length ? at + 3 : past;
  return at + 2;
}
