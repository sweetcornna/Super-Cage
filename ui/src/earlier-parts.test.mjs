import test from "node:test";
import assert from "node:assert/strict";
import { clearedWhen, earlierCount, earlierName, earlierPath, earlierTitle } from "./earlier-parts.mjs";

test("earlier part counts come only from the server's field", () => {
  assert.equal(earlierCount({}), 0);
  assert.equal(earlierCount({ earlier_parts: 2 }), 2);
  assert.equal(earlierCount({ earlier_parts: "x" }), 0);
  assert.equal(earlierCount(null), 0);
});

test("earlier part routes for both clients", () => {
  assert.equal(earlierPath("/api/cli/sessions", "a b"), "/api/cli/sessions/a%20b/earlier");
  assert.equal(earlierPath("/cli/sessions", "id", "cli"), "/cli/sessions/id/earlier/cli");
  assert.equal(earlierPath("/api/mobile/cli/sessions", "id", "cli", "separate"), "/api/mobile/cli/sessions/id/earlier/cli/separate");
});

test("clear times read naturally", () => {
  const now = new Date(2026, 10, 22, 18, 0);
  assert.equal(clearedWhen(new Date(2026, 10, 22, 9, 5).toISOString(), now), "今天 09:05");
  assert.equal(clearedWhen(new Date(2026, 10, 21, 23, 59).toISOString(), now), "昨天 23:59");
  assert.equal(clearedWhen(new Date(2026, 10, 2, 14, 30).toISOString(), now), "11月2日 14:30");
  assert.equal(clearedWhen(new Date(2025, 0, 2, 8, 0).toISOString(), now), "2025年1月2日 08:00");
  assert.equal(clearedWhen("not a date", now), "");
});

test("an earlier part's accessible name", () => {
  const now = new Date(2026, 10, 22, 18, 0);
  const part = { cleared_at: new Date(2026, 10, 22, 9, 5).toISOString(), prompt: "整理发布流程", turns: 3 };
  assert.equal(earlierName(part, now), "清除于 今天 09:05：整理发布流程，3 轮");
  assert.equal(earlierTitle({ prompt: "  " }), "（没有提问）");
});
