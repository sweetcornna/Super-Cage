import React, { Suspense, lazy, useCallback, useEffect, useRef, useState } from "react";
import { ArrowDownToLine, RefreshCw } from "lucide-react";
import { api, native, nativeAvailable } from "./api";
import { activeShellPhases, appUpdateButton, statusNeedsWatch } from "./app-update-state.mjs";
import "./app-updates.css";

const checkInterval = 6 * 60 * 60 * 1000;
const retryInterval = 5 * 60 * 1000;
// The workbench's view (mode, waiting, last attempt, post-update chain) is
// read on focus, visibility and update events, and once a minute only while
// it can change by itself (statusNeedsWatch): manual mode polls nothing.
const statusInterval = 60 * 1000;

// The existing environment actions a failed post-update step offers.
const environmentActions = {
  start_environment: "/start",
  update_components: "/environment/update",
  update_tools: "/environment/tools",
  check_network: "/safety/check",
};

// A shell that reports "manual" without a version is a build updated by its
// own package (Linux, client-only, trial): checking again tells it nothing.
// "manual" with a version is a newer release that only the release page
// installs; it is checked again like any other state.
const checkable = state => ["idle", "current", "failed", "waiting", "available"].includes(state.phase) || (state.phase === "manual" && Boolean(state.latestVersion));

export function useAppUpdates() {
  const [state, setState] = useState({ phase: "idle", message: "" });
  const [status, setStatus] = useState(null);
  const [statusError, setStatusError] = useState(null);
  const [startupFailure, setStartupFailure] = useState("");
  const [retryClock, setRetryClock] = useState(Date.now);
  const retryAt = Date.parse(state.retryAt || "");
  const coolingDown = state.phase === "failed" && retryAt > Date.now();
  const current = useRef(state);
  current.current = state;
  const requestPending = useRef(false);
  const lastCheck = useRef(0);
  const available = nativeAvailable();
  const run = useCallback(async (operation, extra = {}) => {
    if (!available || requestPending.current || activeShellPhases.has(current.current.phase)) return;
    if (["check", "install"].includes(operation) && Date.parse(current.current.retryAt || "") > Date.now()) return;
    requestPending.current = true;
    if (operation === "check") lastCheck.current = Date.now();
    if (operation === "install") setState(previous => ({ ...previous, phase: "checking" }));
    try {
      const next = await native("appUpdate", { operation, ...extra });
      if (next?.phase) setState(next);
    } catch (error) {
      setState(previous => ({ ...previous, phase: "failed", message: error.message, retryable: true }));
    } finally { requestPending.current = false; }
  }, [available]);
  const refreshStatus = useCallback(async () => {
    if (!available) return;
    try {
      const next = await api("/app-update");
      // An older workbench has no such route; its answer is not a status.
      setStatus(typeof next?.mode === "string" ? next : null);
      setStatusError(null);
    } catch (error) {
      setStatus(null);
      setStatusError(error.status === 404 ? null : error);
    }
  }, [available]);

  useEffect(() => {
    if (!coolingDown) return;
    const timer = setTimeout(() => setRetryClock(Date.now()), Math.min(60000, retryAt - Date.now()));
    return () => clearTimeout(timer);
  }, [coolingDown, retryAt, retryClock]);

  useEffect(() => {
    if (!available) return;
    let live = true;
    const receive = event => {
      if (event.detail?.phase) setState(event.detail);
      void refreshStatus();
    };
    const visible = () => document.visibilityState !== "hidden" && window.cageWindowVisible !== false;
    const check = () => {
      if (!visible()) return;
      const phase = current.current.phase;
      const interval = ["failed", "waiting"].includes(phase) ? retryInterval : checkInterval;
      const deadline = phase === "failed" && Number.isFinite(Date.parse(current.current.retryAt || ""))
        ? Date.parse(current.current.retryAt) : lastCheck.current + interval;
      if (checkable(current.current) && Date.now() >= deadline) void run("check");
    };
    const look = () => {
      if (!visible()) return;
      check();
      void refreshStatus();
    };
    window.addEventListener("cage-app-update", receive);
    window.addEventListener("focus", look);
    window.addEventListener("cage-window-visibility", look);
    document.addEventListener("visibilitychange", look);
    (async () => {
      try {
        const next = await native("appUpdate", { operation: "status" });
        if (!live) return;
        if (next?.phase === "failed") setStartupFailure(next.message || "上次应用更新未完成");
        if (next?.phase) { current.current = next; setState(next); }
        if (!next || (next.phase !== "available" && checkable(next))) void run("check");
      } catch (error) {
        if (live) setState({ phase: "failed", message: error.message, retryable: true });
      }
      if (live) void refreshStatus();
    })();
    const checkTimer = setInterval(check, 60 * 1000);
    return () => {
      live = false;
      clearInterval(checkTimer);
      window.removeEventListener("cage-app-update", receive);
      window.removeEventListener("focus", look);
      window.removeEventListener("cage-window-visibility", look);
      document.removeEventListener("visibilitychange", look);
    };
  }, [available, run, refreshStatus]);
  const watch = statusNeedsWatch(status);
  useEffect(() => {
    if (!available || !watch) return;
    const timer = setInterval(() => {
      if (document.visibilityState !== "hidden" && window.cageWindowVisible !== false) void refreshStatus();
    }, statusInterval);
    return () => clearInterval(timer);
  }, [available, watch, refreshStatus]);
  useEffect(() => {
    if (!startupFailure) return;
    const timer = setTimeout(() => setStartupFailure(""), 8000);
    return () => clearTimeout(timer);
  }, [startupFailure]);
  return { state, status, statusError, refreshStatus, run, available, startupFailure, coolingDown };
}

// The confirmation is its own lazy chunk: it loads on the click.
const AppUpdateConfirmation = lazy(() => import("./app-update-confirmation"));

export function AppUpdateButton({ updates }) {
  const { state, status, run, available, startupFailure } = updates;
  const view = appUpdateButton({ shell: state, status });
  const version = state.latestVersion || status?.pending?.version || "";
  const failed = view.tone === "failed";
  const announcedFailure = useRef("");
  const [failure, setFailure] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [acting, setActing] = useState(false);
  const button = useRef(null);
  useEffect(() => {
    if (state.phase === "failed" && view.visible && state.message && announcedFailure.current !== state.message) {
      announcedFailure.current = state.message;
      setFailure(state.message);
    }
    if (state.phase !== "failed") { announcedFailure.current = ""; setFailure(""); }
  }, [state.phase, view.visible, state.message]);
  useEffect(() => {
    if (!failure) return;
    const timer = setTimeout(() => setFailure(""), 8000);
    return () => clearTimeout(timer);
  }, [failure]);
  // A refusal that asks for a new confirmation reopens it once with the list.
  const reopened = useRef("");
  const refusal = state.blocked?.code === "confirm_required" ? JSON.stringify(state.blocked) : "";
  useEffect(() => {
    if (refusal && reopened.current !== refusal) setConfirming(true);
    reopened.current = refusal;
  }, [refusal]);
  if (!available || (!view.visible && !startupFailure)) return null;
  const act = async () => {
    setFailure("");
    if (view.action === "confirm") { setConfirming(true); return; }
    if (view.action === "releases") {
      void native("appReference", { kind: "releases" }).catch(error => setFailure(error.message));
      return;
    }
    const path = environmentActions[view.action];
    if (!path) return;
    setActing(true);
    try { await api(path, {}); await updates.refreshStatus?.(); }
    catch (error) { setFailure(error.message); }
    finally { setActing(false); }
  };
  const circumference = 2 * Math.PI * 15;
  const progress = view.percent !== null;
  return (
    <>
      {view.visible && <button
        ref={button}
        className={`app-update-button${view.busy ? " busy" : ""}${failed ? " failed" : ""}`}
        data-tone={view.tone}
        aria-label={view.label}
        title={view.hint}
        disabled={view.disabled || acting}
        onClick={act}
      >
        {view.busy ? (
          <>
            <svg className={`app-update-ring${progress ? "" : " indeterminate"}`}
              viewBox="0 0 36 36" role="progressbar" aria-label={progress ? "更新下载进度" : view.label}
              aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress ? view.percent : undefined}>
              <circle className="app-update-ring-track" cx="18" cy="18" r="15" />
              <circle className="app-update-ring-value" cx="18" cy="18" r="15"
                strokeDasharray={circumference} strokeDashoffset={circumference * (1 - (progress ? view.percent / 100 : 0.25))} />
            </svg>
            {progress && <span className="app-update-percent">{view.percent}%</span>}
          </>
        ) : failed ? <RefreshCw size={20} /> : <ArrowDownToLine size={20} />}
      </button>}
      {(failure || startupFailure) && <div className="app-update-error" role="alert">{failure || startupFailure}{view.visible && !view.disabled ? "。可点击升级按钮重试。" : ""}</div>}
      {confirming && version && <Suspense fallback={null}><AppUpdateConfirmation
        version={version}
        blocked={state.blocked}
        onCancel={() => setConfirming(false)}
        onContinue={confirmed => { setConfirming(false); void run("install", { confirmed }); }}
      /></Suspense>}
    </>
  );
}
