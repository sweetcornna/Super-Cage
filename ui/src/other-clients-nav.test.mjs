import assert from "node:assert/strict";
import test from "node:test";
import { entriesOf, isListed, navRowVisible, policyAllows, policyHidesRow } from "./other-clients-nav.mjs";

const raw = (over = {}) => ({ id: "codex", name: "Codex", status: "available", with_environment: false, present: null, allowed: true, ...over });

test("the settings row shows only when something allowed is listed", () => {
  assert.equal(navRowVisible({ agents: [raw()] }), true);
  assert.equal(navRowVisible({ agents: [] }), false);
  assert.equal(navRowVisible({}), false);
  assert.equal(navRowVisible({ agents: [raw({ status: "hidden" })] }), false);
  assert.equal(navRowVisible({ agents: [raw({ allowed: false })] }), false);
  assert.equal(navRowVisible({ agents: [raw({ allowed: false }), raw({ id: "b" })] }), true);
  assert.equal(navRowVisible({ agents: [raw()] }, { allow: [] }), false);
  assert.equal(navRowVisible({ agents: [raw()] }, { deny: ["codex"] }), false);
  assert.equal(navRowVisible({ agents: [raw()] }, { allow: ["codex"] }), true);
  assert.equal(navRowVisible({ agents: [raw()] }, undefined), true);
});

test("agents_policy: allow limits, deny removes, nothing allows all, an empty allow hides without asking", () => {
  assert.equal(policyAllows(undefined, "codex"), true);
  assert.equal(policyAllows({ allow: ["a"] }, "codex"), false);
  assert.equal(policyAllows({ deny: ["codex"] }, "codex"), false);
  assert.equal(policyAllows({ allow: ["codex"], deny: ["codex"] }, "codex"), false);
  assert.equal(policyHidesRow({ allow: [] }), true);
  assert.equal(policyHidesRow({ deny: [] }), false);
  assert.equal(policyHidesRow(null), false);
});

test("the list is read from agents, entries or a bare list", () => {
  assert.equal(entriesOf({ agents: [raw()] }).length, 1);
  assert.equal(entriesOf({ entries: [raw()] }).length, 1);
  assert.equal(entriesOf([raw()]).length, 1);
  for (const payload of [null, {}, "x", { agents: "x" }]) assert.equal(entriesOf(payload), null);
});

test("what the section lists: not hidden, not Claude Code, a withdrawn one only when installed", () => {
  assert.equal(isListed(raw()), true);
  assert.equal(isListed(raw({ status: "hidden" })), false);
  assert.equal(isListed(raw({ with_environment: true })), false);
  assert.equal(isListed(raw({ status: "withdrawn" })), false);
  assert.equal(isListed(raw({ status: "withdrawn", present: { by: "cage", version: "1" } })), true);
  assert.equal(isListed(raw({ id: " " })), false);
  assert.equal(isListed(null), false);
});
