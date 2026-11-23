import test from "node:test";
import assert from "node:assert/strict";
import { answerBody, chipState, confirmText, handoffLine, handoffQuestion, limitPhrase, needsConfirm, refusalNeedsConfirm, resetLabel,
  sessionRule, switchBody, switchTargets, targetNote, targetSelectable, undoBody, waitingLine } from "./session-account-state.mjs";
import { answered, offerChange, preferredLabel, ruleSentence, schedulingBody, showOffer } from "./claude-scheduling-state.mjs";

const reset = Date.UTC(2026, 10, 27, 18, 0) / 1000;
const view = (fields = {}) => ({ handoff_api: 2, source_id: "source", supported: true, session: { account_label: "A" }, ...fields });

test("the limit sentence names the window and its reset", () => {
  assert.equal(resetLabel(reset, "UTC"), "11/27 18:00");
  assert.equal(limitPhrase("A", "seven_day", reset, "UTC"), "A 的额度已用完（周限额，11/27 18:00 重置）");
  assert.equal(limitPhrase("A", "five_hour", 0, "UTC"), "A 的额度已用完（5 小时限额）");
});

test("the first-time question asks once, in one sentence", () => {
  const asked = view({ handoff: { window: "seven_day", resets_at: reset, question: { id: "b", label: "B" } } });
  assert.equal(handoffQuestion(asked, "UTC").text, "A 的额度已用完（周限额，11/27 18:00 重置）。要换到 B 继续吗？会话历史会带到 B。");
  assert.equal(chipState(asked), "额度已用完");
  assert.equal(handoffQuestion(view({ handoff: { window: "seven_day" } })), null);
  // An older server never asks.
  assert.equal(handoffQuestion({ ...asked, handoff_api: undefined }), null);
  assert.deepEqual(answerBody(asked, "always", "r1"), { action: "answer", answer: "always", request_id: "r1", source_id: "source", confirm: true });
});

test("one quiet line per handoff at a limit, none for a switch the person made", () => {
  const session = { account_switch: { phase: "completed", reason: "seven_day", resets_at: reset, source_label: "A", target_label: "B", automatic: true } };
  assert.equal(handoffLine(session, "UTC"), "A 的额度已用完（周限额，11/27 18:00 重置），已换到 B 继续");
  assert.equal(handoffLine({ account_switch: { ...session.account_switch, phase: "starting" } }), "");
  assert.equal(handoffLine({ account_switch: { phase: "completed", target_label: "B" } }), "");
  assert.equal(handoffLine({ account_switch: { phase: "completed", reason: "reset", target_label: "A" } }), "A 的额度已恢复，会话已继续");
  assert.deepEqual(undoBody(view(), "r2"), { action: "undo", request_id: "r2", source_id: "source", confirm: true });
});

test("waiting says who resets first and whether it continues by itself", () => {
  assert.match(waitingLine(view({ handoff: { waiting: "source", window: "five_hour", resets_at: reset } }), "UTC"), /^A 的额度已用完（5 小时限额，11\/27 18:00 重置），正在等它恢复/);
  assert.equal(waitingLine(view({ handoff: { waiting: "all", wait_until: reset, wait_label: "B", automatic: true } }), "UTC"),
    "可以接续的账号额度都已用完：B 将在 11/27 18:00 最先恢复，届时自动继续。也可以在账号菜单中直接选一个账号。");
  assert.doesNotMatch(waitingLine(view({ handoff: { waiting: "all", wait_until: reset, wait_label: "B" } }), "UTC"), /自动继续/);
});

test("every logged-in account is a target, with what to expect", () => {
  const targets = switchTargets(view({ targets: [
    { id: "b", label: "B", status: "ready", enabled: false, remaining: 40 },
    { id: "c", label: "C", status: "ready", enabled: true, exhausted: "five_hour", resets_at: reset, remaining: 0 },
    { id: "d", label: "D", status: "signed_out", enabled: true, remaining: -1 },
    { id: "e", label: "E", status: "ready", enabled: true, remaining: -1 },
  ] }));
  assert.deepEqual(targets.map((t) => targetNote(t, "UTC")), ["剩余 40% · 未参与自动分配", "额度已用完，11/27 18:00 重置", "未登录", "额度未知"]);
  assert.deepEqual(targets.map(targetSelectable), [true, true, false, true]);
  // An older server lists its group candidates.
  assert.deepEqual(switchTargets({ candidates: [{ id: "b", label: "B" }] }).map(targetSelectable), [true]);
});

test("one confirm only while the source is really working", () => {
  assert.equal(needsConfirm(view({ source_busy: true })), true);
  assert.equal(needsConfirm(view({ source_busy: false })), false);
  assert.equal(needsConfirm({ source_busy: true }), false, "an older server has no force");
  assert.equal(confirmText("B"), "会停止当前任务并切换到 B，继续吗？");
  assert.equal(refusalNeedsConfirm(view(), "当前任务仍在运行：确认“停止当前任务并切换到 B”，或等本轮结束。"), true);
  assert.equal(refusalNeedsConfirm(view(), "B 尚未登录：在账号管理中点“检查状态”或重新登录后再切换。"), false);
  assert.deepEqual(switchBody(view(), "b", "r3", { force: true }), { action: "switch", target: "b", request_id: "r3", source_id: "source", confirm: true, continue: true, force: true });
  assert.deepEqual(switchBody({ source_id: "source" }, "b", "r4", { force: true }), { action: "switch", target: "b", request_id: "r4", source_id: "source", confirm: true, continue: false });
});

test("a session follows 调度设置 until it has its own rule", () => {
  assert.equal(sessionRule(view({ switch_policy: "allow", switch_policy_own: false })), "default");
  assert.equal(sessionRule(view({ switch_policy: "wait", switch_policy_own: true })), "wait");
  assert.equal(sessionRule({ switch_policy: "wait" }), "wait");
});

test("调度设置 states the rule in plain words and offers the question once", () => {
  assert.match(ruleSentence({ switch_policy: "ask" }), /第一次有账号额度用完时/);
  assert.match(ruleSentence({ switch_policy: "ask", switch_answered: true }), /问你是否换到另一个账号/);
  assert.match(ruleSentence({ switch_policy: "allow", ungrouped_handoff: true }), /没有设分组的账号之间也会互相接续/);
  assert.match(ruleSentence({ switch_policy: "allow" }), /只在同一接续分组内接续/);
  assert.equal(ruleSentence({ switch_policy: "wait" }), "额度用完时等原账号恢复，不换账号。");
  const accounts = [{ id: "a", status: "ready", enabled: true }, { id: "b", status: "ready", enabled: true }];
  assert.equal(showOffer({ handoff_api: 2, accounts, scheduling: { switch_policy: "ask" } }), true);
  assert.equal(showOffer({ handoff_api: 2, accounts: accounts.slice(1), scheduling: { switch_policy: "ask" } }), false);
  assert.equal(showOffer({ handoff_api: 2, accounts, scheduling: { switch_policy: "allow" } }), false, "an explicit stored choice is kept");
  assert.equal(showOffer({ accounts, scheduling: { switch_policy: "ask" } }), false);
  assert.deepEqual(offerChange({ mode: "balanced", switch_policy: "ask" }, "always"), { mode: "balanced", switch_policy: "allow", switch_answered: true, ungrouped_handoff: true });
  assert.equal(answered(offerChange({ switch_policy: "ask" }, "ask")), true);
  assert.deepEqual(schedulingBody({}, { mode: "balanced", switch_policy: "allow", ungrouped_handoff: true }), { mode: "balanced", switch_policy: "allow" });
  assert.equal(preferredLabel({ scheduling: { preferred: "b" }, accounts: [{ id: "b", label: "B" }] }), "B");
});
