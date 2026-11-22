import assert from "node:assert/strict";
import net from "node:net";
import { once } from "node:events";
import { createPrivacyController } from "./browser-privacy-fixture.mjs";

// Run in an isolated Linux X display with CAGE_TEST_BROWSER_PATH set, and with
// nothing listening on 127.0.0.1:15900. The listener below stands in for the
// workbench's own stream, which the host keeps open while its pane is hidden.
// POST /visibility {"visible":false,"held":N} tells the agent that N of the
// connections on the display port are such streams and not legacy viewers.
const fixture = await createPrivacyController({ headless: false });
const { api, evaluate, waitFor } = fixture;
let listener, held, extra;
const accepted = [];
const connect = async () => {
  const socket = net.connect(15900, "127.0.0.1");
  await once(socket, "connect");
  return socket;
};
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
try {
  const original = (await api("/tabs")).active;
  await waitFor(() => evaluate(original, "document.readyState === 'complete'"), "initial page");
  listener = net.createServer(socket => accepted.push(socket));
  await new Promise(resolve => listener.listen(15900, "127.0.0.1", resolve));
  held = await connect();

  // Without the count, a connection on the display port is a legacy viewer.
  await api("/visibility", { visible: false });
  await pause(2500);
  assert.equal(await evaluate(original, "document.hidden"), false, "a connection nobody announced must keep the window visible");

  // With it, the stream the host holds does not.
  await api("/visibility", { visible: false, held: 1 });
  await waitFor(() => evaluate(original, "document.hidden"), "hide while the host holds the stream");

  // A legacy viewer next to it restores the window, and its departure hides it again.
  extra = await connect();
  await waitFor(() => evaluate(original, "!document.hidden"), "a second connection counts as a legacy viewer");
  extra.destroy();
  await waitFor(() => evaluate(original, "document.hidden"), "hide after the legacy viewer left");

  // Once the held stream is gone the count is spent: a new connection is a legacy viewer.
  held.destroy();
  await waitFor(() => accepted.every(socket => socket.destroyed), "the stream to close");
  await pause(2500);
  extra = await connect();
  await waitFor(() => evaluate(original, "!document.hidden"), "a connection after the held stream closed counts as a legacy viewer");
  console.log(JSON.stringify({ passed: true, checks: ["unannounced connection keeps the window visible", "held stream does not", "legacy viewer next to it restores", "spent count does not mask a later viewer"] }));
} finally {
  held?.destroy(); extra?.destroy(); accepted.forEach(socket => socket.destroy()); listener?.close();
  await api("/visibility", { visible: true }).catch(() => {});
  await fixture.close();
}
