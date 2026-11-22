import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { isScienceURL, publicScienceURL, readScienceConfig, scienceLink, scienceSignInWindow, scienceTabReusable } from "./browser-science.mjs";

const port = 47800;
const nonce = "0123456789abcdef0123456789abcdef";
const good = `http://localhost:${port}/?nonce=${nonce}`;

test("the link the daemon prints is accepted unchanged", () => {
  assert.equal(scienceLink(good, port), good);
  assert.equal(scienceLink(`http://localhost:${port}/?nonce=AZaz09_-AZaz09_-`, port), `http://localhost:${port}/?nonce=AZaz09_-AZaz09_-`);
  // Printed when another program holds [::1] on the port; still guest loopback.
  assert.equal(scienceLink(`http://127.0.0.1:${port}/?nonce=${nonce}`, port), `http://127.0.0.1:${port}/?nonce=${nonce}`);
});

test("only the provisioned loopback port and host are accepted", () => {
  const refused = [
    // wrong or non-loopback host
    `http://[::1]:${port}/?nonce=${nonce}`,
    `http://127.0.0.2:${port}/?nonce=${nonce}`,
    `http://127.1:${port}/?nonce=${nonce}`,
    `http://2130706433:${port}/?nonce=${nonce}`,
    `http://0.0.0.0:${port}/?nonce=${nonce}`,
    `http://10.0.2.2:${port}/?nonce=${nonce}`,
    `http://example.com:${port}/?nonce=${nonce}`,
    `http://localhost.example.com:${port}/?nonce=${nonce}`,
    `http://localhost.:${port}/?nonce=${nonce}`,
    `http://LOCALHOST:${port}/?nonce=${nonce}`,
    `http://claude.ai/?nonce=${nonce}`,
    // wrong port
    `http://localhost:47801/?nonce=${nonce}`,
    `http://localhost:8000/?nonce=${nonce}`,
    `http://localhost:047800/?nonce=${nonce}`,
    `http://localhost/?nonce=${nonce}`,
    `http://localhost:80/?nonce=${nonce}`,
    // extra or missing query keys, other paths, fragments
    `http://localhost:${port}/?nonce=${nonce}&dest=/`,
    `http://localhost:${port}/?nonce=${nonce}&nonce=${nonce}`,
    `http://localhost:${port}/?dest=%2F&nonce=${nonce}`,
    `http://localhost:${port}/`,
    `http://localhost:${port}/?nonce=`,
    `http://localhost:${port}/api/auth/login?nonce=${nonce}`,
    `http://localhost:${port}/./?nonce=${nonce}`,
    `http://localhost:${port}/?nonce=${nonce}#x`,
    `http://localhost:${port}?nonce=${nonce}`,
    // scheme and parser tricks
    `https://localhost:${port}/?nonce=${nonce}`,
    `HTTP://localhost:${port}/?nonce=${nonce}`,
    `ws://localhost:${port}/?nonce=${nonce}`,
    `javascript:alert(1)//http://localhost:${port}/?nonce=${nonce}`,
    `data:text/html,http://localhost:${port}/?nonce=${nonce}`,
    `file:///etc/passwd?nonce=${nonce}`,
    `//localhost:${port}/?nonce=${nonce}`,
    `http:/localhost:${port}/?nonce=${nonce}`,
    `http://user@localhost:${port}/?nonce=${nonce}`,
    `http://localhost:${port}@evil.example/?nonce=${nonce}`,
    `http://evil.example#@localhost:${port}/?nonce=${nonce}`,
    `http://localhost:${port}\\@evil.example/?nonce=${nonce}`,
    `http://localhost:${port}/?nonce=${nonce}\n`,
    ` ${good}`,
    `http://local\thost:${port}/?nonce=${nonce}`,
    `http://localhost:${port}/?nonce=${nonce}%0d%0a`,
    // nonce shape
    `http://localhost:${port}/?nonce=short`,
    `http://localhost:${port}/?nonce=${"a".repeat(129)}`,
    `http://localhost:${port}/?nonce=${nonce.slice(0, 20)}%20${nonce.slice(20)}`,
    `http://localhost:${port}/?nonce=${nonce}<script>`,
    `http://localhost:${port}/?nonce=${nonce.slice(0, 20)}.${nonce.slice(20)}`,
  ];
  for (const value of refused) assert.throws(() => scienceLink(value, port), /登录链接无效/, value);
  for (const value of [undefined, null, 47800, {}, [good]]) assert.throws(() => scienceLink(value, port), /登录链接无效/);
});

test("without a provisioned port nothing is accepted", () => {
  for (const value of [undefined, null, "47800", 47800.5, NaN]) assert.throws(() => scienceLink(good, value), /尚未安装或配置/);
});

test("the port comes from the provisioned file, read on use", () => {
  const dir = mkdtempSync(join(tmpdir(), "science-"));
  try {
    const file = join(dir, "science.json");
    assert.equal(readScienceConfig(file), null);
    writeFileSync(file, JSON.stringify({ port }));
    assert.deepEqual(readScienceConfig(file), { port });
    for (const body of ["{}", '{"port":"47800"}', '{"port":80}', '{"port":70000}', '{"port":47800.5}', "not json"]) {
      writeFileSync(file, body);
      assert.equal(readScienceConfig(file), null, body);
    }
    rmSync(file);
    assert.equal(readScienceConfig(file), null);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("Science tabs are recognized by origin and lose their secrets in public state", () => {
  assert.ok(isScienceURL(`http://localhost:${port}/projects/1`, port));
  assert.ok(isScienceURL(`http://127.0.0.1:${port}/`, port));
  for (const value of [`http://localhost:47801/`, `https://localhost:${port}/`, `http://example.com:${port}/`, "about:blank", "not a url", undefined])
    assert.equal(isScienceURL(value, port), false, String(value));
  assert.equal(isScienceURL(`http://localhost:${port}/`, undefined), false);
  assert.equal(publicScienceURL(good, port), `http://localhost:${port}/`);
  assert.equal(publicScienceURL(`http://localhost:${port}/oauth/code/callback?code=secret&state=secret`, port), `http://localhost:${port}/oauth/code/callback`);
  assert.equal(publicScienceURL(`http://localhost:${port}/c/1?view=plot&code=secret`, port), `http://localhost:${port}/c/1?view=plot`);
  assert.equal(publicScienceURL(`http://localhost:${port}/c/1?view=plot`, port), `http://localhost:${port}/c/1?view=plot`);
  // Other sites keep their address; masking is not configured without a port.
  assert.equal(publicScienceURL("https://claude.ai/oauth/authorize?state=x", port), "https://claude.ai/oauth/authorize?state=x");
  assert.equal(publicScienceURL(good, undefined), good);
});

test("the tab given the last link is brought forward only while it shows the app or signs in", () => {
  const app = [`http://localhost:${port}/`, `http://localhost:${port}/projects/1?view=plot`, `http://localhost:${port}/oauth/code/callback?code=x&state=y`];
  const signIn = ["https://claude.ai/oauth/authorize?client_id=x", "https://claude.ai/login", "https://platform.claude.com/v1/oauth/token"];
  for (const value of app) {
    assert.equal(scienceTabReusable(value, port), true, value);
    assert.equal(scienceTabReusable(value, port, true), true, value);
  }
  // Sign-in hosts count only right after a link was handed over: later the
  // user may simply be using claude.ai in that tab.
  for (const value of signIn) {
    assert.equal(scienceTabReusable(value, port, true), true, value);
    assert.equal(scienceTabReusable(value, port, false), false, value);
    assert.equal(scienceTabReusable(value, port), false, value);
  }
  assert.equal(scienceSignInWindow, 15 * 60 * 1000);
  for (const value of [
    good,
    `http://localhost:${port}/?nonce=expired0123456789`,
    `http://localhost:${port}/api/auth/nonce`,
    "https://www.bing.com/",
    "http://claude.ai/",
    "https://evil.example/claude.ai",
    "https://user@claude.ai/",
    "about:blank",
    "chrome://newtab/",
    `http://localhost:47801/`,
    "not a url",
  ]) assert.equal(scienceTabReusable(value, port, true), false, value);
});
