// The production browser controller against a local Chromium-family browser
// with a throwaway profile, for browser data checks and measurements. Like
// scripts/browser-privacy-fixture.mjs, plus what a branded Chrome needs: it
// ignores --load-extension, so the built-in extension is loaded over the
// pipe before the controller opens its page (the guest's Chromium loads it
// from the command line). Set CAGE_TEST_BROWSER_PATH; TMPDIR decides where
// the profile goes.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { randomBytes } from "node:crypto";
import { cp, mkdtemp, readdir, readFile, realpath, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";

// One page per host: Set-Cookie, and seed(n) writes a cookie, localStorage,
// IndexedDB and Cache Storage of about n bytes. *.localhost hosts are secure
// contexts that resolve to loopback without a resolver rule.
export function startSites(port = 0) {
  const big = "x".repeat(64 * 1024);
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, "http://" + req.headers.host);
    if (url.pathname === "/asset.js") {
      res.setHeader("Cache-Control", "max-age=86400"); res.setHeader("Content-Type", "text/javascript");
      res.end("/*" + "a".repeat(32 * 1024) + "*/"); return;
    }
    const host = req.headers.host.split(":")[0];
    res.setHeader("Set-Cookie", ["srv=1; Path=/; Max-Age=86400", "dom=1; Domain=" + host.split(".").slice(-2).join(".") + "; Path=/; Max-Age=86400"]);
    res.setHeader("Content-Type", "text/html");
    res.end(`<!doctype html><title>${host}</title><script src="/asset.js"></script><script>
      window.seed = async n => {
        document.cookie = "js=1; max-age=86400; path=/";
        localStorage.setItem("k", "".padEnd(n, "x"));
        await new Promise((ok, bad) => { const r = indexedDB.open("db", 1); r.onupgradeneeded = () => r.result.createObjectStore("s"); r.onsuccess = () => { const tx = r.result.transaction("s", "readwrite"); tx.objectStore("s").put("".padEnd(n, "y"), "k"); tx.oncomplete = () => { r.result.close(); ok(); }; tx.onerror = bad; }; r.onerror = bad; });
        if (window.caches) await (await caches.open("c")).put("/cached", new Response("".padEnd(n, "z")));
        return true;
      };
      window.seedLocal = n => { localStorage.setItem("only", "".padEnd(n, "l")); return true; };
      window.readBack = async () => ({ local: localStorage.getItem("k")?.length || 0, only: localStorage.getItem("only")?.length || 0, cookie: document.cookie });
    </script>`);
  });
  return new Promise(resolve => server.listen(port, "127.0.0.1", () => resolve({ server, port: server.address().port })));
}

export async function createDataController({ headless = true, extension } = {}) {
  const browserPath = process.env.CAGE_TEST_BROWSER_PATH;
  assert.ok(browserPath, "Set CAGE_TEST_BROWSER_PATH to a Chromium-family browser");
  const directory = await realpath(await mkdtemp(join(tmpdir(), "cage-browser-data-")));
  const token = randomBytes(32).toString("hex");
  let agent, control, stderr = "";
  const api = async (path, body) => {
    const response = await fetch(control + path, {
      method: body === undefined ? "GET" : "POST",
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Controller request failed");
    return result;
  };
  const cdp = (id, method, params = {}) => api("/__data-test", { id, method, params });
  const evaluate = async (id, expression) => {
    const result = await cdp(id, "Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || "evaluate failed");
    return result.result.value;
  };
  const waitFor = async (read, label, timeout = 20000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (agent && agent.exitCode !== null) return null;
      try { const value = await read(); if (value) return value; } catch {}
      await delay(50);
    }
    throw new Error(`Timed out: ${label}. ${stderr.slice(-800)}`);
  };
  const stop = async () => {
    if (agent?.exitCode === null) {
      const done = once(agent, "exit"); agent.kill("SIGTERM");
      const kill = setTimeout(() => agent.kill("SIGKILL"), 5000);
      await done; clearTimeout(kill);
    }
  };
  const start = async () => {
    control = ""; stderr = "";
    agent = spawn(process.execPath, [join(directory, "browser-agent.mjs")], { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, CAGE_LANGUAGE: "zh-CN", BREAKPAD_DUMP_LOCATION: join(directory, "Crashpad") } });
    let output = "";
    agent.stdout.on("data", chunk => { output += chunk; const match = output.match(/PORT=(\d+)/); if (match) control = "http://127.0.0.1:" + match[1]; });
    agent.stderr.on("data", chunk => { stderr = (stderr + chunk).slice(-4000); });
    await waitFor(() => control, "listener");
    await waitFor(async () => (await fetch(control + "/health", { headers: { Authorization: `Bearer ${token}` } })).ok, "browser readiness");
  };
  const close = async () => { await stop(); await rm(directory, { recursive: true, force: true }); };
  try {
    const source = new URL("../../internal/cage/", import.meta.url);
    for (const name of await readdir(source))
      if ((name.startsWith("browser-") && name.endsWith(".mjs") && !name.endsWith(".test.mjs")) || ["device-observation.mjs", "display-density.mjs", "native-input.mjs", "tailnet-client.mjs"].includes(name))
        await cp(new URL(name, source), join(directory, name));
    await cp(extension || new URL("split-extension", source), join(directory, "extension"), { recursive: true });
    const text = await readFile(new URL("browser-agent.mjs", source), "utf8");
    const marker = 'const input = JSON.parse(text || "{}");';
    const launch = 'await cdp("Target.setDiscoverTargets", { discover: true });';
    for (const anchor of [marker, launch, "spawn(browserBinary, flags", '"--start-fullscreen",', 'server.listen(18500, "127.0.0.1");'])
      assert.ok(text.includes(anchor), "Update the browser data fixture: " + anchor);
    const adapted = text.replaceAll("/home/cage-browser", directory)
      .replace("/etc/claude-cage/browser-control.json", join(directory, "control.json"))
      .replace("/usr/local/lib/cage-split", join(directory, "extension"))
      .replace("spawn(browserBinary, flags", `spawn(${JSON.stringify(browserPath)}, [...flags, "--use-mock-keychain", "--password-store=basic", "--no-proxy-server"]`)
      .replace('"--start-fullscreen",', headless ? '"--headless=new", "--disable-background-networking",' : '"--start-fullscreen", "--disable-background-networking",')
      .replace(launch, 'await cdp("Extensions.loadUnpacked", { path: splitDirectory }).catch(() => {});\n  ' + launch)
      .replace('server.listen(18500, "127.0.0.1");', 'server.listen(0,"127.0.0.1",()=>console.log("PORT="+server.address().port));')
      .replace(marker, `${marker}\nif(req.url === "/__data-test") { res.end(JSON.stringify(await cdp(input.method, input.params || {}, input.id ? await session(input.id) : input.sessionId))); return; }`);
    await writeFile(join(directory, "browser-agent.mjs"), adapted);
    await writeFile(join(directory, "control.json"), JSON.stringify({ token, workspace: directory }));
    // No page of the outside world: new tabs and a fresh profile open a blank page.
    await writeFile(join(directory, "homepage.json"), JSON.stringify({ url: "about:blank" }));
    await start();
    return { api, cdp, evaluate, waitFor, directory, close, restart: async () => { await stop(); await start(); }, start, stop, agent: () => agent };
  } catch (error) { await close(); throw error; }
}
