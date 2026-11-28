import assert from "node:assert/strict";
import test from "node:test";
import { LoginBrowser } from "./browser-login.mjs";

function loginFixture() {
  const pages = new Map(), navigations = [], closed = [];
  let next = 0;
  const fixture = { pages, navigations, closed, navigationError: "" };
  fixture.browser = new LoginBrowser(async (method, params, sessionId) => {
    switch (method) {
      case "Target.createBrowserContext": return { browserContextId: String(++next) };
      case "Target.createTarget": {
        const targetId = "tab-" + params.browserContextId;
        pages.set(targetId, { targetId, browserContextId: params.browserContextId, type: "page" });
        return { targetId };
      }
      case "Target.getTargets": return { targetInfos: [...pages.values()] };
      case "Target.disposeBrowserContext":
        closed.push(params.browserContextId);
        for (const [id, page] of pages) if (page.browserContextId === params.browserContextId) pages.delete(id);
        return {};
      case "Page.navigate":
        navigations.push(sessionId);
        return { errorText: fixture.navigationError };
      default: throw new Error("Unexpected CDP method " + method);
    }
  });
  fixture.open = (login, address, active) =>
    fixture.browser.open(login.repeat(24), address, async page => page.targetId, [], active);
  return fixture;
}

test("reopening one authorization request preserves its current page and popup", async () => {
  const fixture = loginFixture(), address = "https://claude.ai/oauth/authorize?state=first";
  const first = await fixture.open("a", address);
  const other = await fixture.open("b", address);
  assert.deepEqual(await fixture.open("a", address, other.id), first);
  fixture.pages.set("popup", { type: "page", targetId: "popup", browserContextId: "1" });
  assert.deepEqual(await fixture.open("a", address, "popup"), { id: "popup" });
  assert.deepEqual(fixture.navigations, [first.id, other.id]);
  assert.deepEqual(fixture.closed, []);
  const next = await fixture.open("a", address.replace("first", "second"), "popup");
  assert.notEqual(next.id, first.id);
  assert.deepEqual(fixture.closed, ["1"]);
  assert.ok(fixture.pages.has(other.id));
  fixture.pages.delete(next.id);
  assert.notEqual((await fixture.open("a", address.replace("first", "second"))).id, next.id);
  await fixture.browser.finish("a".repeat(24));
  await fixture.browser.finish("b".repeat(24));
});

test("an initial navigation error releases the failed context and allows another attempt", async () => {
  const fixture = loginFixture(), address = "https://claude.ai/oauth/authorize?state=example";
  fixture.navigationError = "net::ERR_NAME_NOT_RESOLVED";
  await assert.rejects(fixture.open("a", address), { message: "授权页导航失败" });
  assert.deepEqual(fixture.closed, ["1"]);
  fixture.navigationError = "";
  assert.deepEqual(await fixture.open("a", address), { id: "tab-2" });
  fixture.navigationError = "net::ERR_ABORTED";
  const redirected = await fixture.open("b", address);
  assert.deepEqual(await fixture.open("b", address, redirected.id), redirected);
  await fixture.browser.finish("a".repeat(24));
  await fixture.browser.finish("b".repeat(24));
});

test("login redirects and popups stay private, closing one login leaves the other open", async () => {
  const closed = [];
  let contexts = 0;
  const browser = new LoginBrowser(async (method, params, sessionId) => {
    if (method === "Target.createBrowserContext") return { browserContextId: String(++contexts) };
    if (method === "Target.createTarget") {
      assert.equal(params.url, "about:blank", "login must be prepared before navigation");
      return { targetId: "tab-" + params.browserContextId };
    }
    if (method === "Page.navigate") assert.equal(sessionId, "prepared-page");
    if (method === "Target.disposeBrowserContext") closed.push(params.browserContextId);
  });
  const page = { url: "https://claude.ai/oauth/authorize?state=secret", title: "secret" };
  assert.equal(browser.tab(page), page);
  for (const id of ["a", "b"]) {
    await browser.open(id.repeat(24), page.url, async tab => {
      assert.equal(browser.tab(tab).url, "https://claude.ai");
      return "prepared-page";
    });
  }
  const popup = { ...page, targetId: "popup", browserContextId: "1", url: "http://localhost:9123/callback?code=secret" };
  const privateTab = browser.tab(popup);
  assert.equal(privateTab.title, "账号授权");
  assert.equal(privateTab.url, "http://localhost:9123");
  await browser.finish("a".repeat(24));
  assert.deepEqual(closed, ["1"]);
  assert.equal(browser.tab(popup, privateTab).url, privateTab.url);
  await browser.finish("b".repeat(24));
  assert.deepEqual(closed, ["1", "2"]);
});

test("mainstream and explicitly authorized custom agent login hosts open while untrusted hosts fail", async () => {
  let contexts = 0;
  const browser = new LoginBrowser(async (method) => {
    if (method === "Target.createBrowserContext") return { browserContextId: String(++contexts) };
    if (method === "Target.createTarget") return { targetId: "tab-" + contexts };
  });
  for (const url of [
    "https://auth.openai.com/oauth/authorize?client_id=test",
    "https://AUTH.OPENAI.COM/oauth/authorize?client_id=case",
    "https://accounts.google.com/o/oauth2/v2/auth?client_id=test",
    "https://github.com/login/device",
    "https://auth.githubcopilot.com/login",
    "https://auth.factory.ai/authorize",
    "https://app.factory.ai/login",
    "https://pi.dev/login",
  ]) {
    const res = await browser.open("c".repeat(24), url, async () => "sid");
    assert.ok(res.id);
  }
  const customRes = await browser.open("d".repeat(24), "https://auth.custom-agent.dev/login", async () => "sid", ["AUTH.CUSTOM-AGENT.DEV"]);
  assert.ok(customRes.id);
  await assert.rejects(() => browser.open("e".repeat(24), "https://evil.example.org/phish", async () => "sid"));
  await assert.rejects(() => browser.open("e".repeat(24), "http://auth.openai.com/oauth/authorize", async () => "sid"));
  await assert.rejects(() => browser.open("e".repeat(24), "https://auth.openai.com:8443/oauth/authorize", async () => "sid"));
  await assert.rejects(() => browser.open("e".repeat(24), "https://claude.ai:8443/oauth/authorize", async () => "sid"));
  await assert.rejects(() => browser.open("e".repeat(24), "https://auth.custom-agent.dev:9443/login", async () => "sid", ["auth.custom-agent.dev"]));
  await assert.rejects(() => browser.open("e".repeat(24), "https://127.0.0.1/login", async () => "sid", ["127.0.0.1"]));
  await assert.rejects(() => browser.open("e".repeat(24), "https://auth.internal/login", async () => "sid", ["auth.internal"]));
  await assert.rejects(() => browser.open("e".repeat(24), "https://dev.local/login", async () => "sid", ["dev.local"]));
  await browser.finish("c".repeat(24));
  await browser.finish("d".repeat(24));
});
