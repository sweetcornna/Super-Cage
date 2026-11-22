import React, { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Globe2, Laptop, MemoryStick, RefreshCw } from "lucide-react";
import { api } from "./api";
import { EnvironmentShield } from "./environment-shield";
import { explanationView } from "./safety-state.mjs";
import { useWindowVisibility } from "./window-visibility";
import "./network-explanation.css";

// The server explains a closed or uncertain gate (status.safety.explanation,
// read by networkState in safety-state.mjs). These views show it where the
// workbench names the gate: a popover from the status bar item and a compact
// block in the home card. Without an explanation (an older server) the
// workbench keeps today's text and opens the checklist as before.

// Re-renders the caller once a second for a countdown: only while active, only
// while the window is visible and only until the moment it counts down to has
// passed. Nothing else runs; a caller that is not mounted has no timer.
export function useNow(active, until) {
  const [now, setNow] = useState(() => Date.now());
  const live = useWindowVisibility() && active && Date.now() <= until + 1000;
  useEffect(() => {
    if (!live) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [live]);
  return now;
}

// A manual full check through the existing API (POST /api/safety/check).
export function useRecheck(onStarted) {
  const [state, setState] = useState({ busy: false, error: "" });
  const run = async () => {
    if (state.busy) return;
    setState({ busy: true, error: "" });
    try {
      await api("/safety/check", {});
      setState({ busy: false, error: "" });
      onStarted?.();
    } catch (error) {
      setState({ busy: false, error: error.message });
    }
  };
  return { ...state, run };
}

// The tone of the gate for the dots beside its label: calm amber unless the
// user has to act.
export const networkTone = (network) => {
  const view = explanationView(network?.explanation);
  return view ? view.tone : "";
};

// What is short of memory, beside the title: the environment, the isolated
// browser (the sidebar's browser icon) or the machine itself.
const memoryIcons = { environment: MemoryStick, browser: Globe2, machine: Laptop };
const memoryNames = { environment: "环境内存", browser: "隔离浏览器内存", machine: "这台机器的内存" };

// onResources opens 设置 → 环境与恢复 → 计算资源; advice about the memory
// setting offers it (adviceAction in safety-state.mjs).
export function NetworkExplanation({ explanation, compact = false, inline = false, canRecheck = false, onRechecked, onDetails, onResources, titleId }) {
  // The home card (compact) shows the reason and one action and does not count down; the popover shows
  // everything, and the settings block (inline) everything but the title.
  const target = explanation?.deadline ?? explanation?.recheckAt ?? explanation?.leaseUntil ?? null;
  const now = useNow(!compact && target !== null, target ?? Infinity);
  const recheck = useRecheck(onRechecked);
  const view = explanationView(explanation, now);
  if (!view) return null;
  const offerRecheck = canRecheck && view.recheck;
  // Compact: the check when the user can run it, else the details.
  const offerDetails = onDetails && (!compact || !offerRecheck);
  const lead = view.reason || (compact ? view.next : "");
  const MemoryIcon = memoryIcons[view.memoryCause];
  return (
    <div className={`network-explanation tone-${view.tone}${compact || inline ? " compact" : ""}`} data-state={view.state} data-memory-cause={view.memoryCause || undefined}>
      {!compact && !inline && (
        <p className="network-explanation-title" id={titleId}>
          {MemoryIcon && <MemoryIcon size={16} className="network-memory-icon" aria-hidden="true" data-cause={memoryNames[view.memoryCause]} />}
          {view.title}
        </p>
      )}
      {lead && <p className="network-explanation-reason">{lead}</p>}
      {!compact && (view.nextParts[0] || view.nextParts[1]) && (
        <p className="network-explanation-next">
          {view.nextParts[0]}
          {view.nextParts[1] && <span className="network-countdown" role="timer">{view.nextParts[1]}</span>}
          {view.nextParts[2]}
        </p>
      )}
      {!compact && view.advice.length > 0 && (
        <ul className="network-explanation-advice" aria-label="你可以">
          {view.advice.map((item, index) => (
            <li key={index}>
              {item.text}
              {item.action === "resources" && onResources && (
                <> <button type="button" className="text-button network-advice-link" onClick={onResources}>打开计算资源设置</button></>
              )}
            </li>
          ))}
        </ul>
      )}
      {(offerRecheck || offerDetails) && (
        <div className="network-explanation-actions">
          {offerRecheck && (
            <button onClick={recheck.run} disabled={recheck.busy} aria-busy={recheck.busy || undefined}>
              <RefreshCw size={14} className={recheck.busy ? "spin" : undefined} aria-hidden="true" />
              {recheck.busy ? "正在开始检测…" : "立即重新检测"}
            </button>
          )}
          {offerDetails && <button className="text-button" onClick={onDetails}>查看检查详情</button>}
        </div>
      )}
      {recheck.error && <p className="network-explanation-error" role="alert">{recheck.error}</p>}
    </div>
  );
}

// The status bar's gate item. With an explanation it opens a small popover
// above itself; without one it opens the checklist, as before. A blocked
// navigation asks it to open through the "cage-network-explain" event.
export function NetworkStatusButton({ network, running, healthy, canRecheck, onRechecked, onDetails, onResources }) {
  const view = explanationView(network?.explanation);
  const [open, setOpen] = useState(false);
  const [place, setPlace] = useState(null);
  const button = useRef(null);
  const popover = useRef(null);
  const explained = Boolean(view && running);
  useEffect(() => {
    if (!explained) setOpen(false);
  }, [explained]);
  useEffect(() => {
    const show = () => { if (explained) setOpen(true); };
    window.addEventListener("cage-network-explain", show);
    return () => window.removeEventListener("cage-network-explain", show);
  }, [explained]);
  useLayoutEffect(() => {
    if (!open) return;
    const measure = () => {
      const rect = button.current?.getBoundingClientRect();
      if (!rect) return;
      const width = Math.min(360, window.innerWidth - 24);
      setPlace({ left: Math.max(12, Math.min(rect.left, window.innerWidth - width - 12)), bottom: window.innerHeight - rect.top + 6, width });
    };
    measure();
    popover.current?.focus();
    const dismiss = (restore) => { setOpen(false); if (restore) button.current?.focus(); };
    const key = (event) => { if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); dismiss(true); } };
    const outside = (event) => {
      if (!popover.current?.contains(event.target) && !button.current?.contains(event.target)) dismiss(false);
    };
    const command = () => dismiss(true);
    window.addEventListener("resize", measure);
    document.addEventListener("keydown", key, true);
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("cage-close-modal", command);
    return () => {
      window.removeEventListener("resize", measure);
      document.removeEventListener("keydown", key, true);
      document.removeEventListener("pointerdown", outside, true);
      document.removeEventListener("cage-close-modal", command);
    };
  }, [open]);
  if (!explained) {
    return (
      <button onClick={onDetails} title="网络与隔离检查">
        <EnvironmentShield size={14} healthy={healthy} />
        <span className="status-label">{running ? network.label : "网络检查"}</span>
      </button>
    );
  }
  return (
    <>
      <button ref={button} className={`network-status tone-${view.tone}`} onClick={() => setOpen((value) => !value)}
        aria-expanded={open} aria-haspopup="dialog" aria-controls={open ? "network-popover" : undefined} title="外网状态说明">
        <EnvironmentShield size={14} healthy={healthy} />
        <i className={`network-tone-dot tone-${view.tone}`} aria-hidden="true" />
        <span className="status-label">{view.label}</span>
      </button>
      {open && createPortal(
        <div ref={popover} id="network-popover" className="network-popover" role="dialog" aria-labelledby="network-popover-title" tabIndex={-1}
          style={place ? { left: place.left, bottom: place.bottom, width: place.width } : { visibility: "hidden" }}>
          <NetworkExplanation explanation={network.explanation} canRecheck={canRecheck} titleId="network-popover-title"
            onRechecked={() => { setOpen(false); onRechecked?.(); }}
            onDetails={() => { setOpen(false); onDetails?.(); }}
            onResources={onResources && (() => { setOpen(false); onResources(); })} />
        </div>,
        document.body,
      )}
    </>
  );
}
