import React, { useState } from "react";
import { InlineSelect } from "./inline-select";

const modes = {
  manual: "手动切换",
  exhaust: "用完自动切换",
  smart: "智能切换",
};

const originalModes = { balanced: "均衡分配", sequential: "按列表顺序优先" };

export function launchModeLabel(mode, otherModes = originalModes) {
  return modes[mode] || otherModes[mode]?.split("（")[0] || mode;
}

export function LaunchAccountPolicy({ scheduling, accounts = [], enabled, busy, change, label = "新会话分配", legacyModes = originalModes }) {
  const [chooseManual, setChooseManual] = useState(false);
  const options = enabled ? { ...legacyModes, ...modes } : legacyModes;
  const mode = chooseManual ? "manual" : scheduling.mode;
  const selected = scheduling.selected_account_id || "";
  const ready = (accounts || []).filter(a => a.status === "ready" && a.enabled !== false);

  function setMode(value) {
    if (value === "manual" && !ready.some(a => a.id === selected)) {
      setChooseManual(true);
      return;
    }
    setChooseManual(false);
    change({ action: "scheduling", scheduling: enabled ? { mode: value } : { ...scheduling, mode: value } });
  }

  function setAccount(id) {
    if (chooseManual) {
      setChooseManual(false);
      change({ action: "scheduling", scheduling: { mode: "manual", selected_account_id: id } });
    } else {
      change({ action: "select", id });
    }
  }

  const hasOrder = mode === "exhaust" || mode === "sequential";
  const showSelected = hasOrder || mode === "manual";
  return <>
    <label>{label}<InlineSelect inline aria-label={label} value={mode} disabled={busy} onValueChange={setMode}>
      {Object.entries(options).map(([id, name]) => <option key={id} value={id}>{name}</option>)}
    </InlineSelect></label>
    {enabled && <label>切换账号<InlineSelect inline aria-label="切换账号" disabled={busy || !ready.length}
      value={showSelected ? selected : ""} onValueChange={setAccount}>
      <option value="" disabled>{ready.length ? "选择账号" : "暂无可选账号"}</option>
      {(accounts || []).map(a => <option key={a.id} value={a.id} disabled={a.status !== "ready" || a.enabled === false}>
        {a.label || a.email || a.username || a.key_hint || a.id}
      </option>)}
    </InlineSelect></label>}
    {enabled && <p className="field-help">{chooseManual
      ? "请选择一个账号以启用手动切换。"
      : mode === "manual" ? "新会话只使用选定账号。需要换账号时，在这里重新选择。"
      : hasOrder ? "新会话沿用当前账号。它的额度用完或无法使用时，按账号顺序切换。"
      : mode === "smart" ? "新会话按账号负载和可用额度分配。在这里指定账号会启用手动切换。"
      : "在这里指定账号会启用手动切换。"}</p>}
  </>;
}
