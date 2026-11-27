import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { createServer } from "vite";

const root = fileURLToPath(new URL("..", import.meta.url));
const fixtureId = "harness-marks-fixture";
const fixturePath = "/@id/__x00__" + fixtureId;
const branded = [
  "aider", "amp", "antigravity", "claude", "cline", "codex", "copilot", "cursor",
  "deepseek-harness", "droid", "goose", "grok", "herdr", "kimi", "magpie",
  "opencode", "orca", "pi", "qwen", "t3code", "vibe",
];
const generic = ["custom-test", "constructor"];
const source = `
  import React from "react";
  import { createRoot } from "react-dom/client";
  import { AgentIcon } from "/src/agent-icon.jsx";
  import { ProviderMark } from "/src/provider-mark.jsx";
  import { TerminalMenu } from "/src/terminal-menu.jsx";
  import { TabIcon } from "/src/workspace-tabs.jsx";
  import "/src/tokens.css";
  const el = React.createElement;
  const ids = ${JSON.stringify([...branded, ...generic])};
  createRoot(document.querySelector("main")).render(el(React.Fragment, null,
    ...ids.map(id => el("div", { key: id, "data-test-id": id },
      el(AgentIcon, { agentId: id, size: 17 }), el(AgentIcon, { agentId: id, size: 34 }), id)),
    el("div", { id: "account" }, el(ProviderMark, { provider: " CODEX-CLI ", accountNumber: 12 })),
    el("div", { id: "tab" }, el(TabIcon, { kind: "terminal", agentId: "openai-codex", accountNumber: 2 })),
    el("div", { id: "custom" }, el(AgentIcon, { agentId: " DSH " })),
    el(TerminalMenu, { choice: {}, codex: true, codexLabel: "Codex 终端", onClose() {},
      agents: [{ spec: { id: "qwen", short_name: "Qwen" }, runtime: { installed: true }, config: { enabled: true } }] })
  ));
`;

const vite = await createServer({
  root,
  plugins: [{
    name: "harness-marks-fixture",
    configureServer(server) {
      server.middlewares.use("/__harness-marks", async (_, res, next) => {
        res.setHeader("Content-Type", "text/html");
        const html = `<!doctype html><html data-theme="light"><meta charset="utf-8">
          <style>body{background:var(--paper);color:var(--ink);font:14px system-ui}</style>
          <main></main><script type="module" src="${fixturePath}"></script></html>`;
        try { res.end(await server.transformIndexHtml("/__harness-marks", html)); }
        catch (error) { next(error); }
      });
    },
    resolveId(id) { if (id === fixtureId || id === "\0" + fixtureId) return "\0" + fixtureId; },
    load(id) { if (id === "\0" + fixtureId) return source; },
  }],
  optimizeDeps: {
    noDiscovery: true,
    include: ["react", "react/jsx-dev-runtime", "react-dom", "react-dom/client", "lucide-react"],
  },
  server: { host: "127.0.0.1", port: 0 },
});
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");

try {
  await vite.listen();
  const origin = `http://127.0.0.1:${vite.httpServer.address().port}`;
  const browser = await chromium.launch({
    headless: true, executablePath: process.env.CAGE_TEST_BROWSER_PATH,
  });
  try {
    const page = await browser.newPage({ colorScheme: "light" });
    page.setDefaultTimeout(10000);
    const errors = [];
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/*", route => {
      assert.equal(new URL(route.request().url()).origin, origin);
      return route.continue();
    });
    await page.goto(origin + "/__harness-marks");
    const product = id => page.locator(`[data-test-id="${id}"]`);
    for (const theme of ["light", "dark"]) {
      await page.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      for (const id of branded) {
        const images = product(id).locator("img:visible");
        await images.first().waitFor();
        assert.equal(await images.count(), 2, id);
        for (const image of await images.all()) {
          assert.ok(await image.evaluate(async image => {
            await image.decode();
            return image.naturalWidth > 0 && image.alt === "";
          }), id);
        }
        if (id === "codex" || id === "cursor") {
          assert.ok((await images.first().getAttribute("class")).endsWith(`--${theme}`));
        }
      }
      for (const id of generic) {
        assert.equal(await product(id).locator("img").count(), 0);
        assert.equal(await product(id).locator("svg:visible").count(), 2);
      }
      if (process.env.CAGE_TEST_CAPTURE) {
        await page.screenshot({ path: `${process.env.CAGE_TEST_CAPTURE}-${theme}.png`, fullPage: true });
      }
    }

    const account = page.locator("#account").getByRole("img", { name: "Codex · 账号 12" });
    assert.equal(await account.getAttribute("data-number"), "12");
    assert.equal(await account.evaluate(el => getComputedStyle(el, "::after").content), '"12"');
    await page.locator("#tab").getByRole("img", { name: "Codex · 账号 2" }).waitFor();
    await page.locator("#custom").getByRole("img", { name: "DeepSeek Harness" }).waitFor();
    const item = page.getByRole("menuitem", { name: "Codex 终端", exact: true });
    assert.equal(await item.locator(".provider-mark").getAttribute("aria-hidden"), "true");
    await item.locator("img:visible").waitFor();

    await page.emulateMedia({ colorScheme: "dark" });
    await page.evaluate(() => { delete document.documentElement.dataset.theme; });
    await account.locator("img.harness-mark-graphic--dark").waitFor();
    await page.route("**/src/harness-marks/codex.png*", route =>
      route.request().resourceType() === "image" ? route.abort() : route.fallback());
    await page.reload();
    await account.locator("svg.harness-mark-graphic--light").waitFor();
    assert.equal(await account.getAttribute("data-number"), "12");
    assert.deepEqual(errors, []);
    console.log("PASS harness marks, local assets, themes, aliases, account badges and image fallback");
  } finally {
    await browser.close();
  }
} finally {
  await vite.close();
}
