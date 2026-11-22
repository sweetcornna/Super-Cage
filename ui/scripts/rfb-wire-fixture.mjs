import http from "node:http";
import { createHash } from "node:crypto";

// A bounded RFB display over real browser WebSockets. Playwright's WebSocket
// replacement lacks the prototype properties checked by the shipped noVNC.
// displayControl: the server of a held display stream. It selects the
// cage-display.v1 subprotocol when offered, reads visibility messages from
// text frames (stats.control), sends frames only when asked (stats.requests),
// shows an empty desktop for emptyFor ms after the pane is shown again, and
// closes a stream hidden for holdFor ms with code 4001.
// echoBinary: answer a client that offers the "binary" subprotocol with it,
// as websockify does. Off, the server selects none, as Cage servers before
// cage-display.v1 do; a browser then refuses a client that offered one.
export async function startDisplay(upstream, { frameInterval = 50, sharedClipboard = false, displayControl = false, holdFor = 60_000, emptyFor = 0, frame: frameSource, echoBinary = false, size = { width: 640, height: 480 } } = {}) {
  const stats = { connections: 0, closed: 0, frames: 0, clipboardConnections: 0, clipboardUpdates: [], keys: [], control: [], requests: [], protocols: [], closeCodes: [], pointers: [] };
  const sockets = new Set();
  const displays = new Set();
  const clipboardWriters = new Set();
  const sharedWriters = new Set(), sharedSockets = new Set();
  const terminalWriters = new Set();
  let width = size.width, height = size.height, framesPaused = false;
  // replay: whole FramebufferUpdate messages sent in turn, one per request.
  let replay = null, replayIndex = 0;
  const server = http.createServer((req, res) => {
    if (req.url.startsWith("/api/")) {
      res.writeHead(503); res.end("Unmocked fixture request"); return;
    }
    const target = new URL(req.url, upstream);
    const proxy = http.get(target, (response) => {
      res.writeHead(response.statusCode, response.headers);
      response.pipe(res);
    });
    proxy.on("error", () => { res.writeHead(502); res.end(); });
  });
  server.on("upgrade", (req, socket) => {
    const shared = sharedClipboard && new URL(req.url,upstream).pathname === "/api/clipboard";
    const terminal = sharedClipboard && /^\/api\/terminals\/[^/]+\/stream$/.test(new URL(req.url, upstream).pathname);
    if (!shared && !terminal && !["/api/vnc", "/api/claude-desktop/vnc"].includes(new URL(req.url, upstream).pathname)) { socket.destroy(); return; }
    sockets.add(socket);
    stats.connections++;
    const key = createHash("sha1").update(req.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
    const offered = String(req.headers["sec-websocket-protocol"] || "").split(",").map(value => value.trim()).filter(Boolean);
    const control = displayControl && !shared && !terminal && offered.includes("cage-display.v1");
    const selected = control ? "cage-display.v1" : echoBinary && offered.includes("binary") ? "binary" : "";
    stats.protocols.push(offered);
    const path = new URL(req.url, upstream).pathname;
    (stats.paths ||= {})[path] = (stats.paths[path] || 0) + 1;
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${key}\r\n${selected ? `Sec-WebSocket-Protocol: ${selected}\r\n` : ""}\r\n`);
    const send = (payload, opcode = 2) => {
      if (socket.destroyed) return;
      const length = payload.length;
      const header = Buffer.alloc(length < 126 ? 2 : length < 65536 ? 4 : 10);
      header[0] = 0x80 | opcode;
      header[1] = length < 126 ? length : length < 65536 ? 126 : 127;
      if (header.length === 4) header.writeUInt16BE(length, 2);
      if (header.length === 10) header.writeBigUInt64BE(BigInt(length), 2);
      socket.write(Buffer.concat([header, payload]));
    };
    const sharedWrite = value => send(Buffer.from(JSON.stringify(value)),1);
    const terminalWrite = text => send(Buffer.from(text));
    if (terminal) terminalWriters.add(terminalWrite);
    if (shared) {
      stats.clipboardConnections++; sharedWriters.add(sharedWrite); sharedSockets.add(socket);
      sharedWrite({ hello: { version: 1, os: "linux", session: "fixture-session", formats: ["text", "png", "files"], available: true } });
    }
    const clipboard = (text) => {
      const bytes = Buffer.from(text, "latin1"), message = Buffer.alloc(8 + bytes.length);
      message[0] = 3;
      message.writeUInt32BE(bytes.length, 4);
      bytes.copy(message, 8);
      send(message);
    };
    let wire = Buffer.alloc(0), rfb = Buffer.alloc(0), phase = 0, timer, frame;
    // Held-stream state: outstanding update requests, the hide timer and the
    // moment the guest window is back after a show.
    let asked = 0, hidden = false, holdTimer, emptyUntil = 0;
    const makeFrame = () => {
      frame = Buffer.alloc(16 + width * height * 4);
      frame.writeUInt16BE(1, 2); frame.writeUInt16BE(width, 8); frame.writeUInt16BE(height, 10);
    };
    // A guest window: two halves of different colours. An empty desktop: one colour.
    const paint = (count) => {
      if (frameSource) return frameSource(frame, { width, height, count, empty: Date.now() < emptyUntil });
      if (!control) return void frame.fill(count % 255, 16);
      const empty = Date.now() < emptyUntil;
      for (let y = 0; y < height; y++) {
        const row = 16 + y * width * 4;
        frame.fill(empty ? 0 : 40 + count % 200, row, row + width * 2);
        frame.fill(empty ? 0 : 230, row + width * 2, row + width * 4);
      }
    };
    const closeHeld = () => {
      stats.closeCodes.push(4001);
      const payload = Buffer.alloc(2 + 14);
      payload.writeUInt16BE(4001, 0);
      payload.write("hidden-timeout", 2);
      send(payload, 8);
      socket.end();
    };
    const resize = () => {
      const update = Buffer.alloc(16);
      update.writeUInt16BE(1, 2); update.writeUInt16BE(width, 8); update.writeUInt16BE(height, 10);
      update.writeInt32BE(-223, 12); // RFB DesktopSize pseudo-encoding.
      send(update);
      makeFrame();
    };
    const consume = (data) => {
      rfb = Buffer.concat([rfb, data]);
      if (phase === 0 && rfb.length >= 12) { rfb = rfb.subarray(12); phase = 1; send(Buffer.from([1, 1])); }
      if (phase === 1 && rfb.length >= 1) { rfb = rfb.subarray(1); phase = 2; send(Buffer.alloc(4)); }
      if (phase === 2 && rfb.length >= 1) {
        rfb = rfb.subarray(1);
        phase = 3;
        const name = Buffer.from("Cage test display");
        const init = Buffer.alloc(24 + name.length);
        init.writeUInt16BE(width, 0); init.writeUInt16BE(height, 2);
        init.set([32, 24, 0, 1], 4);
        for (const offset of [8, 10, 12]) init.writeUInt16BE(255, offset);
        init.set([16, 8, 0], 14);
        init.writeUInt32BE(name.length, 20); name.copy(init, 24); send(init);
        const fence = Buffer.alloc(9);
        fence[0] = 248;
        fence.writeUInt32BE(0x80000000, 4);
        send(fence);
        makeFrame();
        displays.add(resize);
        clipboardWriters.add(clipboard);
        timer = setInterval(() => {
          if (framesPaused || socket.writableLength > frame.length * 2) return;
          // A held stream's server answers requests; the legacy one pushes.
          if (control || replay) { if (!asked) return; asked--; }
          if (replay) {
            const message = replay[replayIndex++ % replay.length];
            stats.replayBytes = (stats.replayBytes || 0) + message.length;
            send(message); stats.frames++;
            return;
          }
          paint(stats.frames);
          send(frame); stats.frames++;
        }, frameInterval);
      }
      while (phase === 3 && rfb.length) {
        const type = rfb[0];
        let length = { 0: 20, 3: 10, 4: 8, 5: 6, 150: 10 }[type];
        if (type === 2) {
          if (rfb.length < 4) return;
          length = 4 + 4 * rfb.readUInt16BE(2);
        } else if (type === 6) {
          if (rfb.length < 8) return;
          length = 8 + Math.abs(rfb.readInt32BE(4));
        } else if (type === 248) {
          if (rfb.length < 9) return;
          length = 9 + rfb[8];
        }
        if (!length) { socket.destroy(); return; }
        if (rfb.length < length) return;
        if (type === 5) { stats.pointer = { x: rfb.readUInt16BE(2), y: rfb.readUInt16BE(4) }; stats.pointers.push({ ...stats.pointer, at: Date.now() }); }
        if (type === 3) { stats.requests.push({ incremental: Boolean(rfb[1]), at: Date.now() }); if (control || replay) asked = 1; }
        if (type === 4) stats.keys.push({ down: Boolean(rfb[1]), symbol: rfb.readUInt32BE(4) });
        if (type === 248 && (rfb.readUInt32BE(4) & 0x80000000)) {
          const response = Buffer.from(rfb.subarray(0, length));
          response.writeUInt32BE(rfb.readUInt32BE(4) & 3, 4);
          send(response);
        }
        rfb = rfb.subarray(length);
      }
    };
    socket.on("data", (data) => {
      wire = Buffer.concat([wire, data]);
      while (wire.length >= 2) {
        const opcode = wire[0] & 15, masked = Boolean(wire[1] & 128);
        let length = wire[1] & 127, offset = 2;
        if (length === 126) { if (wire.length < 4) return; length = wire.readUInt16BE(2); offset = 4; }
        if (length === 127) { if (wire.length < 10) return; length = Number(wire.readBigUInt64BE(2)); offset = 10; }
        if (length > 65536) { socket.destroy(); return; }
        const maskBytes = masked ? 4 : 0;
        if (wire.length < offset + maskBytes + length) return;
        const payload = Buffer.from(wire.subarray(offset + maskBytes, offset + maskBytes + length));
        if (masked) for (let i = 0; i < length; i++) payload[i] ^= wire[offset + (i % 4)];
        wire = wire.subarray(offset + maskBytes + length);
        if (opcode === 8) { send(payload, 8); socket.end(); return; }
        if (opcode === 9) send(payload, 10);
        if (control && opcode === 1) {
          const value = JSON.parse(payload.toString());
          stats.control.push({ ...value, path: new URL(req.url, upstream).pathname, at: Date.now() });
          if (value.type === "visibility" && value.visible === false && !hidden) {
            hidden = true;
            holdTimer = setTimeout(closeHeld, holdFor);
          } else if (value.type === "visibility" && value.visible === true) {
            hidden = false;
            clearTimeout(holdTimer);
            emptyUntil = Date.now() + emptyFor;
          }
        } else if (shared && opcode === 1) {
          const value = JSON.parse(payload.toString());
          stats.clipboardUpdates.push(value);
          sharedWrite({ ack: value.serial });
        } else if (!terminal && (opcode === 1 || opcode === 2)) consume(payload);
      }
    });
    socket.on("error", () => {});
    socket.on("close", () => { clearInterval(timer); clearTimeout(holdTimer); displays.delete(resize); clipboardWriters.delete(clipboard); sharedWriters.delete(sharedWrite); terminalWriters.delete(terminalWrite); sharedSockets.delete(socket); sockets.delete(socket); stats.closed++; });
    if (terminal) terminalWrite("Terminal clipboard ready\r\n$ ");
    else if (!shared) send(Buffer.from("RFB 003.008\n"));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    stats,
    pauseFrames(paused) { framesPaused = paused; },
    // Recorded-style updates for a measurement: an array of FramebufferUpdate
    // messages (see tightJpegUpdate). Requests are answered from it in turn.
    replay(messages) { replay = messages; replayIndex = 0; },
    clipboard(text) { for (const write of clipboardWriters) write(text); },
    sharedClipboard(value) { for (const write of sharedWriters) write(value); },
    terminalOutput(text) { for (const write of terminalWriters) write(text); },
    disconnectClipboard() { for (const socket of sharedSockets) socket.destroy(); },
    resize(size) { width = size.width; height = size.height; for (const resize of displays) resize(); },
    close: async () => {
      for (const socket of sockets) socket.destroy();
      server.closeAllConnections();
      await new Promise((resolve) => server.close(resolve));
    },
  };
}

// One FramebufferUpdate with a Tight JPEG rectangle, as Xtigervnc sends a
// scrolled page at quality 8.
export function tightJpegUpdate(jpeg, { x = 0, y = 0, width, height }) {
  const length = jpeg.length;
  const compact = length < 128 ? [length] : length < 16384 ? [(length & 127) | 128, length >> 7]
    : [(length & 127) | 128, ((length >> 7) & 127) | 128, length >> 14];
  const header = Buffer.alloc(4 + 12 + 1 + compact.length);
  header.writeUInt16BE(1, 2); // one rectangle
  header.writeUInt16BE(x, 4); header.writeUInt16BE(y, 6);
  header.writeUInt16BE(width, 8); header.writeUInt16BE(height, 10);
  header.writeInt32BE(7, 12); // Tight
  header[16] = 0x90; // JPEG
  Buffer.from(compact).copy(header, 17);
  return Buffer.concat([header, jpeg]);
}
