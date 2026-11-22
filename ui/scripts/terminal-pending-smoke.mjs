import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

// A new terminal's tab and pane appear as soon as it is asked for, with a
// quiet waiting line while the server creates it; the record replaces the
// placeholder in place, a status poll meanwhile keeps it, a failure is shown
// in its place with a retry, a placeholder closed early closes the terminal
// that still arrives, and a user who moved on is not pulled back.
const fixture = await createWorkbench();
const { browser, page, errors, state } = fixture;
const until = async (condition, message) => {
  for (let i = 0; i < 400; i++) {
    if (await condition()) return;
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  throw new Error(message);
};
let hold = false, fail = "";
const held = [];
const statusReads = [];
page.on("request", request => {
  if (request.method() === "GET" && new URL(request.url()).pathname === "/api/status") statusReads.push(Date.now());
});
await page.route("**/api/terminals", async route => {
  if (route.request().method() !== "POST") return route.fallback();
  if (fail) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: fail });
  if (hold) await new Promise(resolve => held.push(resolve));
  return route.fallback();
});
const release = () => held.splice(0).forEach(resolve => resolve());
const title = () => page.evaluate(() => document.querySelector("header.terminal-chrome .current-title")?.textContent || "");
const terminalTabs = () => page.evaluate(() => [...document.querySelectorAll(".workspace-sidebar [role=tab]")]
  .map(tab => tab.textContent.trim()).filter(text => /^(终端|Claude|Codex)/.test(text)));
const newTerminal = async () => {
  await page.getByRole("button", { name: /新建终端$/ }).first().click();
  await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
};
const waiting = page.getByRole("status").filter({ hasText: "正在准备终端…" });
try {
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();

  // The placeholder shows at once, survives a status poll and is replaced in place.
  hold = true;
  await newTerminal();
  await waiting.waitFor({ timeout: 2_000 });
  assert.equal(await title(), "终端", "the placeholder tab is active");
  assert.deepEqual(await terminalTabs(), ["终端"]);
  const polls = statusReads.length;
  await until(() => statusReads.length > polls + 1, "status was not polled while the terminal was created");
  await new Promise(resolve => setTimeout(resolve, 300));
  assert.equal(await waiting.count(), 1, "a status poll dropped the placeholder");
  assert.equal(await title(), "终端", "a status poll moved away from the placeholder");
  release();
  await until(async () => (await title()) === "终端 1", "the record did not replace the placeholder");
  await page.locator(".terminal-lane:not([hidden]) .xterm-rows").getByText("Linux terminal").waitFor();
  assert.equal(await waiting.count(), 0);
  assert.deepEqual(await terminalTabs(), ["终端 1"]);

  // A failure replaces the waiting line, once, with the server's words; a
  // retry waits again and ends with the terminal in the same place.
  hold = false;
  fail = "请先通过打开项目添加该文件夹";
  await newTerminal();
  const failure = page.getByRole("alert").filter({ hasText: "终端没有打开" });
  await failure.waitFor();
  assert.equal(await page.getByText(fail, { exact: true }).count(), 1, "the error must show once, in place");
  assert.deepEqual(await terminalTabs(), ["终端 1", "终端"]);
  fail = "";
  hold = true;
  await failure.getByRole("button", { name: "重试", exact: true }).click();
  await waiting.waitFor();
  release();
  await until(async () => (await title()) === "终端 2", "a retry did not open the terminal");
  assert.deepEqual(await terminalTabs(), ["终端 1", "终端 2"]);

  // Closed while it is created: no confirmation, and the terminal that still
  // arrives is closed on the server.
  await newTerminal();
  await waiting.waitFor();
  await page.locator("header.terminal-chrome").getByRole("button", { name: "关闭当前标签", exact: true }).click();
  await until(async () => (await waiting.count()) === 0, "the placeholder was not closed");
  assert.equal(await page.getByRole("dialog").count(), 0, "closing a placeholder must not ask");
  release();
  await until(() => state.requests.some(request => request.method === "DELETE" && request.path === "/terminals/term-3"),
    "the terminal created after its placeholder closed was not closed");
  await new Promise(resolve => setTimeout(resolve, 300));
  assert.deepEqual(await terminalTabs(), ["终端 1", "终端 2"]);

  // A user who moved on stays there; the terminal appears in its place.
  await newTerminal();
  await waiting.waitFor();
  await page.getByRole("tab", { name: "终端 1", exact: true }).first().click();
  await until(async () => (await title()) === "终端 1", "could not switch away from the placeholder");
  release();
  await until(async () => (await terminalTabs()).includes("终端 3"), "the record did not replace the hidden placeholder");
  await new Promise(resolve => setTimeout(resolve, 300));
  assert.equal(await title(), "终端 1", "the record pulled the user back");
  assert.deepEqual(await terminalTabs(), ["终端 1", "终端 2", "终端 3"]);
  assert.deepEqual(errors, []);
  console.log("PASS: a new terminal's tab appears at once, survives polls, is replaced in place, shows failures in place with a retry, closes early and leaves the user where they are");
} finally {
  release();
  await browser.close();
}
