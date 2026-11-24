// Herdr terminals in the list without a running viewer: Cage's own after
// their viewer ended (herdr_detached) and other Herdr clients' (also
// herdr_imported). Nothing connects to them until their tab is shown; then
// the stream opens (the server attaches a viewer, herdr_terminals.go), the
// heading carries the Herdr tag, and closing one says it ends its program.
// Needs a Vite dev server (CAGE_UI_URL).
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const { page, browser, state, errors } = await createWorkbench({ mockWebSockets: false });
try {
  const connections = new Map(), sockets = new Map();
  await page.routeWebSocket("**/api/**", (socket) => {
    const id = socket.url().match(/\/terminals\/([^/]+)\/stream/)?.[1];
    if (!id) return socket.close();
    connections.set(id, (connections.get(id) || 0) + 1);
    sockets.set(id, socket);
    socket.send(Buffer.from(id + "$ "));
  });
  const workspace = state.settings.workspace;
  const own = "0123456789abcdef", imported = "fedcba9876543210";
  state.terminals = [
    { id: "term-1", kind: "shell", title: "终端 1", workspace },
    { id: own, kind: "claude", title: "Claude 3", workspace, closed: false, herdr_backed: true, herdr_detached: true },
    { id: imported, kind: "shell", title: "vim notes", workspace, closed: false, herdr_backed: true, herdr_detached: true, herdr_imported: true },
    { id: "1111222233334444", kind: "shell", title: "Shell 5", workspace, closed: false, herdr_backed: true, herdr_detached: true },
  ];
  await page.reload();
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  await page.getByRole("tab", { name: "vim notes" }).waitFor();
  await page.waitForTimeout(1500);
  assert.equal(connections.get(own) || 0, 0, "a detached Herdr tab connected before it was shown");
  assert.equal(connections.get(imported) || 0, 0, "another client's terminal was attached before it was shown");

  await page.getByRole("tab", { name: "vim notes" }).click();
  await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("fedcba9876543210$"));
  assert.equal(connections.get(imported), 1);
  const chip = page.locator(".terminal-lane:not([hidden]) .terminal-herdr-chip");
  assert.equal(await chip.textContent(), "Herdr");
  assert.equal(await chip.getAttribute("data-imported"), "true");
  assert.match(await chip.getAttribute("title"), /其他 Herdr 客户端/);
  assert.equal(connections.get(own) || 0, 0);

  await page.getByRole("tab", { name: /Claude 3/ }).click();
  await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("0123456789abcdef$"));
  assert.equal(await page.locator(".terminal-lane:not([hidden]) .terminal-herdr-chip").getAttribute("data-imported"), null);

  // A viewer that ended while its hc terminal lives: 重新连接 views it again
  // in the same tab.
  const shell5 = "1111222233334444";
  const mark = (fields) => { state.terminals = state.terminals.map((item) => item.id === shell5 ? { ...item, ...fields } : item); };
  await page.getByRole("tab", { name: /Shell 5/ }).click();
  await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("1111222233334444$"));
  // The viewer ends (its ssh dropped) while the hc terminal lives: the list
  // says closed and herdr_live, then the stream ends.
  mark({ closed: true, herdr_live: true, herdr_detached: undefined });
  await page.waitForTimeout(6000);
  sockets.get(shell5).close();
  await page.locator(".terminal-lane:not([hidden]) .terminal-note").getByText("已和 Herdr 断开，终端里的程序仍在运行。").waitFor();
  const before = connections.get(shell5) || 0;
  mark({ closed: false, herdr_live: undefined });
  await page.getByRole("button", { name: "重新连接", exact: true }).click();
  await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("1111222233334444$"));
  assert.ok((connections.get("1111222233334444") || 0) > before, "重新连接 opened no stream");
  assert.equal(await page.locator(".terminal-lane:not([hidden]) .terminal-note").getByText("已和 Herdr 断开，终端里的程序仍在运行。").count(), 0);
  assert.equal(await page.getByRole("tab", { name: /Shell 5/ }).count(), 1, "重新连接 made another tab");

  // Closing another client's terminal says what it does, then ends it.
  await page.getByRole("tab", { name: "vim notes" }).click();
  await page.getByRole("button", { name: "关闭当前标签", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByText("这个终端来自其他 Herdr 客户端，关闭会结束其中的程序。", { exact: true }).waitFor();
  await dialog.getByRole("button", { name: "关闭终端", exact: true }).click();
  await page.getByRole("tab", { name: "vim notes" }).waitFor({ state: "detached" });
  assert.ok(state.requests.some((r) => r.method === "DELETE" && r.path === "/terminals/" + imported));
  assert.deepEqual(errors, []);
  console.log("Detached and imported Herdr terminals stay unconnected until shown; Herdr tag; 重新连接 in the same tab; closing copy and DELETE");
} finally { await browser.close(); }
