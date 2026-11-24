// An account-login terminal belongs to no project. Opening its authorization
// page must leave a way back: the sidebar's 账号登录 group and the page's
// 回到登录终端 button both return to the terminal to paste the copied code.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const url = "https://accounts.google.com/o/oauth2/v2/auth?client_id=fixture&state=" + "fixture".repeat(12);
const login = {
  id: "login-1", kind: "agent-login", workspace: "", title: "Antigravity 账号登录",
  agent_id: "antigravity", agent_name: "Antigravity", oauth_domains: ["accounts.google.com"], closed: false,
};
const screenshot = process.env.SCREENSHOT || "";
const { browser, page, errors } = await createWorkbench({ mockWebSockets: false });
try {
  let created = false;
  await page.route("**/api/terminals", (route) => {
    if (route.request().method() === "GET") return route.fulfill({ json: created ? [login] : [] });
    created = true;
    return route.fulfill({ json: login });
  });
  await page.route("**/api/terminals/*/login-link", (route) => route.fulfill({ json: { id: "login-tab" } }));
  await page.routeWebSocket("**/api/**", (socket) => {
    if (!socket.url().includes("/terminals/")) return socket.close();
    socket.send(Buffer.from("Open this link to sign in:\r\n" + url + "\r\nEnter the authorization code: "));
  });
  await page.reload();
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
  await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
  const row = page.locator(".xterm-rows > div").filter({ hasText: "accounts.google.com" }).first();
  await row.waitFor();
  const rect = await row.boundingBox();
  await page.mouse.move(rect.x + 20, rect.y + rect.height / 2);
  await page.waitForFunction(() => !!document.querySelector(".xterm-cursor-pointer"));
  await page.mouse.down();
  await page.mouse.up();
  await page.getByRole("tab", { name: /账号授权/ }).waitFor();

  const group = page.getByRole("region", { name: "账号登录终端" });
  await group.waitFor();
  assert.equal(await group.getByText("Antigravity", { exact: false }).count() > 0, true);
  const back = page.getByRole("button", { name: "回到登录终端", exact: true });
  await back.waitFor();
  if (screenshot) await page.screenshot({ path: screenshot + "-page.png" });
  await back.click();
  await page.locator(".xterm-rows > div").filter({ hasText: "Enter the authorization code" }).first().waitFor();
  await page.waitForFunction(() => !document.querySelector(".browser-login-return"));
  if (screenshot) await page.screenshot({ path: screenshot + "-terminal.png" });

  // From any other view the sidebar row leads back as well.
  await page.getByRole("button", { name: "工作台", exact: true }).click();
  await page.locator(".xterm-rows").first().waitFor({ state: "hidden" });
  await group.getByText("Antigravity 账号登录", { exact: false }).first().click();
  await page.locator(".xterm-rows > div").filter({ hasText: "Enter the authorization code" }).first().waitFor();
  assert.deepEqual(errors, []);
} finally { await browser.close(); }
console.log("The authorization page and the sidebar both lead back to the account-login terminal.");
