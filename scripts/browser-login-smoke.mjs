// Exercise the guest login routes against a local HTTPS mock with real Chromium.
// Set CAGE_TEST_BROWSER_PATH; no account, external endpoint or user profile is used.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { X509Certificate, createHash } from "node:crypto";
import { once } from "node:events";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import https from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createPrivacyController } from "./browser-privacy-fixture.mjs";

const host = "cage-login-fixture.test", origin = `https://${host}`;
const profile = await mkdtemp(join(tmpdir(), "cage-login-tls-"));
const login = "a".repeat(24);
const request = { login, url: `${origin}/start?state=fixture-only`, allowed_hosts: [host] };
const key = join(profile, "key.pem"), cert = join(profile, "cert.pem");
let fixture, site, opens = 0;
try {
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
    "-subj", `/CN=${host}`, "-keyout", key, "-out", cert], { stdio: "ignore" });
  const tls = { key: await readFile(key), cert: await readFile(cert) };
  // Only the fixture's ephemeral certificate is trusted; no global TLS bypass.
  const pin = createHash("sha256").update(new X509Certificate(tls.cert).publicKey.export({ type: "spki", format: "der" })).digest("base64");
  site = https.createServer(tls, (req, res) => {
    const path = new URL(req.url, origin).pathname;
    if (path === "/drop") { req.socket.destroy(); return; }
    if (path === "/start") {
      opens++;
      res.writeHead(302, { Location: "/continue?stage=fixture-only", "Set-Cookie": "fixture=present; Secure; Path=/" });
      res.end();
      return;
    }
    res.setHeader("Content-Type", "text/html");
    if (path === "/unavailable") res.writeHead(503);
    res.end(`<!doctype html><title>Fixture ${path}</title><script>globalThis.webdriverAtLoad = navigator.webdriver</script>`);
  });
  site.listen(0, "127.0.0.1");
  await once(site, "listening");
  fixture = await createPrivacyController({ browserArgs: ["--no-proxy-server",
    `--host-resolver-rules=MAP ${host} 127.0.0.1:${site.address().port}, EXCLUDE localhost, MAP * ~NOTFOUND`,
    `--ignore-certificate-errors-spki-list=${pin}`,
  ] });
  const { api, cdp, evaluate, waitFor } = fixture;
  const first = await api("/login-open", request);
  await waitFor(() => evaluate(first.id, "location.pathname === '/continue' && typeof webdriverAtLoad === 'boolean'"), "mock redirect");
  assert.deepEqual(await evaluate(first.id, "[navigator.webdriver, webdriverAtLoad, document.cookie]"), [true, true, "fixture=present"]);
  const tab = (await api("/tabs")).tabs.find(tab => tab.id === first.id);
  assert.equal(tab.login, true);
  assert.equal(tab.url, origin);
  assert.equal(tab.title, "账号授权");

  await evaluate(first.id, "history.replaceState({}, '', '/in-progress#fixture-only'); sessionStorage.fixture = 'preserved'");
  assert.deepEqual(await api("/login-open", request), first);
  assert.deepEqual(await evaluate(first.id, "[location.pathname, document.cookie, sessionStorage.fixture]"), ["/in-progress", "fixture=present", "preserved"]);
  assert.equal(opens, 1, "reopen must not reissue the request");

  await cdp(first.id, "Runtime.evaluate", { expression: "void window.open('/popup?fixture-only')", userGesture: true });
  const popup = await waitFor(async () => {
    const { targetInfos } = await cdp(undefined, "Target.getTargets");
    return targetInfos.find(target => target.openerId === first.id && target.url.startsWith(`${origin}/popup?`));
  }, "login popup");
  await api("/activate", { id: popup.targetId });
  assert.deepEqual(await api("/login-open", request), { id: popup.targetId });
  assert.equal(await evaluate(popup.targetId, "navigator.webdriver && document.cookie === 'fixture=present'"), true);
  assert.equal((await api("/tabs")).tabs.find(tab => tab.id === popup.targetId).url, origin);
  assert.equal(opens, 1, "reopening the popup must not reissue the request");

  await assert.rejects(api("/login-open", { ...request, url: `${origin}/drop?state=fixture-only` }),
    { message: "授权页导航失败" });
  assert.ok(!(await api("/tabs")).tabs.some(tab => tab.login), "failed navigation must release the login context");
  const retried = await api("/login-open", request);
  assert.notEqual(retried.id, first.id);
  assert.equal(opens, 2);
  assert.equal(await evaluate(retried.id, "sessionStorage.fixture === undefined"), true);

  const unavailable = await api("/login-open", { ...request, url: `${origin}/unavailable` });
  await waitFor(() => evaluate(unavailable.id, "document.title === 'Fixture /unavailable'"), "mock HTTP 503 page remains visible");
  assert.deepEqual(await api("/login-finish", { login }), { ok: true });
  assert.ok(!(await api("/tabs")).tabs.some(tab => tab.login));

  const interrupted = await api("/login-open", request);
  await fixture.stop();
  await assert.rejects(api("/login-open", request), TypeError);
  await fixture.restart();
  const reconnected = await api("/login-open", request);
  assert.notEqual(reconnected.id, interrupted.id);
  await api("/login-finish", { login });
  console.log("PASS: isolated Chromium login, native webdriver, redaction, reopen page and popup without navigation, network failure and retry, HTTP error page, finish and controller reconnect");
} finally {
  await fixture?.close();
  if (site) await new Promise(resolve => site.close(resolve));
  await rm(profile, { recursive: true, force: true });
}
