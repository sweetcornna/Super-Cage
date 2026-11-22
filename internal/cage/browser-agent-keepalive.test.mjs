// Runs the real browser agent against a headless Chromium and checks that the
// connection the host keeps open between requests survives a pause. The host
// reaches the agent through one SSH channel per HTTP connection, so a
// connection the agent closes after a few seconds costs a handshake to reopen.
//
//   CAGE_TEST_BROWSER_PATH=/path/to/chrome node --test internal/cage/browser-agent-keepalive.test.mjs
//
// Set CAGE_AGENT_SOURCE to run it against another copy of the agent.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { randomBytes } from "node:crypto";
import { cp, mkdir, mkdtemp, readdir, readFile, realpath, rm, writeFile } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

const browserPath = process.env.CAGE_TEST_BROWSER_PATH;
const sourceDirectory = new URL("./", import.meta.url);

async function startAgent() {
  const directory = await realpath(await mkdtemp(join(tmpdir(), "cage-keepalive-")));
  const token = randomBytes(32).toString("hex");
  for (const name of await readdir(sourceDirectory))
    if ((name.startsWith("browser-") && name.endsWith(".mjs") && !name.endsWith(".test.mjs")) || ["device-observation.mjs", "display-density.mjs", "native-input.mjs", "tailnet-client.mjs", "browser-extension-archive.py"].includes(name))
      await cp(new URL(name, sourceDirectory), join(directory, name));
  await cp(new URL("split-extension", sourceDirectory), join(directory, "extension"), { recursive: true });
  const original = process.env.CAGE_AGENT_SOURCE ? await readFile(process.env.CAGE_AGENT_SOURCE, "utf8") : await readFile(new URL("browser-agent.mjs", sourceDirectory), "utf8");
  const listen = 'server.listen(18500, "127.0.0.1");';
  assert.ok(original.includes(listen) && original.includes("spawn(browserBinary, flags") && original.includes('"--start-fullscreen",'), "update the isolated launch adapter");
  const adapted = original.replaceAll("/home/cage-browser", directory)
    .replace("/etc/claude-cage/browser-control.json", join(directory, "control.json"))
    .replace("/usr/local/lib/cage-split", join(directory, "extension"))
    .replace("spawn(browserBinary, flags", `spawn(${JSON.stringify(browserPath)}, [...flags, "--use-mock-keychain", "--password-store=basic"]`)
    .replace('"--start-fullscreen",', '"--headless=new", "--disable-background-networking",')
    .replace(listen, 'server.listen(0, "127.0.0.1", () => console.log("PORT=" + server.address().port));');
  await mkdir(join(directory, "profile"), { recursive: true });
  await writeFile(join(directory, "browser-agent.mjs"), adapted);
  await writeFile(join(directory, "control.json"), JSON.stringify({ token, workspace: directory }));
  await writeFile(join(directory, "homepage.json"), JSON.stringify({ url: "about:blank" }));
  const agent = spawn(process.execPath, [join(directory, "browser-agent.mjs")], { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, BREAKPAD_DUMP_LOCATION: join(directory, "Crashpad") } });
  let output = "", stderr = "", port = 0;
  agent.stdout.on("data", chunk => { output += chunk; port = Number(output.match(/PORT=(\d+)/)?.[1] ?? 0); });
  agent.stderr.on("data", chunk => { stderr = (stderr + chunk).slice(-4000); });
  const stop = async () => {
    if (agent.exitCode === null) {
      const done = once(agent, "exit");
      agent.kill("SIGTERM");
      const kill = setTimeout(() => agent.kill("SIGKILL"), 5000);
      await done;
      clearTimeout(kill);
    }
    await rm(directory, { recursive: true, force: true });
  };
  try {
    for (const deadline = Date.now() + 30000; ; await delay(100)) {
      if (agent.exitCode !== null) throw new Error("the agent exited: " + stderr);
      if (port) {
        const ready = await fetch(`http://127.0.0.1:${port}/health`, { headers: { Authorization: "Bearer " + token } }).then(response => response.ok, () => false);
        if (ready) break;
      }
      if (Date.now() > deadline) throw new Error("the agent did not become ready: " + stderr);
    }
  } catch (error) {
    await stop();
    throw error;
  }
  return { port, token, stop };
}

// One HTTP/1.1 request on an open socket; resolves with the status line.
function get(socket, token, path) {
  return new Promise((resolve, reject) => {
    let data = "";
    const finish = (error, value) => { socket.off("data", onData); socket.off("close", onClose); error ? reject(error) : resolve(value); };
    const onClose = () => finish(new Error("the agent closed the connection"));
    const onData = chunk => {
      data += chunk;
      const end = data.indexOf("\r\n\r\n");
      const length = Number(data.match(/content-length: (\d+)/i)?.[1] ?? 0);
      if (end >= 0 && data.length >= end + 4 + length) finish(null, data.split("\r\n")[0]);
    };
    socket.on("data", onData);
    socket.on("close", onClose);
    socket.write(`GET ${path} HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer ${token}\r\nConnection: keep-alive\r\n\r\n`);
  });
}

test("a connection the host keeps open survives a pause longer than Node's default", { skip: !browserPath && "set CAGE_TEST_BROWSER_PATH to a Chromium", timeout: 90000 }, async () => {
  const agent = await startAgent();
  try {
    const socket = net.connect(agent.port, "127.0.0.1");
    await once(socket, "connect");
    assert.match(await get(socket, agent.token, "/health"), /200/);
    // The default closes an idle connection after 5 s, plus a second's buffer.
    await delay(7500);
    assert.equal(socket.destroyed, false, "the agent closed the idle connection");
    assert.match(await get(socket, agent.token, "/health"), /200/);
    socket.destroy();
  } finally {
    await agent.stop();
  }
});
