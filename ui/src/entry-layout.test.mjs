import assert from "node:assert/strict";
import { beforeEach, test } from "node:test";
import { guardDesktopEntry, mountPhoneSwitch, phoneEntryOffered, phoneSized } from "./entry-layout.mjs";

// A minimal browser for the entry rules: a viewport, media queries, the cookie jar, the server's mark.
let env;
beforeEach(() => {
  env = { coarse: true, hover: false, width: 390, height: 844, screen: [390, 844], marked: true, cookies: [], reloads: 0, appended: [], storage: new Map() };
  globalThis.window = globalThis;
  delete globalThis.cageNativeBridge;
  delete globalThis.webkit;
  globalThis.matchMedia = (query) => ({ matches: query === "(pointer: coarse)" ? env.coarse : query === "(hover: hover)" ? env.hover : query === "(hover: none)" ? !env.hover : false });
  Object.defineProperty(globalThis, "innerWidth", { configurable: true, get: () => env.width });
  Object.defineProperty(globalThis, "innerHeight", { configurable: true, get: () => env.height });
  globalThis.screen = { get width() { return env.screen[0]; }, get height() { return env.screen[1]; } };
  globalThis.location = { protocol: "https:", reload: () => { env.reloads += 1; } };
  globalThis.sessionStorage = { getItem: (k) => env.storage.get(k) ?? null, setItem: (k, v) => env.storage.set(k, v), removeItem: (k) => env.storage.delete(k) };
  globalThis.document = {
    get cookie() { return env.cookies.map((c) => c.split(";")[0]).join("; "); },
    set cookie(value) { env.cookies = env.cookies.filter((c) => c.split("=")[0] !== value.split("=")[0]).concat(value); },
    querySelector: (selector) => (env.marked && selector === 'meta[name="cage-phone-entry"][content="available"]' ? {} : null),
    createElement: () => ({ setAttribute() {}, addEventListener() {} }),
    body: { append: (node) => env.appended.push(node) },
  };
});

test("a phone in either orientation is phone-sized; tablets, mice and the desktop site are not", () => {
  assert.equal(phoneSized(), true);
  Object.assign(env, { width: 844, height: 390 });
  assert.equal(phoneSized(), true, "a phone on its side");
  Object.assign(env, { width: 820, height: 1180, screen: [820, 1180] });
  assert.equal(phoneSized(), false, "a tablet");
  Object.assign(env, { width: 390, height: 844, screen: [390, 844], coarse: false, hover: true });
  assert.equal(phoneSized(), false, "a desktop browser resized narrow");
});

test("a phone that asked for the desktop site keeps it, whatever its height", () => {
  Object.assign(env, { width: 980, height: 1690 });
  assert.equal(phoneSized(), false, "portrait: 980 px layout viewport");
  Object.assign(env, { width: 980, height: 450, screen: [390, 844] });
  assert.equal(phoneSized(), false, "landscape: 980 px wide, under 500 px tall");
  Object.assign(env, { width: 980, height: 450, screen: [1024, 1366] });
  assert.equal(phoneSized(), false, "980 px even on a screen that wide");
  Object.assign(env, { width: 900, height: 450, screen: [390, 844] });
  assert.equal(phoneSized(), false, "a layout viewport wider than the screen");
});

test("only a marked document in a browser offers the phone UI", () => {
  assert.equal(phoneEntryOffered(), true);
  env.marked = false;
  assert.equal(phoneEntryOffered(), false, "serve-desktop, guests and older workspaces do not mark it");
  env.marked = true;
  globalThis.cageNativeBridge = { postMessage() {} };
  assert.equal(phoneEntryOffered(), false, "Linux and Windows desktop windows");
  delete globalThis.cageNativeBridge;
  globalThis.webkit = { messageHandlers: { cage: { postMessage() {} } } };
  assert.equal(phoneEntryOffered(), false, "the macOS desktop window");
});

test("the guard hands a phone over once, and only from a marked document", () => {
  env.marked = false;
  assert.equal(guardDesktopEntry(), false);
  assert.deepEqual([env.cookies, env.reloads], [[], 0]);
  env.marked = true;
  globalThis.cageNativeBridge = {};
  assert.equal(guardDesktopEntry(), false, "never in a native desktop window, touch-only or not");
  assert.deepEqual([env.cookies, env.reloads], [[], 0]);
  delete globalThis.cageNativeBridge;
  assert.equal(guardDesktopEntry(), true);
  assert.equal(env.reloads, 1);
  assert.match(env.cookies[0], /^cage-layout=phone-auto; Path=\/; SameSite=Lax; Secure$/);
  env.cookies = [];
  assert.equal(guardDesktopEntry(), false, "once per tab");
});

test("the 手机版 switch appears only where the phone UI is offered", () => {
  env.marked = false;
  mountPhoneSwitch();
  assert.equal(env.appended.length, 0);
  env.marked = true;
  globalThis.cageNativeBridge = {};
  mountPhoneSwitch();
  assert.equal(env.appended.length, 0);
  delete globalThis.cageNativeBridge;
  mountPhoneSwitch();
  assert.equal(env.appended.length, 1);
  assert.equal(env.appended[0].textContent, "手机版");
});
