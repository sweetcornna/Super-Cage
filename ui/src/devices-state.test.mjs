import assert from "node:assert/strict";
import test from "node:test";
import {
  DEVICE_KEY, DEVICE_NAME_KEY, FAILED_TEXT, approvalText, atLimit, computerMeta, countdownText, customRelayLine, deviceIdOf, deviceMeta, deviceName,
  forgetConfirmation, groupDigits, holdReads, initialInvite, initialJoin, inviteReducer, joinFailure, joinReducer, joinedText, kindLine, limitOf, limitText,
  platformName, readCodeInput, readJoinName, rememberDevice, removeConfirmation, requestHint, requestTitle, summaryText, timeText, validTime,
} from "./devices-state.mjs";

const now = new Date(2026, 10, 22, 15, 30, 0).getTime();
const ago = (ms) => new Date(now - ms).toISOString();

test("the code is grouped in fours while it is typed", () => {
  assert.equal(groupDigits("48219037"), "4821 9037");
  assert.equal(groupDigits("4821"), "4821");
  assert.equal(groupDigits("48219"), "4821 9");
  assert.equal(groupDigits("4821 9037 55"), "4821 9037");
  assert.equal(groupDigits(""), "");
});

test("the joining field takes eight digits or a pasted long code", () => {
  assert.deepEqual(readCodeInput("   "), { kind: "empty", display: "", ready: false });
  assert.deepEqual(readCodeInput("4821 9037"), { kind: "short", code: "48219037", display: "4821 9037", ready: true });
  assert.equal(readCodeInput("4821-9037").code, "48219037");
  assert.equal(readCodeInput("48219").ready, false);
  assert.equal(readCodeInput("48219").display, "4821 9");
  assert.equal(readCodeInput("482190371234").code, "48219037");
  const long = readCodeInput(" CAGE-ABCD-1234\n  EFGH ");
  assert.equal(long.kind, "long");
  assert.equal(long.code, "CAGE-ABCD-1234EFGH");
  assert.equal(long.ready, true);
  assert.equal(readCodeInput("CAGE1:{\"v\":1}").kind, "long");
  assert.equal(readCodeInput("CAGE-").ready, false);
  // the start of a long code is not a mistake while it is typed
  for (const typing of ["c", "CA", "cag", "CAGE", "CAGE-", "cage1:"]) assert.equal(readCodeInput(typing).kind, "long", typing);
  assert.equal(readCodeInput("cx").kind, "invalid");
  assert.equal(readCodeInput("CAGEX").kind, "invalid");
  assert.equal(readCodeInput("CAGE-A").ready, false);
  assert.deepEqual(readCodeInput("hello"), { kind: "invalid", display: "hello", ready: false });
});

test("times read the way a person says them", () => {
  assert.equal(timeText(ago(20000), now), "刚刚");
  assert.equal(timeText(ago(12 * 60000), now), "12 分钟前");
  assert.equal(timeText(ago(3 * 3600000), now), "3 小时前");
  assert.equal(timeText(new Date(2026, 10, 22, 8, 2).toISOString(), now), "今天 08:02");
  assert.equal(timeText(new Date(2026, 10, 21, 14, 2).toISOString(), now), "昨天 14:02");
  assert.equal(timeText(new Date(2026, 9, 3, 14, 2).toISOString(), now), "10月3日");
  assert.equal(timeText(new Date(2025, 10, 20, 14, 2).toISOString(), now), "2025年11月20日");
  // the zero time of a host that never saw the device, and no time at all
  assert.equal(timeText("0001-01-01T00:00:00Z", now), "");
  assert.equal(timeText("", now), "");
  assert.equal(timeText(undefined, now), "");
  assert.equal(validTime("0001-01-01T00:00:00Z"), 0);
});

test("a device row names the device, its kind and when it was last connected", () => {
  assert.equal(deviceMeta({ kind: "phone", platform: "ios", last_seen: ago(3 * 3600000) }, now), "手机 · iOS · 上次连接 3 小时前");
  assert.equal(deviceMeta({ kind: "computer", platform: "darwin", connected: true, last_seen: ago(1000) }, now), "电脑 · macOS · 已连接");
  assert.equal(deviceMeta({ kind: "computer", platform: "windows" }, now), "电脑 · Windows · 还没有连接过");
  assert.equal(kindLine({ kind: "phone", platform: "plan9" }), "手机");
  assert.equal(platformName("Linux"), "Linux");
  assert.equal(computerMeta({ platform: "linux", last_open: ago(2 * 86400000) }, now), "电脑 · Linux · 上次打开 11月20日");
  assert.equal(computerMeta({ platform: "darwin" }, now), "电脑 · macOS · 还没有打开过");
  // a host that predates the device report shows a phone without a model
  assert.equal(deviceName({ kind: "phone", name: "" }), "型号待获取");
  assert.equal(deviceName({ kind: "phone", name: "Google Pixel 9 Pro" }), "Google Pixel 9 Pro");
  assert.equal(deviceName({ kind: "computer", name: " " }), "未命名的电脑");
});

test("the header counts devices and online ones; five is the limit unless the host says otherwise", () => {
  const devices = [{ connected: true }, {}, {}];
  assert.equal(summaryText(devices), "3 台设备 · 1 台在线");
  assert.equal(summaryText([]), "");
  assert.equal(summaryText(undefined), "");
  assert.equal(limitOf({}), 5);
  assert.equal(limitOf({ limit: 3 }), 3);
  assert.equal(atLimit({ devices: [{}, {}, {}, {}], limit: 5 }), false);
  assert.equal(atLimit({ devices: [{}, {}, {}, {}, {}], limit: 5 }), true);
  assert.equal(atLimit({ devices: [{}, {}, {}], limit: 3 }), true);
  assert.equal(limitText(5), "已达到 5 台设备，请先移除一台。");
});

test("removing says what happens, with the destructive style", () => {
  const phone = removeConfirmation({ kind: "phone", name: "iPhone 15" });
  assert.equal(phone.body, "移除 iPhone 15 后，它要重新配对才能访问这台电脑。正在进行的操作会立即中断。");
  assert.equal(phone.danger, true);
  assert.equal(phone.title, "移除 iPhone 15？");
  const joined = forgetConfirmation({ name: "家里的 Mac mini" });
  assert.match(joined.body, /还需要在那台电脑的“设备”里移除这台电脑。/);
  assert.equal(joined.danger, true);
});

test("the countdown reads m:ss and ends at zero", () => {
  const expires = new Date(now + 107000).toISOString();
  assert.equal(countdownText(expires, now), "1:47 后失效");
  assert.equal(countdownText(expires, now + 100000), "0:07 后失效");
  assert.equal(countdownText(expires, now + 107000), "已过期");
  assert.equal(countdownText(expires, now + 500000), "已过期");
  assert.equal(countdownText(now + 107000, now), "1:47 后失效", "the dialog holds milliseconds");
  assert.equal(countdownText("", now), "");
  assert.equal(countdownText(0, now), "");
  assert.equal(customRelayLine("https://relay.example.com/x"), "这台电脑使用自建中继 relay.example.com，对方也要设置同一个中继。");
  assert.equal(customRelayLine(""), "");
});

test("the add dialog follows the host from code to approval to joined", () => {
  const expires = new Date(now + 120000).toISOString();
  let state = inviteReducer(initialInvite, { type: "minted", reply: { image: "data:image/png;base64,AA==", manual: "CAGE-X", short: "48219037", expires }, now });
  assert.equal(state.phase, "code");
  assert.equal(state.short, "48219037");
  assert.equal(state.expires, now + 120000);
  assert.equal(state.refreshed, false);
  assert.equal(inviteReducer(initialInvite, { type: "minted", reply: { image: "x", manual: "CAGE-Y", expires }, refreshed: true }).refreshed, true);
  // waiting changes nothing
  assert.equal(inviteReducer(state, { type: "poll", result: { state: "waiting" } }), state);
  // a host without short codes shows the QR and the long code only
  assert.equal(inviteReducer(initialInvite, { type: "minted", reply: { image: "x", manual: "CAGE-X", expires } }).short, "");
  assert.equal(inviteReducer(initialInvite, { type: "minted", reply: { image: "x", manual: "CAGE-X", short: "12", expires } }).short, "");
  state = inviteReducer(state, { type: "poll", result: { state: "request", request: { name: "MacBook Air", platform: "darwin", check: "3817" } } });
  assert.equal(state.phase, "request");
  assert.deepEqual(state.request, { name: "MacBook Air", platform: "darwin", check: "3817" });
  assert.equal(requestTitle(state.request), "MacBook Air（macOS）想加入这台电脑");
  assert.equal(requestHint("3817"), "核对数字 3817，与那台电脑上显示的一致再允许。");
  const deciding = inviteReducer(state, { type: "deciding" });
  assert.equal(deciding.deciding, true);
  // the same request read again keeps the decision in progress
  assert.equal(inviteReducer(deciding, { type: "poll", result: { state: "request", request: { name: "MacBook Air", platform: "darwin", check: "3817" } } }), deciding);
  // the time passing does not end an approval that is waiting
  assert.equal(inviteReducer(state, { type: "tick", now: now + 999999 }), state);
  const joined = inviteReducer(deciding, { type: "poll", result: { state: "joined", device: { id: "d2", name: "MacBook Air" } } });
  assert.deepEqual(joined, { phase: "joined", device: { id: "d2", name: "MacBook Air" } });
  // nothing moves a finished dialog
  assert.equal(inviteReducer(joined, { type: "poll", result: { state: "failed" } }), joined);
  assert.equal(inviteReducer(joined, { type: "tick", now: now + 999999 }), joined);
});

test("a code that is not used expires, fails or is refused", () => {
  const expires = new Date(now + 5000).toISOString();
  const code = inviteReducer(initialInvite, { type: "minted", reply: { image: "x", manual: "CAGE-X", short: "48219037", expires }, now });
  assert.equal(inviteReducer(code, { type: "tick", now: now + 4999 }), code);
  assert.deepEqual(inviteReducer(code, { type: "tick", now: now + 5000 }), { phase: "expired" });
  assert.deepEqual(inviteReducer(code, { type: "poll", result: { state: "expired" } }), { phase: "expired" });
  assert.deepEqual(inviteReducer(code, { type: "poll", result: { state: "failed" } }), { phase: "failed" });
  assert.equal(FAILED_TEXT, "配对失败：配对码不对或连接被篡改。请重新生成。");
  const request = inviteReducer(code, { type: "poll", result: { state: "request", request: { name: "x", platform: "windows", check: "1234" } } });
  assert.equal(inviteReducer(request, { type: "rejected" }).phase, "rejected");
  const limit = inviteReducer(initialInvite, { type: "error", error: Object.assign(new Error("最多添加 5 台设备。请先移除不用的设备，再添加新的设备。"), { status: 409 }) });
  assert.deepEqual(limit, { phase: "limit", limit: 5 });
  // another conflict is told as the host tells it
  assert.deepEqual(inviteReducer(initialInvite, { type: "error", error: Object.assign(new Error("设备列表无法读取。请先移除全部设备，再重新添加。"), { status: 409 }) }), { phase: "error", message: "设备列表无法读取。请先移除全部设备，再重新添加。" });
  // the host has no code any more
  assert.deepEqual(inviteReducer(code, { type: "poll", result: { state: "none" } }), { phase: "expired" });
  assert.equal(inviteReducer(initialInvite, { type: "error", error: Object.assign(new Error("请先解锁 Super Cage"), { status: 423 }) }).phase, "error");
  assert.deepEqual(inviteReducer(code, { type: "restart" }), initialInvite);
});

test("the joining dialog asks, waits for the other computer, and says why it failed", () => {
  let state = joinReducer(initialJoin, { type: "submit" });
  assert.equal(state.phase, "searching");
  state = joinReducer(state, { type: "reply", reply: { state: "approval", check: "3817" } });
  assert.deepEqual(state, { phase: "approval", check: "3817" });
  assert.equal(approvalText("3817"), "在那台电脑上点“允许”。核对数字 3817");
  assert.equal(joinReducer(state, { type: "reply", reply: { state: "approval", check: "3817" } }), state);
  assert.deepEqual(joinReducer(state, { type: "reply", reply: { state: "joined", id: "h1", name: "家里的 Mac mini" } }), { phase: "joined", computer: { id: "h1", name: "家里的 Mac mini" } });
  assert.deepEqual(joinReducer(initialJoin, { type: "reply", reply: { id: "h1", name: "Mac mini" } }), { phase: "joined", computer: { id: "h1", name: "Mac mini" } });
  assert.equal(joinedText("家里的 Mac mini"), "已加入「家里的 Mac mini」");
  assert.equal(joinedText(""), "已加入「那台电脑」");
  assert.equal(joinReducer(state, { type: "reply", reply: { state: "denied" } }).error, "那台电脑没有允许加入。");
  assert.equal(joinReducer(state, { type: "reply", reply: { state: "expired" } }).phase, "input");
  // typing clears the error; typing while waiting changes nothing
  assert.equal(joinReducer({ phase: "input", error: "x" }, { type: "edit" }).error, "");
  assert.equal(joinReducer(state, { type: "edit" }), state);
});

test("a refused join says what to do next", () => {
  assert.equal(joinFailure(Object.assign(new Error("x"), { status: 404 })), "没有找到这个配对码：请核对数字，并确认两台电脑使用同一个中继。");
  assert.equal(joinFailure(Object.assign(new Error("x"), { status: 409 })), "配对码已被使用，请在那台电脑上重新生成。");
  assert.equal(joinFailure(Object.assign(new Error("x"), { status: 410 })), "配对码已被使用，请在那台电脑上重新生成。");
  // the gateway's own sentence wins when it has one the page cannot know: the 409 that names both relays, a locked computer
  const relays = "这个配对码属于中继 relay.example.com，而这台电脑使用官方中继。请在“设备”里选择同一个中继。";
  assert.equal(joinFailure(Object.assign(new Error(relays), { status: 409 })), relays);
  assert.equal(joinFailure(Object.assign(new Error("Super Cage 已锁定，请先解锁。"), { status: 423 })), "Super Cage 已锁定，请先解锁。");
  // but the generic sentence api.js composes for a status is not shown
  assert.equal(joinFailure(Object.assign(new Error("连接 Super Cage 工作台失败（HTTP 409）：环境状态已变化。"), { status: 409 })), "配对码已被使用，请在那台电脑上重新生成。");
  assert.equal(joinFailure(Object.assign(new Error("连接 Super Cage 工作台失败（HTTP 502）：后台未能连接。"), { status: 502 })), "加入没有完成，请稍后重试。");
  assert.equal(joinFailure(new Error("")), "加入没有完成，请稍后重试。");
});

test("the name of a computer to join is optional, up to 40 characters, without control characters", () => {
  assert.equal(readJoinName("家里的 Mac mini"), "家里的 Mac mini");
  assert.equal(readJoinName("a\u0000b\nc\u007fd"), "abcd");
  assert.equal(readJoinName("x".repeat(60)).length, 40);
  assert.equal(readJoinName(undefined), "");
  // the gateway does not know the other computer's platform yet and sends no last open before the first one
  assert.equal(computerMeta({ platform: "", last_open: null }, now), "电脑 · 还没有打开过");
});

test("a joined computer's window remembers its connection and name; only a device connection has an id", () => {
  const store = new Map();
  const storage = { setItem: (key, value) => store.set(key, value) };
  rememberDevice("device:h1:3", "家里的 Mac mini", storage);
  assert.equal(store.get(DEVICE_KEY), "device:h1:3");
  assert.equal(store.get(DEVICE_NAME_KEY), "家里的 Mac mini");
  assert.equal(deviceIdOf("device:h1:3"), "h1");
  assert.equal(deviceIdOf("device:a-b_c:12"), "a-b_c");
  for (const other of ["", null, "local", "device:", "device:h1", "device:h1:x", "device:h/1:2", "xdevice:h1:2", "device:h1:2 "]) assert.equal(deviceIdOf(other), "", String(other));
});

// ── held reads on a fake clock ─────────────────────────────────────────────

function clock() {
  let time = 0;
  const sleeps = [];
  return { now: () => time, advance: (ms) => { time += ms; }, sleeps, sleep: async (ms) => { sleeps.push(ms); time += ms; } };
}

test("a read that came back at once is followed by the rest of the second", async () => {
  const c = clock();
  const results = [{ state: "waiting" }, { state: "waiting" }, { state: "joined" }];
  const seen = [];
  await holdReads({
    read: async (wait) => { assert.equal(wait, 25); return results.shift(); },
    onResult: (result) => { seen.push(result.state); return result.state === "joined"; },
    signal: new AbortController().signal, now: c.now, sleep: c.sleep,
  });
  assert.deepEqual(seen, ["waiting", "waiting", "joined"]);
  assert.deepEqual(c.sleeps, [1000, 1000]);
});

test("a held read that waited its time starts the next at once", async () => {
  const c = clock();
  let reads = 0;
  await holdReads({
    read: async () => { c.advance(25000); return { state: ++reads === 2 ? "expired" : "waiting" }; },
    onResult: (result) => result.state === "expired",
    signal: new AbortController().signal, now: c.now, sleep: c.sleep,
  });
  assert.equal(reads, 2);
  assert.deepEqual(c.sleeps, []);
});

test("failed reads back off, and the caller can end the loop", async () => {
  const c = clock();
  const failures = [];
  let reads = 0;
  await holdReads({
    read: async () => { reads++; throw Object.assign(new Error("断开"), { status: reads > 3 ? 404 : 502 }); },
    onResult: () => false,
    onError: (error, count) => { failures.push([error.status, count]); return error.status === 404; },
    signal: new AbortController().signal, now: c.now, sleep: c.sleep,
  });
  assert.deepEqual(failures, [[502, 1], [502, 2], [502, 3], [404, 4]]);
  assert.deepEqual(c.sleeps, [1000, 2000, 5000]);
});

test("closing the dialog ends the loop without another request", async () => {
  const controller = new AbortController();
  let reads = 0;
  await holdReads({
    read: async () => { reads++; controller.abort(); return { state: "waiting" }; },
    onResult: () => { throw new Error("not after abort"); },
    signal: controller.signal, sleep: async () => { throw new Error("no sleep after abort"); },
  });
  assert.equal(reads, 1);
  const pending = new AbortController();
  const done = holdReads({ read: () => new Promise((_, reject) => pending.signal.addEventListener("abort", () => reject(new Error("aborted")))), onResult: () => false, signal: pending.signal });
  pending.abort();
  await done;
});
