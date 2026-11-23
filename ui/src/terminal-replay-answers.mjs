// A replayed terminal output can hold queries (device attributes, cursor
// position, mode and colour reports) that a terminal answered when the output
// was first shown. Answered again by a page that opens later, the answers
// reach the program as typing: a shell prompt shows "1;2c" or "0;276;0c",
// a full-screen program reads keys nobody pressed. The server says up to
// which offset its output had reached a viewer (`answered` on the history
// marker and on the phone's reply); xterm.js leaves the queries in that part
// unanswered and answers every later one as before.

// answeredBytes is how many leading bytes of a replay that starts at offset
// were shown to a viewer before. A server that does not tell answers all.
export function answeredBytes(offset, length, answered) {
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(answered) || !(length > 0)) return 0;
  return Math.max(0, Math.min(length, answered - offset));
}

const reports = [14, 16, 18]; // CSI t: window and cell sizes
const empty = new Uint8Array(0);

// quietReplayAnswers(terminal) returns write(data, answered, callback): it
// writes data and leaves the queries in its first answered bytes unanswered.
// The markers are writes of their own, so output queued before or after the
// answered part is answered as usual.
export function quietReplayAnswers(terminal) {
  let quiet = 0;
  const mute = () => quiet > 0;
  const parser = terminal.parser;
  const handlers = [
    parser.registerCsiHandler({ final: "c" }, mute),
    parser.registerCsiHandler({ prefix: ">", final: "c" }, mute),
    parser.registerCsiHandler({ final: "n" }, mute),
    parser.registerCsiHandler({ prefix: "?", final: "n" }, mute),
    parser.registerCsiHandler({ intermediates: "$", final: "p" }, mute),
    parser.registerCsiHandler({ prefix: "?", intermediates: "$", final: "p" }, mute),
    parser.registerCsiHandler({ final: "t" }, (params) => mute() && reports.includes(params[0])),
    parser.registerDcsHandler({ intermediates: "$", final: "q" }, mute),
    // OSC 4, 10, 11 and 12 report a colour for "?" and set it otherwise.
    ...[4, 10, 11, 12].map((id) => parser.registerOscHandler(id, (data) => mute() && data.split(";").includes("?"))),
  ];
  const write = (data, answered = 0, callback) => {
    const quietPart = Math.max(0, Math.min(answered, data.byteLength));
    if (quietPart) {
      terminal.write(empty, () => { quiet++; });
      terminal.write(data.subarray(0, quietPart), () => { quiet--; });
    }
    if (quietPart < data.byteLength || callback) terminal.write(quietPart ? data.subarray(quietPart) : data, callback);
  };
  write.dispose = () => { for (const handler of handlers) handler.dispose(); };
  return write;
}
