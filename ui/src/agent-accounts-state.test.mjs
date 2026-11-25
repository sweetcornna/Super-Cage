import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { agentAccountsPath, createAgentAccountsSequencer } from "./agent-accounts-state.mjs";

const srcDir = path.dirname(fileURLToPath(import.meta.url));

function cssRuleBody(css, selector) {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const re = new RegExp(`(?:^|\\}|\\n)\\s*${escaped}\\s*\\{([^}]*)\\}`, "g");
  const bodies = [];
  for (const m of css.matchAll(re)) {
    bodies.push(m[1]);
  }
  return bodies.join("\n");
}

test("claude-participation checkbox overlay is strictly contained inside its label", () => {
  const claudeCSS = fs.readFileSync(path.join(srcDir, "claude-accounts.css"), "utf8");
  const agentCSS = fs.readFileSync(path.join(srcDir, "agent-accounts.css"), "utf8");

  // Confirm the invisible checkbox input uses position:absolute; inset:0.
  const inputRule = cssRuleBody(claudeCSS, ".claude-participation input");
  assert.match(inputRule, /position\s*:\s*absolute/);
  assert.match(inputRule, /inset\s*:\s*0/);

  // Because AgentAccountCard places .claude-participation inside .claude-profile-heading
  // (NOT .claude-profile-actions), .claude-participation itself and the agent-accounts
  // override must both declare position:relative; otherwise the invisible checkbox covers
  // the entire .workbench-main pane and toggles `enabled` on every click anywhere on the page.
  const baseParticipationRule = cssRuleBody(claudeCSS, ".claude-participation");
  assert.match(
    baseParticipationRule,
    /position\s*:\s*relative/,
    "claude-accounts.css must set position: relative on .claude-participation itself",
  );

  const agentParticipationRule = cssRuleBody(agentCSS, ".agent-accounts-page .claude-participation");
  assert.match(
    agentParticipationRule,
    /position\s*:\s*relative/,
    "agent-accounts.css must set position: relative on .agent-accounts-page .claude-participation",
  );
  assert.match(
    agentParticipationRule,
    /cursor\s*:\s*pointer/,
    "agent-accounts.css must set cursor: pointer on .agent-accounts-page .claude-participation",
  );
});

test("agent-accounts.jsx sequences mutate() with load() and preserves workspace", () => {
  const jsx = fs.readFileSync(path.join(srcDir, "agent-accounts.jsx"), "utf8");
  assert.match(
    jsx,
    /agent-accounts-state\.mjs/,
    "agent-accounts.jsx must use the shared state/URL sequencer so in-flight loads cannot overwrite mutations",
  );
  assert.equal(
    agentAccountsPath("antigravity", { workspace: "/proj/app" }),
    "/agent-clis/antigravity/accounts?workspace=%2Fproj%2Fapp",
  );
  assert.equal(
    agentAccountsPath("antigravity", { workspace: "/proj/app", forceRefresh: true }),
    "/agent-clis/antigravity/accounts?workspace=%2Fproj%2Fapp&refresh=1",
  );

  // Stale in-flight load() resolving after a newer mutate() must NOT flap enabled back.
  const seq = createAgentAccountsSequencer();
  let state = { enabled: true };

  const loadTicket = seq.begin();
  const mutateTicket = seq.begin();

  // Mutation completes first -> enabled becomes false.
  if (seq.isCurrent(mutateTicket)) {
    state = { enabled: false };
  }
  assert.equal(state.enabled, false);

  // Earlier load resolves late with stale enabled: true -> must be discarded.
  if (seq.isCurrent(loadTicket)) {
    state = { enabled: true };
  }
  assert.equal(state.enabled, false);

  // Unmount invalidation discards any still-pending request.
  const pending = seq.begin();
  seq.invalidate();
  assert.equal(seq.isCurrent(pending), false);

  // Mutation counter tracks in-flight mutations independently of superseding loads.
  const m1 = seq.beginMutation();
  const supersededByLoad = seq.begin();
  assert.equal(seq.isCurrent(m1), false);
  assert.equal(seq.isCurrent(supersededByLoad), true);
  assert.equal(seq.endMutation(), false, "ending the only in-flight mutation must clear submitting even when superseded by load");

  // Overlapping mutations keep submitting true until the last one completes.
  seq.beginMutation();
  seq.beginMutation();
  assert.equal(seq.endMutation(), true);
  assert.equal(seq.endMutation(), false);
});
