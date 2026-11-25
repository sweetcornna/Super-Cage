import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  accountPollDelay,
  desktopFilesPollDelay,
  statusPollDelay,
  vaultPollActive,
} from "./idle-poll.mjs";

const srcDir = path.dirname(fileURLToPath(import.meta.url));
const readSrc = (name) => fs.readFileSync(path.join(srcDir, name), "utf8");

test("idle-poll delay helpers stop timers when hidden and back off when inactive", () => {
  assert.equal(statusPollDelay({ jobRunning: false, windowVisible: false }), null);
  assert.equal(statusPollDelay({ jobRunning: false, windowVisible: true }), 5000);
  assert.equal(statusPollDelay({ jobRunning: true, windowVisible: true }), 400);
  assert.equal(statusPollDelay({ jobRunning: true, windowVisible: false }), 2000);

  assert.equal(accountPollDelay({ windowVisible: false }), null);
  assert.equal(accountPollDelay({ windowVisible: true }), 2500);

  assert.equal(desktopFilesPollDelay({ failures: 0, windowVisible: false, desktopVisible: true }), null);
  assert.equal(desktopFilesPollDelay({ failures: 0, windowVisible: true, desktopVisible: true }), 1500);
  assert.equal(desktopFilesPollDelay({ failures: 0, windowVisible: true, desktopVisible: false }), 10000);
  assert.equal(desktopFilesPollDelay({ failures: 4, windowVisible: true, desktopVisible: false }), 30000);

  assert.equal(vaultPollActive({ ready: true, tab: "t1", browserVisible: true, windowVisible: true }), true);
  assert.equal(vaultPollActive({ ready: true, tab: "t1", browserVisible: true, windowVisible: false }), false);
  assert.equal(vaultPollActive({ ready: true, tab: "t1", browserVisible: false, windowVisible: true }), false);
  assert.equal(vaultPollActive({ ready: true, tab: "", browserVisible: true, windowVisible: true }), false);
  assert.equal(vaultPollActive({ ready: false, tab: "t1", browserVisible: true, windowVisible: true }), false);
});

test("global always-mounted pollers stop when window is hidden or feature view is inactive", () => {
  const vault = readSrc("password-vault.jsx");
  assert.match(vault, /useWindowVisibility/, "password-vault.jsx must gate polling on useWindowVisibility");
  assert.match(vault, /vaultPollActive/, "password-vault.jsx must gate polling via vaultPollActive");

  const main = readSrc("main.jsx");
  assert.match(main, /statusPollDelay/, "main.jsx must compute /status delay via statusPollDelay so hidden idle stops polling");
  assert.match(main, /usePasswordVault\([^,]+,\s*browser\.active,\s*browserShown\)/, "main.jsx must pass browserShown to usePasswordVault");

  const downloads = readSrc("browser-downloads.jsx");
  assert.match(downloads, /desktopFilesPollDelay.*idle-poll\.mjs/s, "browser-downloads.jsx must import desktopFilesPollDelay from idle-poll.mjs");
  assert.doesNotMatch(downloads, /windowVisible \? 1500 : 4500/, "browser-downloads.jsx must stop polling when hidden instead of 4.5s background polling");

  const account = readSrc("account.jsx");
  assert.match(account, /useWindowVisibility/, "account.jsx must observe useWindowVisibility");
  assert.match(account, /accountPollDelay/, "account.jsx must stop polling while hidden via accountPollDelay");

  const latency = readSrc("network-latency.jsx");
  assert.match(latency, /useWindowVisibility/, "network-latency.jsx must stop external latency probes when hidden");
});

test("panel and dialog timers stop when hidden or when VM is stopped", () => {
  for (const file of [
    "claude-accounts.jsx",
    "codex-accounts.jsx",
    "tailscale.jsx",
    "proxy-settings.jsx",
    "resources.jsx",
    "agent-accounts.jsx",
    "session-account.jsx",
    "cli-sessions.jsx",
    "devices-dialogs.jsx",
    "app-updates.jsx",
  ]) {
    const text = readSrc(file);
    assert.match(text, /useWindowVisibility/, `${file} must gate timers on useWindowVisibility`);
  }
  assert.match(readSrc("claude-accounts.jsx"), /if\s*\(\s*!windowVisible\s*\|\|\s*!running\s*\)\s*return/, "claude-accounts.jsx must stop polling when hidden or VM stopped");
  assert.match(readSrc("codex-accounts.jsx"), /if\s*\(\s*!windowVisible\s*\|\|\s*!running/, "codex-accounts.jsx must stop polling when hidden or VM stopped");
  assert.match(readSrc("tailscale.jsx"), /if\s*\(\s*!ready\s*\|\|\s*!windowVisible\s*\)\s*return/, "tailscale.jsx must stop timer when hidden or VM not ready");
});
