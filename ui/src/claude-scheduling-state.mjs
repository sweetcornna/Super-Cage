// 调度设置: the rule for a used-up quota, in plain words.

export const handoffAPI = (registry) => Number(registry?.handoff_api) || 0;

export function answered(scheduling) {
  return !!scheduling?.switch_answered || scheduling?.switch_policy === "allow" || scheduling?.switch_policy === "wait";
}

export function ruleSentence(scheduling) {
  const policy = scheduling?.switch_policy || "ask";
  if (policy === "allow") {
    return "额度确认用完时，自动换到还有额度、参与分配的账号继续，会话历史随之带过去"
      + (scheduling.ungrouped_handoff ? "；没有设分组的账号之间也会互相接续" : "；只在同一接续分组内接续，没有设分组的账号之间仍会先问你")
      + "。都用完时等最先恢复的账号，届时自动继续。";
  }
  if (policy === "wait") return "额度用完时等原账号恢复，不换账号。";
  return answered(scheduling)
    ? "额度用完时，在会话里问你是否换到另一个账号。"
    : "还没有设定：第一次有账号额度用完时，会在会话里问你一次。";
}

export const MANUAL_SENTENCE = "手动切换不受这些限制：在会话的账号菜单里选哪个账号，就换到哪个。在某个会话里选的规则只管那个会话。";

// The explicit yes in 调度设置 for accounts without a 接续分组.
export const UNGROUPED_LABEL = "没有设接续分组的账号之间也自动接续";

export function ungroupedChange(scheduling, on) {
  return { ...scheduling, ungrouped_handoff: !!on };
}
export const GROUP_SENTENCE = "接续分组只限制自动接续：设了分组的账号只和同组账号互相接续。工作账号和个人账号请分到不同分组，以免会话历史自动进入另一身份。";

// Offered once two logged-in accounts take part, until it is answered.
export function showOffer(registry) {
  if (handoffAPI(registry) < 2 || answered(registry?.scheduling)) return false;
  return (registry?.accounts || []).filter((a) => a.status === "ready" && a.enabled).length >= 2;
}

export const OFFER_TEXT = "某个账号额度用完时，要自动换到另一个账号继续吗？会话历史会带过去。";

export function offerChange(scheduling, choice) {
  const next = { ...scheduling, switch_answered: true };
  if (choice === "always") return { ...next, switch_policy: "allow", ungrouped_handoff: true };
  if (choice === "wait") return { ...next, switch_policy: "wait" };
  return { ...next, switch_policy: "ask" };
}

// What a scheduling change sends: an older server knows only mode and
// switch_policy and refuses unknown fields.
export function schedulingBody(registry, scheduling) {
  if (handoffAPI(registry) >= 2) return scheduling;
  return { mode: scheduling.mode, switch_policy: scheduling.switch_policy };
}

export function preferredLabel(registry) {
  const preferred = registry?.scheduling?.preferred;
  if (!preferred) return "";
  const account = (registry.accounts || []).find((a) => a.id === preferred);
  return account ? account.label || account.email : "";
}
