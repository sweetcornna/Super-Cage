// The 新建 menu as the window renders it for each Codex presence state
// (design 2.3, choice C): TerminalMenu rendered on the server with the prop
// main.jsx passes, codexSurfaces(status, system).menu.
import assert from "node:assert/strict";
import test from "node:test";
import { fileURLToPath } from "node:url";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { createServer } from "vite";
import { codexSurfaces } from "../src/codex-accounts-state.mjs";
import { codexMenuLabel } from "../src/session-provider.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));

const items = (html) => [...html.matchAll(/<button role="menuitem"[^>]*>(.*?)<\/button>/g)]
  .map((match) => match[1].replace(/<[^>]*>/g, ""));

test("the 新建 menu offers Codex when it is present or unknown, and not when it is absent", async () => {
  const vite = await createServer({ root, configFile: false, logLevel: "silent", appType: "custom",
    server: { middlewareMode: true, hmr: false, ws: false }, optimizeDeps: { noDiscovery: true, include: [] } });
  try {
    const { TerminalMenu } = await vite.ssrLoadModule("/src/terminal-menu.jsx");
    const menu = (status, system = "debian-13") => items(renderToStaticMarkup(React.createElement(TerminalMenu, {
      choice: { anchor: null }, busy: false, codex: codexSurfaces(status, system).menu,
      codexLabel: codexMenuLabel({ sessions: true }), onClose() {}, onSelect() {},
    })));
    const today = ["普通终端", "Claude 终端", "Codex 会话"];
    assert.deepEqual(menu({ clients_unknown: true }), today, "unknown: as today");
    assert.deepEqual(menu({ clients: ["codex"] }), today, "present");
    assert.deepEqual(menu({}), ["普通终端", "Claude 终端"], "absent");
    assert.deepEqual(menu({ clients: ["antigravity-cli"] }), ["普通终端", "Claude 终端"], "another client only");
    assert.deepEqual(menu({ clients_unknown: true }, "macos"), ["普通终端", "Claude 终端"], "a macOS guest, as today");
  } finally {
    await vite.close();
  }
});
