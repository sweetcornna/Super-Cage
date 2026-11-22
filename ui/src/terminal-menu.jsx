import React, { useLayoutEffect, useRef } from "react";
import { ExternalLink, TerminalSquare } from "lucide-react";
import { ProviderMark } from "./provider-mark";

// codexLabel: "Codex 会话" where the server makes Codex sessions from the menu,
// "Codex 终端" (a plain Codex terminal) otherwise (codexMenuLabel).
// local, when given, is "在本机终端中打开" ({label, enabled, reason,
// onSelect}): the desktop app only. Its reason, when disabled, is its tooltip.
export function TerminalMenu({ choice, busy, codex = false, codexLabel = "Codex 终端", local = null, onClose, onSelect }) {
  const menu = useRef(null);
  const close = useRef(onClose);
  close.current = onClose;
  useLayoutEffect(() => {
    const node = menu.current;
    const anchor = choice.anchor;
    if (!anchor?.isConnected) {
      close.current();
      return;
    }
    anchor.setAttribute("aria-expanded", "true");
    anchor.setAttribute("aria-controls", "terminal-create-menu");
    node.querySelector("button:not(:disabled)")?.focus();
    const dismiss = (restoreFocus = false) => {
      if (restoreFocus) anchor.focus();
      close.current();
    };
    const outside = (event) => {
      if (!node.contains(event.target) && !anchor.contains(event.target)) dismiss();
    };
    const keydown = (event) => {
      if (event.key === "Escape" || event.key === "Tab") {
        if (event.key === "Escape") event.preventDefault();
        event.stopPropagation();
        dismiss(true);
      } else if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
        event.preventDefault();
        const items = [...node.querySelectorAll("button:not(:disabled)")];
        const index = items.indexOf(document.activeElement);
        const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1
          : (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
        items[next]?.focus();
      }
    };
    const closeCommand = () => dismiss(true);
    document.addEventListener("click", outside, true);
    document.addEventListener("keydown", keydown, true);
    document.addEventListener("cage-close-modal", closeCommand);
    return () => {
      anchor.setAttribute("aria-expanded", "false");
      anchor.removeAttribute("aria-controls");
      document.removeEventListener("click", outside, true);
      document.removeEventListener("keydown", keydown, true);
      document.removeEventListener("cage-close-modal", closeCommand);
    };
  }, [choice]);

  return (
    <div ref={menu} id="terminal-create-menu" className="menu terminal-create-menu" role="menu" aria-label="新建终端">
      <button role="menuitem" disabled={busy} onClick={() => onSelect("shell")}>
        <TerminalSquare size={16} />普通终端
      </button>
      <button role="menuitem" disabled={busy} onClick={() => onSelect("claude")}>
        <ProviderMark provider="claude" decorative />Claude 终端
      </button>
      {codex && <button role="menuitem" disabled={busy} onClick={() => onSelect("codex")}>
        <ProviderMark provider="codex" decorative />{codexLabel}
      </button>}
      {local && <button role="menuitem" className="local-terminal-item" disabled={busy || !local.enabled} title={local.reason || undefined} onClick={local.onSelect}>
        <ExternalLink size={16} />{local.label}
      </button>}
    </div>
  );
}
