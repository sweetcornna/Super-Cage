import { shortcut } from "./platform.mjs";
import React, { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {
  FileCode2,
  Anchor,
  Archive,
  LoaderCircle,
  Monitor,
  ChevronDown,
  Globe2,
  Settings,
  Pencil,
  Pin,
  PinOff,
  TerminalSquare,
  History,
  X,
} from "lucide-react";
import { EarlierPartsDialog } from "./earlier-parts-dialog";
import { tabName } from "./workspace-state.mjs";
import { ProviderMark } from "./provider-mark";
import { AgentIcon } from "./agent-icon";
import { api } from "./api";

const terminalProvider = (tab) => tab.sessionProvider === "codex" || tab.cli_session_provider === "codex" || /^codex/.test(tab.terminalKind || "") ? "codex" : "claude";
// A Codex session another device or process writes: a quiet chip at the row's end.
const ownerChips = {
  phone: { label: "手机", tooltip: "手机上的一轮正在运行，点按以只读方式查看" },
  external: { label: "外部", tooltip: "在其他 Codex 进程中打开" },
};

export function TabIcon({ kind, favicon, agent, provider = "claude", agentId = "", glyph = "", accentColor = "", accountNumber, size = 16 }) {
  const [failed, setFailed] = useState("");
  if (kind === "t3code") return <AgentIcon agentId="t3code" glyph={glyph || "T3"} accentColor={accentColor} size={size} />;
  if (kind === "magpie") return <AgentIcon agentId="magpie" glyph={glyph || "MP"} accentColor={accentColor} size={size} />;
  if (kind === "terminal" && agentId && agentId !== "claude" && agentId !== "codex") {
    return <AgentIcon agentId={agentId} glyph={glyph} accentColor={accentColor} accountNumber={accountNumber} size={Math.max(18, size)} />;
  }
  if (kind === "terminal" && agent) return <ProviderMark provider={provider} accountNumber={accountNumber} />;
  if (kind === "terminal") return <TerminalSquare size={16} />;
  if (kind === "editor") return <FileCode2 size={size} />;
  if (kind === "browser-settings") return <Settings size={size} />;
  if (typeof favicon === "string" && favicon.length <= 8192 &&
      /^data:image\/png;base64,[A-Za-z0-9+/=]+$/.test(favicon) && failed !== favicon)
    return <img src={favicon} className="tab-favicon" alt="" width={size} height={size}
      draggable={false} onError={() => setFailed(favicon)} />;
  return <Globe2 size={size} />;
}

function TerminalAction({ label, icon: Icon, disabled, onClick }) {
  return (
    <button className="icon-button" aria-label={label} disabled={disabled} onClick={onClick}>
      <span className="terminal-action-label" aria-hidden="true">{label}</span>
      <Icon size={14} />
    </button>
  );
}

// Active content carries the strongest emphasis; titles and actions use the
// existing paper/selected/muted tokens, 14px type and 8px spacing in both layouts.
export function WorkspaceTabs({
  tabs,
  saved,
  active,
  label,
  horizontal = false,
  vertical = false,
  busy,
  ready,
  agentsById = null,
  onSelect,
  onClose,
  onRename,
  onPin,
  onReorder,
  splitDrag,
  openingSession,
  sessionOpenError,
}) {
  const [menu, setMenu] = useState(null);
  const [earlier, setEarlier] = useState(null);
  // 本机终端: "保留" on a terminal an ssh session opened, so it outlives the
  // connection; the terminal list's next revision shows the change.
  const [keepError, setKeepError] = useState(null);
  const keep = (tab) => {
    setKeepError(null);
    api("/terminals/" + encodeURIComponent(tab.sessionId) + "/keep", {}).catch((error) => setKeepError({ id: tab.id, message: error.message }));
  };
  const [drag, setDrag] = useState(null);
  const [allOpen, setAllOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [width, setWidth] = useState(0);
  const pointer = useRef(null);
  const press = useRef(0);
  // Until when the click that ends a long press is ignored (it would land on the menu's backdrop).
  const settle = useRef(0);
  // Under a finger the sidebar's rows scroll and the browser tabs may too, so a drag cannot reorder them:
  // a long press on a row opens a menu to move it, and tabs keep a readable width and scroll sideways
  // (touch.css).
  const suppressClick = useRef(false);
  const trigger = useRef();
  const list = useRef();
  useEffect(() => {
    if (!horizontal || !list.current) return;
    const observer = new ResizeObserver(([entry]) =>
      setWidth(entry.contentRect.width),
    );
    observer.observe(list.current);
    return () => observer.disconnect();
  }, [horizontal]);
  const gap = tabs.length > 20 ? 1 : 4;
  const available = Math.max(0, width - gap * Math.max(0, tabs.length - 1));
  const minTab = horizontal && matchMedia("(pointer: coarse) and (hover: none)").matches ? 156 : 0;
  const regularWidth = Math.max(minTab, Math.min(220, available / Math.max(1, tabs.length)));
  const activeWidth =
    tabs.length > 1 && tabs.some((tab) => tab.id === active)
      ? Math.min(220, Math.max(regularWidth, Math.min(160, available * 0.4)))
      : regularWidth;
  const otherWidth = Math.max(
    minTab,
    (available - activeWidth) / Math.max(1, tabs.length - 1),
  );
  const tabWidth = (id) =>
    id === active ? activeWidth : Math.min(220, otherWidth);
  const scrolls = minTab > 0 && tabs.length * (minTab + gap) - gap > width;
  const matchingTabs = tabs.filter((tab) =>
    `${tabName(tab, saved)} ${tab.url || ""}`
      .toLowerCase()
      .includes(query.toLowerCase()),
  );
  const firstFocusable = tabs.findIndex(tab => tab.historySession
    ? !busy && openingSession !== tab.cli_session_id && !tab.sessionRunningElsewhere
    : ready || tab.kind === "terminal");
  const finishDrag = (commit, event) => {
    clearTimeout(press.current);
    settle.current = Math.min(settle.current, performance.now() + 400);
    const current = pointer.current;
    pointer.current = null;
    setDrag(null);
    splitDrag.clear();
    if (!current?.moved) return;
    suppressClick.current = true;
    if (!commit) return;
    const { clientX, clientY } = event;
    if (!tabs.find(tab => tab.id === current.id)?.historySession && splitDrag.drop(current.id, clientX, clientY)) return;
    const over = document.elementFromPoint(clientX, clientY);
    if (!list.current?.contains(over)) return;
    if (horizontal) onReorder(current.id, null, current.order);
    else {
      const target = over?.closest("[data-tab-id]")?.dataset.tabId;
      if (target && target !== current.id) onReorder(current.id, target);
    }
  };
  useEffect(() => {
    if (!drag) return;
    const cancel = (event) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopImmediatePropagation();
      finishDrag(false);
    };
    document.addEventListener("keydown", cancel, true);
    return () => document.removeEventListener("keydown", cancel, true);
  }, [Boolean(drag)]);
  useEffect(() => () => splitDrag.clear(), []);
  const startDrag = (event, tab) => {
    suppressClick.current = false;
    if (busy || (!ready && tab.kind !== "terminal") || event.button !== 0 ||
        !event.isPrimary || event.target.closest(".tab-close, .terminal-row-actions, .session-owner")) return;
    const items = [...list.current.querySelectorAll(":scope > [data-tab-id]")].map((node) => ({
      id: node.dataset.tabId,
      left: node.offsetLeft,
      width: node.getBoundingClientRect().width,
    }));
    const group = horizontal || vertical ? items.filter(item =>
      Boolean(saved[item.id]?.pinned) === Boolean(saved[tab.id]?.pinned)) : items;
    pointer.current = {
      id: tab.id, startX: event.clientX, startY: event.clientY,
      items, group, order: items.map(item => item.id), moved: false,
    };
    clearTimeout(press.current);
    if (!horizontal && !vertical && event.pointerType === "touch") {
      const row = event.currentTarget.firstElementChild;
      press.current = setTimeout(() => {
        if (!pointer.current || pointer.current.moved) return;
        settle.current = Infinity;
        openMenu(tab, row, true);
      }, 450);
    }
    (event.target.closest("button") || event.currentTarget).setPointerCapture(event.pointerId);
  };
  const moveDrag = (event) => {
    const current = pointer.current;
    if (!current) return;
    if (!event.buttons) return finishDrag(false);
    let dx = event.clientX - current.startX;
    // A fingertip wanders a few pixels during a tap: only a longer move is a drag.
    if (!current.moved && Math.hypot(dx, event.clientY - current.startY) < (event.pointerType === "touch" ? 10 : 5)) return;
    current.moved = true;
    const dropSide = tabs.find(tab => tab.id === current.id)?.historySession ? null : splitDrag.hover(current.id, event.clientX, event.clientY);
    if (!horizontal || dropSide) {
      setDrag({ id: current.id, offsets: {}, widths: {} });
      return;
    }
    const item = current.items.find(item => item.id === current.id);
    // Clamp the tab's drawing to its group, but let the pointer cross the last tab's center.
    const center = item.left + item.width / 2 + dx;
    dx = Math.max(
      current.group[0].left - item.left,
      Math.min(
        dx,
        current.group.at(-1).left +
          current.group.at(-1).width -
          item.left -
          item.width,
      ),
    );
    const rest = current.group.filter((other) => other.id !== item.id);
    const target = rest.filter(
      (other) => center > other.left + other.width / 2,
    ).length;
    const groupOrder = rest.map((other) => other.id);
    groupOrder.splice(target, 0, item.id);
    let at = 0;
    current.order = current.items.map((other) =>
      current.group.includes(other) ? groupOrder[at++] : other.id,
    );
    let left = current.items[0].left;
    const offsets = {};
    for (const id of current.order) {
      const original = current.items.find((other) => other.id === id);
      offsets[id] = id === item.id ? dx : left - original.left;
      left += original.width + gap;
    }
    setDrag({
      id: item.id,
      offsets,
      widths: Object.fromEntries(
        current.items.map((item) => [item.id, item.width]),
      ),
    });
  };
  // The tab's menu under its button; order: only the move up / down choices (a long press on a sidebar row).
  const openMenu = (tab, button, order) => {
    trigger.current = button;
    const rect = button.getBoundingClientRect();
    setMenu({
      tab,
      order,
      left: Math.max(8, Math.min(rect.left, window.innerWidth - 224)),
      top: Math.max(8, Math.min(rect.bottom + 4, window.innerHeight - 152)),
    });
  };
  const showMenu = (tab, event) => {
    event.preventDefault();
    if (!horizontal && !vertical) {
      event.currentTarget.focus();
      event.currentTarget.parentElement.querySelector(".terminal-row-actions button:not(:disabled)")?.focus();
      return;
    }
    openMenu(tab, event.currentTarget);
  };
  useEffect(() => {
    if (horizontal && !scrolls) return;
    list.current
      ?.querySelector('[aria-selected="true"]')
      ?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [active, horizontal, scrolls, tabs.length]);
  useEffect(() => {
    if (!menu && !allOpen) return;
    const dismiss = (event) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        setMenu(null);
        setAllOpen(false);
        trigger.current?.focus();
      }
    };
    const outside = (event) => {
      if (event.target.closest(".all-tabs-menu") || trigger.current?.contains(event.target)) return;
      setAllOpen(false);
    };
    document.addEventListener("keydown", dismiss);
    document.addEventListener("click", outside, true);
    return () => {
      document.removeEventListener("keydown", dismiss);
      document.removeEventListener("click", outside, true);
    };
  }, [menu, allOpen]);
  const at = menu?.order ? tabs.findIndex((tab) => tab.id === menu.tab.id) : -1;
  const actions = menu && (
    <div className="menu workspace-tab-menu"
      style={{ top: menu.top, left: menu.left }}
      role="menu" aria-label={menu.order ? "调整顺序" : "标签操作"}>
      {menu.order ? [["上移", -1], ["下移", 1]].map(([text, step]) => (
        <button key={step} role="menuitem" disabled={!tabs[at + step]}
          onClick={() => { setMenu(null); onReorder(menu.tab.id, tabs[at + step].id); }}>
          {text}
        </button>
      )) : <>
              <button
                role="menuitem"
                onClick={() => {
                  setMenu(null);
                  onRename(menu.tab);
                }}
              >
                重命名标签
              </button>
              <button
                role="menuitem"
                onClick={() => {
                  setMenu(null);
                  onPin(menu.tab.id);
                }}
              >
                <Pin size={14} />
                {saved[menu.tab.id]?.pinned ? "取消置顶" : "置顶标签"}
              </button>
              {menu.tab.earlierSession && (
                <button
                  role="menuitem"
                  onClick={() => {
                    setMenu(null);
                    setEarlier(menu.tab.earlierSession);
                  }}
                >
                  <History size={14} />
                  清除前的对话
                </button>
              )}
              {menu.tab.origin?.kind === "ssh" && !menu.tab.origin.kept && (
                <button
                  role="menuitem"
                  title="连接断开后，这个终端继续运行"
                  onClick={() => {
                    setMenu(null);
                    keep(menu.tab);
                  }}
                >
                  保留终端
                </button>
              )}
              <button
                role="menuitem"
                onClick={() => {
                  setMenu(null);
                  onClose(menu.tab);
                }}
              >
                <X size={15} />
                关闭标签<kbd>{shortcut("⌘W")}</kbd>
              </button>
      </>}
    </div>
  );
  return (
    <>
      <div
        ref={list}
        role="tablist"
        aria-label={label}
        aria-orientation={horizontal ? "horizontal" : "vertical"}
        className={
          horizontal || vertical
            ? `browser-tabs ${vertical ? "browser-tabs-vertical" : ""} ${drag ? "is-dragging" : ""} ${scrolls ? "scrolls" : ""}`
            : "sidebar-tabs"
        }
        style={
          horizontal
            ? {
                gap,
                width: tabs.length * 220 + gap * Math.max(0, tabs.length - 1),
                flex: "0 1 auto",
              }
            : undefined
        }
      >
        {tabs.map((tab, index) => {
          const title = tabName(tab, saved);
          const opening = Boolean(openingSession) && openingSession === tab.cli_session_id;
          const runningElsewhere = tab.sessionRunningElsewhere;
          const runningNotice = tab.sessionClaudeDesktop
            ? "Claude Desktop 占用中，请先在 Claude Desktop 中退出此会话再接续"
            : "其他窗口占用中，退出原会话后可接续";
          const OwnerIcon = tab.sessionClaudeDesktop ? Monitor : TerminalSquare;
          const openError = tab.historySession && sessionOpenError?.id === tab.cli_session_id ? sessionOpenError.message : "";
          const ownerChip = !horizontal && !vertical && ownerChips[tab.sessionOwner];
          const errorID = `session-open-error-${tab.cli_session_id}`;
          // The row's chip: the account (the provider is in the mark, so the label
          // is the account alone), or where an ssh terminal came from.
          const chip = ownerChip ? null : (tab.claudeLabel || tab.account_label) ? { text: (tab.claudeLabel || tab.account_label).replace(/^Codex · /, "") }
            : tab.origin?.kind === "ssh" ? { text: `来自 ${tab.origin.device}${tab.origin.kept ? " · 已保留" : ""}`,
              title: tab.origin.kept ? "来自本机终端，已保留：在本机终端设置里结束" : "来自本机终端，连接断开时结束" } : null;
          const chipElement = chip && <small className="claude-tab-profile" title={chip.title}>{chip.text}</small>;
          return (
            <React.Fragment key={tab.id}>
            <div
              data-tab-id={tab.id}
              className={`${horizontal || vertical ? "browser-tab" : "sidebar-tab"} ${runningElsewhere ? "session-occupied" : ""} ${tab.id === active ? "active" : ""} ${tab.closed ? "ended" : ""} ${drag?.id === tab.id ? "dragging" : ""} ${horizontal && tabWidth(tab.id) < 96 ? "compact" : ""} ${horizontal && tabWidth(tab.id) < 40 ? "tiny" : ""} ${horizontal && tabWidth(tab.id) < 24 ? "sliver" : ""}`}
              style={
                horizontal
                  ? {
                      width: drag?.widths[tab.id] ?? tabWidth(tab.id),
                      transform: drag
                        ? `translateX(${drag.offsets[tab.id] || 0}px)`
                        : undefined,
                    }
                  : undefined
              }
              onPointerDown={(event) => startDrag(event, tab)}
              onPointerMove={moveDrag}
              onPointerUp={(event) => finishDrag(true, event)}
              onPointerCancel={() => finishDrag(false)}
              onLostPointerCapture={() => {
                if (pointer.current) finishDrag(false);
              }}
              onClickCapture={(event) => {
                if (suppressClick.current || performance.now() < settle.current) {
                  event.preventDefault();
                  event.stopPropagation();
                  suppressClick.current = false;
                }
              }}
            >
              <button
                role={tab.historySession ? undefined : "tab"}
                className={tab.historySession ? "sidebar-session" : undefined}
                data-codex-session={tab.cli_session_id && terminalProvider(tab) === "codex" ? "" : undefined}
                aria-busy={opening || undefined}
                aria-describedby={openError ? errorID : undefined}
                aria-description={runningElsewhere ? runningNotice : ownerChip ? ownerChip.tooltip : undefined}
                aria-label={tab.claudeLabel ? `${title} · ${tab.claudeLabel}` : title}
                aria-selected={tab.historySession ? undefined : tab.id === active}
                tabIndex={
                  tab.id === active ||
                  (!tabs.some((t) => t.id === active) && index === firstFocusable)
                    ? 0
                    : -1
                }
                disabled={tab.historySession ? busy || opening || runningElsewhere : !ready && tab.kind !== "terminal"}
                title={runningElsewhere ? `${title}\n${runningNotice}` : ownerChip ? `${title}\n${ownerChip.tooltip}` : tab.url ? `${title}\n${tab.url}` : [title, tab.claudeAccountNumber && `账号 ${tab.claudeAccountNumber}`, tab.claudeLabel].filter(Boolean).join(" · ")}
                onClick={() => onSelect(tab.id)}
                onContextMenu={(event) => showMenu(tab, event)}
                onKeyDown={(event) => {
                  if (!horizontal && !vertical && event.altKey && ["ArrowUp", "ArrowDown"].includes(event.key)) {
                    event.preventDefault();
                    const target = tabs[index + (event.key === "ArrowUp" ? -1 : 1)];
                    if (target) onReorder(tab.id, target.id);
                    return;
                  }
                  if (event.key === "F10" && event.shiftKey) {
                    showMenu(tab, event);
                    return;
                  }
                  const prev = horizontal ? "ArrowLeft" : "ArrowUp";
                  const next = horizontal ? "ArrowRight" : "ArrowDown";
                  if (![prev, next, "Home", "End"].includes(event.key)) return;
                  event.preventDefault();
                  const buttons = [...list.current.querySelectorAll('[role="tab"], .sidebar-session')];
                  const enabled = buttons.map((button, at) => ({button, at})).filter(item => !item.button.disabled);
                  const from = enabled.findIndex(item => item.at === index);
                  const target = enabled[
                    event.key === "Home"
                      ? 0
                      : event.key === "End"
                        ? enabled.length - 1
                        : (from +
                            (event.key === prev ? -1 : 1) +
                            enabled.length) %
                          enabled.length];
                  target?.button.focus();
                  if (target && !tabs[target.at].historySession) onSelect(tabs[target.at].id, false);
                }}
              >
                <TabIcon kind={tab.isT3Code ? "t3code" : tab.isMagpie ? "magpie" : tab.kind} favicon={tab.favicon} size={15}
                  agentId={tab.agent_id || (tab.terminalKind === "herdr" || tab.herdr_imported ? "herdr" : tab.terminalKind === "magpie-tui" ? "magpie" : "")}
                  glyph={tab.agentGlyph || (tab.agent_id ? agentsById?.get?.(tab.agent_id)?.spec?.glyph : "") || ""}
                  accentColor={tab.agentAccentColor || (tab.agent_id ? agentsById?.get?.(tab.agent_id)?.spec?.accent_color : "") || ""}
                  agent={tab.terminalKind === "claude" || tab.terminalKind === "claude-login" || tab.terminalKind === "codex" || tab.terminalKind === "codex-login" || tab.terminalKind === "agent" || tab.terminalKind === "agent-login" || Boolean(tab.cli_session_id) || Boolean(tab.claude_account_id) || Boolean(tab.agent_id)}
                  provider={terminalProvider(tab)} accountNumber={tab.claudeAccountNumber} />
                {/* A phone or external row names its owner instead; the account is in the label and tooltip.
                    In the sidebar the title and the chip share one line: a chip that does not fit whole is not shown (cli-sessions.css). */}
                {chip && !horizontal && !vertical
                  ? <i className="session-text"><span>{title}</span>{chipElement}</i>
                  : <><span>{title}</span>{chipElement}</>}
                {saved[tab.id]?.pinned && <Pin size={11} className="tab-pin" />}
                {ownerChip && <small className="chip session-owner-chip">{ownerChip.label}</small>}
                {!horizontal && !vertical && (opening
                  ? <LoaderCircle size={13} className="spin session-activity" role="img" aria-label="正在打开会话" />
                  : tab.sessionActive && !runningElsewhere &&
                    <i className="live-dot session-activity" role="img" aria-label="会话正在运行" />)}
              </button>
              {!horizontal && !vertical && runningElsewhere &&
                <span className="session-owner" role="img" aria-label={runningNotice} tabIndex={0}>
                  <OwnerIcon size={15} aria-hidden="true" />
                </span>}
              {horizontal || vertical ? (
                <button
                  className="tab-close icon-button"
                  aria-label={`关闭 ${title}`}
                  disabled={busy}
                  onClick={() => onClose(tab)}
                >
                  <X size={14} />
                </button>
              ) : (
                <div className="terminal-row-actions" role="group" aria-label={`${title} 的操作`}>
                  <TerminalAction label={saved[tab.id]?.pinned ? "取消置顶" : "置顶"}
                    icon={saved[tab.id]?.pinned ? PinOff : Pin} disabled={busy} onClick={() => onPin(tab.id)} />
                  {tab.origin?.kind === "ssh" && !tab.origin.kept &&
                    <TerminalAction label="保留终端" icon={Anchor} disabled={busy} onClick={() => keep(tab)} />}
                  <TerminalAction label="重命名" icon={Pencil} disabled={busy} onClick={() => onRename(tab)} />
                  {tab.earlierSession && <TerminalAction label="清除前的对话" icon={History} disabled={busy} onClick={() => setEarlier(tab.earlierSession)} />}
                  <TerminalAction label={tab.cli_session_id ? "归档" : "关闭终端"} icon={tab.cli_session_id ? Archive : X} disabled={busy} onClick={event => onClose(tab, event.currentTarget)} />
                </div>
              )}
            </div>
            {openError && <p id={errorID} className="sidebar-session-open-error" role="alert">{openError}</p>}
            {keepError?.id === tab.id && <p className="sidebar-session-open-error" role="alert">{keepError.message}</p>}
            </React.Fragment>
          );
        })}
      </div>
      {horizontal && (
        <div className="browser-all-tabs">
          <button
            className="icon-button"
            aria-label="搜索标签页"
            title="搜索标签页"
            aria-expanded={allOpen}
            onClick={(event) => {
              trigger.current = event.currentTarget;
              setAllOpen(!allOpen);
              setQuery("");
            }}
          >
            <ChevronDown size={16} />
          </button>
          {allOpen && (
            <>

              <div
                className="menu all-tabs-menu"
                role="dialog"
                aria-label="搜索标签页"
              >
                <input
                  autoFocus
                  aria-label="搜索已打开的标签页"
                  placeholder="搜索标签页"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                />
                <div className="all-tabs-results">
                  {matchingTabs.length === 0 && (
                    <p className="favorites-empty">没有匹配的标签页</p>
                  )}
                  {matchingTabs.map((tab) => (
                    <button
                      key={tab.id}
                      title={tab.url}
                      onClick={() => {
                        setAllOpen(false);
                        onSelect(tab.id);
                      }}
                    >
                      <TabIcon kind={tab.isT3Code ? "t3code" : tab.isMagpie ? "magpie" : tab.kind} favicon={tab.favicon}
                        agentId={tab.agent_id || (tab.terminalKind === "herdr" || tab.herdr_imported ? "herdr" : tab.terminalKind === "magpie-tui" ? "magpie" : "")}
                        glyph={tab.agentGlyph || (tab.agent_id ? agentsById?.get?.(tab.agent_id)?.spec?.glyph : "") || ""}
                        accentColor={tab.agentAccentColor || (tab.agent_id ? agentsById?.get?.(tab.agent_id)?.spec?.accent_color : "") || ""}
                        agent={tab.terminalKind === "claude" || tab.terminalKind === "claude-login" || tab.terminalKind === "codex" || tab.terminalKind === "codex-login" || tab.terminalKind === "agent" || tab.terminalKind === "agent-login" || Boolean(tab.cli_session_id) || Boolean(tab.claude_account_id) || Boolean(tab.agent_id)}
                        provider={terminalProvider(tab)} accountNumber={tab.claudeAccountNumber} />
                      <span>{tabName(tab, saved)}</span>
                      {tab.id === active && (
                        <span className="current-tab-marker">当前</span>
                      )}
                    </button>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>
      )}
      {menu &&
        createPortal(
          <>
            <div className="menu-dismiss" onClick={() => { if (performance.now() >= settle.current) setMenu(null); }} />
            {actions}
          </>,
          document.body,
        )}
      {earlier && <EarlierPartsDialog session={earlier} onClose={() => setEarlier(null)} />}
    </>
  );
}
