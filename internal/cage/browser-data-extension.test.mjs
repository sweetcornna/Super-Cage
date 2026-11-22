import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createContext, runInContext } from "node:vm";

// The extension page's bridge (split-extension/data.js) with Chromium's APIs
// replaced by fakes that behave like Chrome 157 where it matters: remove
// rejects the whole call for one invalid origin, search honours endTime and
// returns the newest entries first.
function page({ history = [] } = {}) {
  const calls = [];
  const chrome = {
    browsingData: { remove: async (options, types) => {
      for (const origin of [...(options.origins || []), ...(options.excludeOrigins || [])]) {
        let valid = false;
        try { valid = new URL(origin).origin !== "null"; } catch {}
        if (!valid) throw new Error("'" + origin + "' is not a valid origin.");
      }
      calls.push({ options, types });
    } },
    history: {
      search: async ({ text, startTime = 0, endTime = Infinity, maxResults }) => history
        .filter(item => item.lastVisitTime >= startTime && item.lastVisitTime < endTime && (item.url.includes(text) || item.title.includes(text)))
        .sort((a, b) => b.lastVisitTime - a.lastVisitTime).slice(0, maxResults),
      deleteUrl: async ({ url }) => { history = history.filter(item => item.url !== url); calls.push({ deleted: url }); },
    },
  };
  const context = createContext({ chrome, URL });
  runInContext(readFileSync(new URL("./split-extension/data.js", import.meta.url), "utf8"), context);
  // Values cross from the page's realm as plain data, as returnByValue does.
  return { call: async input => JSON.parse(JSON.stringify(await context.cageBrowsingData(input))), calls, remaining: () => history };
}

test("finding 2: invalid origins are dropped and counted, never sent; an empty list removes nothing", async () => {
  const p = page();
  const result = await p.call({ op: "remove", since: 0, excludeOrigins: ["https://www.192.168.1.10", "http://a.example", "https://foo.123"], types: { cookies: true } });
  assert.deepEqual(result, { ok: true, skipped: 2 });
  assert.deepEqual(p.calls[0].options.excludeOrigins, ["http://a.example"]);
  const none = await p.call({ op: "remove", since: 0, origins: ["https://www.1.2.3"], types: { cookies: true } });
  assert.deepEqual(none, { ok: true, skipped: 1 });
  assert.equal(p.calls.length, 1, "a filter that lost every origin never becomes a clear of all sites");
});

test("finding 12: history entries are bounded per entry and the full address comes by id", async () => {
  const long = "https://example.com/?" + "q".repeat(12000);
  const p = page({ history: [{ id: "1", url: long, title: "x".repeat(1000), lastVisitTime: 100.5, visitCount: 1 }, { id: "2", url: "https://example.com/b", title: "b", lastVisitTime: 90, visitCount: 1 }] });
  const { items } = await p.call({ op: "history-page", query: "", end: 1000, limit: 10 });
  assert.equal(items[0].url.length, 2048);
  assert.equal(items[0].title.length, 300);
  assert.equal(items[0].truncated, true);
  assert.equal(items[0].id, "1");
  assert.equal(items[1].truncated, undefined);
  assert.deepEqual(await p.call({ op: "history-url", id: "1", time: 100.5 }), { url: long });
  assert.deepEqual(await p.call({ op: "history-url", id: "9", time: 100.5 }), { url: "" });
});

test("finding 12: deleting a site's history walks past a thousand unrelated matches", async () => {
  const history = [];
  for (let i = 0; i < 1500; i++) history.push({ id: "o" + i, url: "https://other.test/?ref=keep.example&i=" + i, title: "", lastVisitTime: 10_000 + i, visitCount: 1 });
  for (let i = 0; i < 30; i++) history.push({ id: "k" + i, url: "https://app.keep.example/" + i, title: "", lastVisitTime: 100 + i, visitCount: 1 });
  const p = page({ history });
  const result = await p.call({ op: "history-delete-host", host: "keep.example" });
  assert.equal(result.removed, 30);
  assert.equal(p.remaining().filter(item => item.url.includes("app.keep.example")).length, 0);
  assert.equal(p.remaining().length, 1500);
});

test("finding 12: deleting a long entry by id removes the whole address", async () => {
  const long = "https://example.com/?" + "q".repeat(12000);
  const p = page({ history: [{ id: "1", url: long, title: "", lastVisitTime: 50, visitCount: 1 }] });
  assert.deepEqual(await p.call({ op: "history-delete-entries", entries: [{ id: "1", time: 50 }, { id: "2", time: 50 }] }), { ok: true, removed: 1 });
  assert.equal(p.remaining().length, 0);
});
