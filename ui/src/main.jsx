import { shortcut, primaryModifier } from "./platform.mjs";
import React, { useEffect, useRef, useState } from "react";
import {
  Clock3,
  LoaderCircle,
  MapPin,
  MoreHorizontal,
  Power,
  RefreshCw,
  RotateCcw,
  X,
} from "lucide-react";
import { api, normalizeURL, native, nativeAvailable } from "./api";
import { clientKeyboard, clientStorageKey, getClientContext, isCloudWorkbench, isRemoteClient, runtimeURL, runtimeSocketURL } from "./client-context.mjs";
import { shareClipboard } from "./shared-clipboard.mjs";
import { RemoteView, TerminalView, TerminalPending, Modal } from "./views";
import {
  isPendingTerminal, loginTerminalKind, markPendingTab, pendingTerminalTab, renamePendingLayout, settlePendingTab,
} from "./terminal-pending.mjs";
import { keepSame, reuseItems, terminalListDue } from "./poll-state.mjs";
import { usePasswordVault } from "./password-vault";
import { SettingsPanel } from "./settings";
import { validGuestChoice } from "./guest-system-picker";
import { AppearanceProvider } from "./appearance";
import { EnvironmentShield } from "./environment-shield";
import { TailscalePanel } from "./tailscale";
import { WorkspaceSidebar } from "./workspace-sidebar";
import { useCLISessions, sessionsChanged } from "./cli-sessions";
import { TerminalMenu } from "./terminal-menu";
import { InitializationGate } from "./initialization";
import { MacOSAppWindow } from "./macos-claude-window";
import { ClaudeDesktop } from "./claude-desktop";
import { ClaudeSciencePanel } from "./claude-science";
import { ClaudeAccountsPanel, useClaudeAccounts } from "./claude-accounts";
import { CodexAccountsPanel, useCodexAccounts } from "./codex-accounts";
import { codexTerminalDetail, codexTerminalLabel, codexTerminalSupported } from "./codex-accounts-state.mjs";
import { AccountProvider, AccountGate, useAccount } from "./account";
import { BrowserHeader, BrowserTabRail, TerminalHeader, ExpandSidebar, ExpandPane } from "./pane-headers";
import { useDownloadNotifications, useDesktopFiles } from "./browser-downloads";
import { BrowserUploads } from "./browser-uploads";
import { BrowserSettingsPage } from "./browser-settings";
import { BrowserDataHost, openBrowserData } from "./browser-data-launcher";
import { AudioControls, useAudioOutput } from "./audio";
import { EnvironmentClock } from "./environment-clock";
import { ProjectDialog } from "./project-dialog";
import {
  readLocal,
  orderedTabs,
  focusTab,
  splitTab,
  projectName,
  projectGroups,
  tabName,
} from "./workspace-state.mjs";
import { SafetyPanel } from "./safety";
import { EnvironmentRestartContext, SafetyChecklistContext, SafetyMessage } from "./safety-message";
import { networkState, isGuestLocalURL, needsEnvironmentRestart } from "./safety-state.mjs";
import { needsProxySetup } from "./onboarding-state.mjs";
import { NetworkLatency } from "./network-latency";
import { WorkbenchHome } from "./workbench-home";
import { BrandMark } from "./brand";
import { Drawing } from "./drawings";
import { useWindowVisibility } from "./window-visibility";
import { AppUpdateButton, useAppUpdates } from "./app-updates";
import "./appearance.css";
import { stopAndExit } from "./quit";
import { guestCapability } from "./guest-capabilities.mjs";

function App() {
  const { account } = useAccount();
  const [, refreshClientPreferences] = useState(0);
  useEffect(() => {
    const refresh = () => refreshClientPreferences((value) => value + 1);
    window.addEventListener("cage-client-preferences", refresh);
    return () => window.removeEventListener("cage-client-preferences", refresh);
  }, []);
  const claudeProfiles = useClaudeAccounts();
  const codexProfiles = useCodexAccounts();
  const [managementPage, setManagementPage] = useState("");
  const [tailnetLogin, setTailnetLogin] = useState(null);
  const [desktopOpen, setDesktopOpen] = useState(false);
  const [terminalChoice, setTerminalChoice] = useState(null);
  const [openingSession, setOpeningSession] = useState("");
  const [sessionOpenError, setSessionOpenError] = useState(null);
  const sessionOpenRequest = useRef(null);
  useEffect(() => () => {
    const request = sessionOpenRequest.current;
    sessionOpenRequest.current = null;
    request?.controller.abort();
  }, []);
  const terminalTrigger = useRef(null);
  const appUpdates = useAppUpdates();
  const [recoveringEditor, setRecoveringEditor] = useState(false);
  const windowVisible = useWindowVisibility();
  const windowVisibleRef = useRef(windowVisible);
  windowVisibleRef.current = windowVisible;
  const [serverStatus, setStatus] = useState(null),
    [settings, setSettings] = useState(null),
    [proxy, setProxy] = useState(null),
    [tabs, setTabs] = useState([]),
    [layout, setLayout] = useState(() => ({ active: localStorage.getItem(clientStorageKey("cage-active")) || "" })),
    [browser, setBrowser] = useState({ tabs: [] }),
    [browserSettingsRevision, setBrowserSettingsRevision] = useState(0),
    [settingsOpen, setSettingsOpen] = useState(false),
    [guestSystem, setGuestSystem] = useState(undefined),
    [startRequest, setStartRequest] = useState(null),
    [settingsSection, setSettingsSection] = useState("keyboard"),
    [home, setHome] = useState(true),
    [sidebarCollapsed, setSidebarCollapsed] = useState(
      readLocal(clientStorageKey("cage-sidebar-collapsed"), false),
    ),
    [browserTabLayout, setBrowserTabLayout] = useState(
      readLocal(clientStorageKey("cage-browser-tab-layout"), "horizontal"),
    ),
    [selectedProject, setSelectedProject] = useState(""),
    [savedTabs, setSavedTabs] = useState(readLocal(clientStorageKey("cage-tab-layout"), {})),
    [privateTabMetadata, setPrivateTabMetadata] = useState({}),
    [projectDialog, setProjectDialog] = useState(null),
    [rename, setRename] = useState(null),
    [toast, setToast] = useState(""),
    [projectRefreshing, setProjectRefreshing] = useState(false),
    [fatal, setFatal] = useState(""),
    [connectionError, setConnectionError] = useState(""),
    [browserError, setBrowserError] = useState(""),
    [dialog, setDialog] = useState(null),
    [transfer, setTransfer] = useState(null),
    [details, setDetails] = useState(false),
    [safetyOpen, setSafetyOpen] = useState(false),
    [quitting, setQuitting] = useState(null);
  // Keep feedback continuous until polling observes the accepted start job.
  const clientStatus = serverStatus && { ...serverStatus, keyboard: clientKeyboard(serverStatus.keyboard) };
  const status = clientStatus && startRequest
    ? { ...clientStatus, job: { running: true, kind: "start", message: "正在启动 Super Cage 环境…" } }
    : clientStatus;
  const system = status?.guest_system || account?.guest_system;
  const macGuest = system === "macos";
  const nativeGuestLabel = {macos: "macOS", "windows-11": "Windows", "windows-remote": "Windows 云端"}[system];
  const { active } = layout;
  const setActive = id => setLayout(previous => focusTab(previous, id));
  const workspaceBox = useRef(null);
  const [dropSide, setDropSide] = useState(null);
  const terminalControls = useRef(new Map());
  // One session object per tab object, so an unchanged tab passes the same props.
  const terminalSessions = useRef(new WeakMap());
  const terminalSession = (tab) => {
    let session = terminalSessions.current.get(tab);
    if (!session) terminalSessions.current.set(tab, session = { ...tab, id: tab.sessionId });
    return session;
  };
  const remote = useRef(),
    activeRef = useRef(active),
    lastBrowserTab = useRef(""),
    readyRef = useRef(false),
    browserIds = useRef(new Set()),
    initialized = useRef(false),
    browserStreamReady = useRef(false),
    browserSnapshot = useRef({ tabs: [] }),
    refreshStatus = useRef(null),
    launchAcknowledged = useRef(false),
    lastJob = useRef(""),
    quitAfterStop = useRef(null);
  activeRef.current = active;
  readyRef.current = Boolean(status?.ready);
  const storageRecovery = status?.storage_recovery?.required;
  const browserAvailable = guestCapability(status, "browser");
  const projectsAvailable = guestCapability(status, "projects");
  const editorAvailable = guestCapability(status, "editor");
  const clipboardAvailable = guestCapability(status, "clipboard");
  const desktopAvailable = guestCapability(status, "desktop");
  const current = tabs.find((t) => t.id === active),
    terminal = !desktopOpen && current?.kind === "terminal",
    busy = Boolean(status?.job?.running),
    showHome = storageRecovery || !desktopOpen && (home || !status?.vm_running || !current);
  const setupProgress = status?.job?.setup_progress && (busy || status.job.error);
  const cliSessions = useCLISessions(Boolean(status && !busy && !storageRecovery && guestCapability(status, "claude-cli")), windowVisible);
  useEffect(() => {
    if (storageRecovery) {
      setDesktopOpen(false);
      setHome(true);
      setStartRequest(null);
    }
  }, [storageRecovery]);
  const other = tabs.find(tab => tab.id === layout.other);
  const split = Boolean(other && current && !showHome && !desktopOpen);
  const panes = !split ? [current].filter(Boolean)
    : layout.side === "left" ? [current, other] : [other, current];
  const webPanes = panes.filter(tab => tab.kind !== "terminal");
  const passwordVault = usePasswordVault(Boolean(status?.ready && !macGuest && browserAvailable), browser.active);
  const dualBrowser = split && webPanes.length === 2;
  const leftId = dualBrowser ? panes[0].id : "";
  const rightId = dualBrowser ? panes[1].id : "";
  const nativeSplit = browser.split;
  const splitReady = dualBrowser
    ? nativeSplit?.left === leftId && nativeSplit?.right === rightId
    : !nativeSplit;
  const liveBrowser = webPanes.find(tab => tab.id === browser.active) || webPanes[0];
  const liveBrowserVisible = !managementPage && !desktopOpen && !showHome && status?.ready &&
    liveBrowser && splitReady && (!split || browser.active === liveBrowser.id);
  const viewBeforeBrowser = useRef(null);
  const browserDismissed = useRef(false);
  const browserVisible = !desktopOpen && !showHome && webPanes.length > 0;
  const browserWasVisible = useRef(false);
  useEffect(() => {
    // Preserve the return destination until the last tab's removal is handled,
    // including removals received through events or fallback polling.
    if (browserWasVisible.current && !tabs.some(tab => tab.kind !== "terminal")) closeBrowser();
    else if (!browserVisible) viewBeforeBrowser.current = {
      layout, home: showHome, desktopOpen, selectedProject,
    };
    browserWasVisible.current = browserVisible;
  }, [browserVisible, layout, showHome, desktopOpen, selectedProject, tabs]);
  const closeBrowser = () => {
    const previous = viewBeforeBrowser.current;
    browserDismissed.current = true;
    const available = id => tabs.some(tab => tab.id === id);
    const restored = previous?.layout;
    const id = [restored?.active, restored?.other].find(available);
    setLayout(id ? { ...restored, active: id,
      other: restored.other !== id && available(restored.other) ? restored.other : undefined,
    } : { active });
    setHome(Boolean(previous?.home || !previous?.desktopOpen && !id));
    setDesktopOpen(Boolean(previous?.desktopOpen));
    if (previous?.selectedProject) setSelectedProject(previous.selectedProject);
  };
  const splitQueue = useRef(Promise.resolve());
  const requestedSplit = useRef(false);
  const nativeSplitRef = useRef(null);
  nativeSplitRef.current = nativeSplit;
  useEffect(() => {
    if (!status?.ready || !browserAvailable) { requestedSplit.current = false; return; }
    if (!leftId && !requestedSplit.current && !nativeSplitRef.current) return;
    requestedSplit.current = Boolean(leftId);
    let cancelled = false;
    const apply = async () => {
      if (cancelled) return;
      const id = activeRef.current;
      try {
        const result = await api("/browser/split", { left: leftId, right: rightId,
          id: leftId ? (id === rightId ? rightId : leftId) : id });
        if (!cancelled) setBrowser(previous => ({ ...previous, split: result.split }));
      } catch {
        if (!cancelled) {
          setToast(tabs.some(tab => [leftId, rightId].includes(tab.id) && tab.incognito)
            ? "无痕标签可与终端分屏；两个网页分屏目前仅支持普通标签。"
            : "无法调整浏览器分屏。请确认环境组件已更新，两个网页属于同一浏览器配置，且客机 Chromium 为 155 或更高版本。");
          setLayout(previous => ({ active: previous.active }));
        }
      }
    };
    splitQueue.current = splitQueue.current.then(apply);
    return () => { cancelled = true; };
  }, [leftId, rightId, status?.ready, browserAvailable, Boolean(!leftId && nativeSplit)]);
  const layoutRef = useRef(layout);
  layoutRef.current = layout;
  useEffect(() => {
    if (!layout.other) return;
    if (!status?.vm_running || !other || !current) {
      const survivor = current || other;
      setLayout({ active: survivor?.id || "" });
      if (!current && survivor && survivor.kind !== "terminal" && status?.ready) {
        api("/browser/activate", { id: survivor.id }).catch(error => setToast(error.message));
      }
    }
  }, [layout, other, current, status?.vm_running, status?.ready]);
  const paneSide = id => !split ? "" : id === active ? layout.side :
    layout.side === "left" ? "right" : "left";
  const running = Boolean(status?.vm_running);
  const mixedBrowserId = split && !dualBrowser ? webPanes[0]?.id : null;
  useEffect(() => {
    if (status?.ready && mixedBrowserId && mixedBrowserId !== browser.active)
      api("/browser/activate", { id: mixedBrowserId }).catch(error => setToast(error.message));
  }, [mixedBrowserId, status?.ready, browser.active]);
  const finishedLogins = tabs.filter((tab) => tab.terminalKind === "claude-login" && tab.closed).map((tab) => tab.id).sort().join(",");
  useEffect(() => {
    if (finishedLogins) claudeProfiles.load();
  }, [finishedLogins, claudeProfiles.load]);
  const finishedCodexLogins = tabs.filter((tab) => tab.terminalKind === "codex-login" && tab.closed).map((tab) => tab.id).sort().join(",");
  useEffect(() => {
    if (finishedCodexLogins) codexProfiles.load();
  }, [finishedCodexLogins, codexProfiles.load]);
  const audio = useAudioOutput(running && guestCapability(status, "audio") && !storageRecovery && !status?.environment_update_required && !connectionError);
  useEffect(() => {
    if (audio.error) setToast(audio.error);
  }, [audio.error]);
  const groups = projectGroups(status);
  const projects = groups.map(group => group.workspace);
  const selectedWorkspace = selectedProject || status?.workspace;
  const workspace = groups.find(group => group.folders.includes(selectedWorkspace))?.workspace || selectedWorkspace;
  const contentWorkspace =
    terminal && !showHome ? current.workspace : workspace;
  const setupProxy = !nativeGuestLabel && needsProxySetup(settings, status, proxy);
  const network = connectionError
    ? { mode: "unknown", label: "外网状态待确认" }
    : networkState(status);
  const runtimeLabel = connectionError
    ? "连接中断"
    : busy
      ? network.mode === "checking" || status?.job?.kind === "safety"
        ? "检查中"
        : status?.job?.kind === "stop"
          ? "停止中"
          : running
            ? "应用中"
            : "启动中"
      : status?.ready
        ? network.mode === "open"
          ? "运行中"
          : "离线可用"
        : running
          ? status?.environment_update_required ? "环境待更新" : "浏览器待恢复"
          : "已停止";
  const notify = (text) => setToast(text);
  // A Linux environment keeps what it learned about project files for a while.
  const projectFilesCached = settings?.project_file_cache === true &&
    !/^(windows|macos)/.test(account?.guest_system || status?.guest_system || "");
  const refreshProjectFiles = async () => {
    setProjectRefreshing(true);
    try {
      await api("/projects/refresh", {});
      setToast("项目文件已刷新");
    } catch (error) {
      setToast(error.message);
    } finally {
      setProjectRefreshing(false);
    }
  };
  const clipboardConnection = getClientContext().connection_id;
  useEffect(() => {
    if (!status?.ready || !clipboardAvailable || !nativeAvailable()) return;
    const clipboard = shareClipboard({
      connect: () => new WebSocket(runtimeSocketURL("/clipboard")), native,
      onStatus: setToast,
      setActive: active => { window.cageSharedClipboardActive = active; },
    });
    window.cageSyncClipboard = () => clipboard.sync();
    return () => { clipboard.dispose(); delete window.cageSyncClipboard; };
  }, [status?.ready, clipboardAvailable, clipboardConnection]);
  const desktopFiles = useDesktopFiles(Boolean(status?.ready && desktopAvailable), notify, desktopOpen && !settingsOpen && !managementPage && !storageRecovery && windowVisible, windowVisible);
  const downloads = [...(browser.downloads || []), ...desktopFiles.downloads];
  useDownloadNotifications(downloads, notify, Boolean(browser.uploads?.length || desktopFiles.uploads.length));
  const refreshSettings = async () => {
    const results = await Promise.allSettled([api("/settings"), api("/proxy")]);
    if (results[0].status === "fulfilled") setSettings(results[0].value);
    else notify(results[0].reason.message);
    if (results[1].status === "fulfilled") setProxy(results[1].value);
    else setProxy(null);
  };
  const confirm = (options) =>
    new Promise((resolve) => setDialog({ ...options, resolve }));
  const openSettings = (section = "keyboard") => {
    setSafetyOpen(false);
    setSettingsSection(section);
    setSettingsOpen(true);
  };
  const select = async (id, focusContent = true) => {
    setTailnetLogin(null);
    setManagementPage("");
    const tab = tabs.find((t) => t.id === id);
    if (tab && tab.kind !== "terminal") browserDismissed.current = false;
    setDesktopOpen(false);
    setHome(false);
    if (tab?.kind === "browser-settings") setLayout({ active: id });
    else setActive(id);
    if (!tab?.incognito) localStorage.setItem(clientStorageKey("cage-active"), id);
    if (tab?.kind === "terminal") setSelectedProject(tab.workspace);
    if (tab && tab.kind !== "terminal") {
      try {
        await api("/browser/activate", { id });
        if (activeRef.current === id) {
          setBrowser(previous => ({ ...previous, active: id }));
          if (focusContent) remote.current?.focus();
        }
      } catch (e) {
        notify(e.message);
      }
    }
  };
  const destination = (id, x, y) => {
    const box = workspaceBox.current?.getBoundingClientRect();
    if (!box || busy || !running || desktopOpen || tabs.length < 2 ||
        document.elementFromPoint(x, y)?.closest(".chrome") ||
        !tabs.some(tab => tab.id === id) || tabs.find(tab => tab.id === id)?.kind === "browser-settings" ||
        current?.kind === "browser-settings" || x < box.left || x > box.right ||
        y < box.top || y > box.bottom) return null;
    return x < box.left + box.width * (dualBrowser && nativeSplit?.fraction || 0.5) ? "left" : "right";
  };
  const splitDrag = {
    clear: () => setDropSide(null),
    hover(id, x, y) {
      const side = destination(id, x, y);
      setDropSide(side);
      return side;
    },
    drop(id, x, y) {
      const side = destination(id, x, y);
      if (!side) return false;
      setLayout(previous => splitTab(previous, tabs, id, side));
      void select(id);
      return true;
    },
  };
  const action = async (path, body = {}) => {
    try {
      return await api(path, body);
    } catch (e) {
      notify(e.message);
      return null;
    }
  };
  const starting = useRef(false);
  const startEnvironment = async (options = {}) => {
    if (starting.current) return;
    starting.current = true;
    setStartRequest({ id: null });
    try {
      const result = await action("/start", options);
      setStartRequest(result ? { id: result.id } : null);
      if (result) refreshStatus.current?.();
    } finally {
      starting.current = false;
    }
  };
  const openBrowserPage = async (
    url = status?.guest_system === "windows-remote" && !guestCapability(status, "network")
      ? "about:blank" : settings?.browser_home_url || "https://www.bing.com/",
    incognito = Boolean(current?.incognito),
  ) => {
    if (!browserAvailable) {
      notify("此 Windows 云端环境的浏览器尚未接通。当前可使用隔离 PowerShell 终端。");
      return null;
    }
    setTailnetLogin(null);
    browserDismissed.current = false;
    const result = await action("/browser/new", { url, incognito });
    if (result) {
      setManagementPage("");
      setDesktopOpen(false);
      setHome(false);
      setTabs((list) =>
        list.some((t) => t.id === result.id)
          ? list
          : [
              ...list,
              {
                id: result.id,
                incognito,
                kind: "web",
                url,
                title: "新标签页",
              },
            ],
      );
      setActive(result.id);
    }
    return result;
  };
  const newPage = () => openBrowserPage();
  const showBrowserResult = result => {
    if (!result?.id) return;
    browserDismissed.current = false;
    setTailnetLogin(null);
    setManagementPage(""); setDesktopOpen(false); setHome(false);
    setTabs(previous => previous.some(tab => tab.id === result.id)
      ? previous.map(tab => tab.id === result.id ? { ...tab, ...result } : tab)
      : [...previous, result]);
    setLayout({ active: result.id });
  };
  const openBrowserSettings = async (section = "general") => {
    const result = await action("/browser/settings", { mode: section });
    if (result) showBrowserResult(result);
  };
  const openInternalPage = async (mode, id) => {
    const result = await action("/browser/internal-page", { mode, id });
    if (result) showBrowserResult(result);
  };
  const openSettingsPageResult = result => result.id ? showBrowserResult(result) : openBrowserPage(result.url, false);
  const openBrowser = () => {
    const tab =
      tabs.find(
        (t) => t.id === lastBrowserTab.current && t.kind !== "terminal",
      ) ||
      tabs.find((t) => t.id === browser.active && t.kind !== "terminal") ||
      tabs.find((t) => t.kind !== "terminal");
    return tab ? select(tab.id) : newPage();
  };
  const newTerminal = (path = workspace, anchor = terminalTrigger.current) => {
    if (system === "windows-remote" && !guestCapability(status, "claude-cli")) {
      createTerminal(status.workspace || "", "shell").catch((e) => notify(e.message));
      return;
    }
    if (!path && system !== "windows-remote") { setProjectDialog({}); return; }
    setTerminalChoice((previous) =>
      previous?.anchor === anchor ? null : { path, anchor },
    );
  };
  // The tab of a new terminal appears at once and the server's record
  // replaces it in place; a failure is shown there (terminal-pending.mjs).
  const pendingClosed = useRef(new Set());
  const createTerminal = async (path, kind, loginAccountId, retrying) => {
    setTerminalChoice(null);
    const codex = kind === "codex" || kind === "codex-login";
    const pending = !loginTerminalKind(kind) && (retrying || pendingTerminalTab(path, kind));
    if (retrying) setTabs(list => markPendingTab(list, retrying.id, ""));
    else if (pending) openTerminalTab(pending);
    let result;
    try {
      result = await api("/terminals", {
        workspace: path, kind,
        ...(loginAccountId && (codex ? { codex_account_id: loginAccountId } : { claude_account_id: loginAccountId })),
      });
    } catch (error) {
      if (!pending) throw error;
      pendingClosed.current.delete(pending.id);
      setTabs(list => markPendingTab(list, pending.id, error.message));
      return;
    }
    if (pending && pendingClosed.current.delete(pending.id)) {
      // Closed while it was being created.
      api("/terminals/" + result.id, undefined, "DELETE").catch(() => {});
      return;
    }
    showTerminal(result, pending?.id);
    sessionsChanged();
    if (kind === "claude" && result.claude_account_id &&
        !claudeProfiles.accounts?.some((profile) => profile.id === result.claude_account_id)) {
      claudeProfiles.load();
    }
    if (codex && result.codex_account_id &&
        !codexProfiles.accounts?.some((profile) => profile.id === result.codex_account_id)) {
      codexProfiles.load();
    }
  };
  const showTerminal = (result, placeholder) => {
    const tab = {
      ...result, id: "terminal:" + result.id, sessionId: result.id,
      terminalKind: result.kind, kind: "terminal",
    };
    if (!placeholder) return openTerminalTab(tab);
    // The user may have moved on meanwhile: only the placeholder changes.
    setTabs(list => settlePendingTab(list, placeholder, tab));
    setLayout(previous => renamePendingLayout(previous, placeholder, tab.id));
  };
  const openTerminalTab = tab => {
    setTailnetLogin(null);
    setManagementPage("");
    setTabs((list) => [...list.filter((t) => t.id !== tab.id && (!tab.cli_session_id || t.cli_session_id !== tab.cli_session_id)), tab]);
    setSelectedProject(tab.workspace);
    setDesktopOpen(false);
    setHome(false);
    setActive(tab.id);
  };
  const openSessionTerminal = async session => {
    if (session.running) return;
    if (sessionOpenRequest.current?.id === session.id) return;
    sessionOpenRequest.current?.controller.abort();
    const controller = new AbortController();
    const request = { id: session.id, controller };
    sessionOpenRequest.current = request;
    setOpeningSession(session.id);
    setSessionOpenError(null);
    const timer = setTimeout(() => controller.abort(new Error("打开会话超时，历史已保留。请重新打开；若仍失败，请在设置中导出诊断包。")), 60000);
    try {
      const view = await api("/cli/sessions/" + session.id + "/open", {}, undefined, {}, { signal: controller.signal });
      const terminals = await api("/terminals", undefined, undefined, {}, { signal: controller.signal });
      if (sessionOpenRequest.current !== request) return;
      const terminal = terminals.find(item => item.id === view.desktop_terminal_id && item.cli_session_id === session.id);
      if (!terminal) throw new Error("终端状态已变化，请重新打开会话");
      showTerminal(terminal);
      sessionsChanged();
    } catch (error) {
      if (sessionOpenRequest.current === request && error.code !== "connection_changed") {
        const message = (controller.signal.aborted ? controller.signal.reason : error).message;
        setSessionOpenError({ id: session.id, message });
      }
    } finally {
      clearTimeout(timer);
      if (sessionOpenRequest.current === request) {
        sessionOpenRequest.current = null;
        setOpeningSession("");
      }
    }
  };
  const archiveSession = async (session, anchor) => {
    await confirm({title:"归档这段会话？", body:"归档会关闭关联终端。历史将保留，可从设置中的已归档会话恢复。", confirm:"归档会话", pendingLabel:"正在归档…", anchor,
      submit: async () => {
        await api("/cli/sessions/" + session.id + "/archive", {});
        setTabs(list => list.filter(tab => tab.cli_session_id !== session.id));
        sessionsChanged();
        refreshStatus.current?.();
      },
    });
  };
  const pinSession = async session => {
    try {
      await api("/cli/sessions/" + session.id + "/metadata", {pinned:!session.pinned});
      sessionsChanged();
    } catch (error) { notify(error.message); }
  };
  const newTerminalHere = (event) =>
    newTerminal(contentWorkspace, event?.currentTarget?.tagName === "BUTTON"
      ? event.currentTarget : terminalTrigger.current);
  const terminalMenuAt = (location) => terminalChoice?.anchor?.dataset.terminalLocation === location && (
    <TerminalMenu choice={terminalChoice} busy={busy} codex={codexTerminalSupported(system)}
      onClose={() => setTerminalChoice(null)}
      onSelect={(kind) => createTerminal(terminalChoice.path, kind).catch((e) => notify(e.message))} />
  );
  const openEditor = async (path = workspace) => {
    if (!browserAvailable || !editorAvailable) {
      notify("此 Windows 云端环境的文件编辑器尚未接通。当前可在隔离 PowerShell 中操作文件。");
      return;
    }
    if (!path) { setProjectDialog({}); return; }
    browserDismissed.current = false;
    setTailnetLogin(null);
    const result = await action("/browser/editor", { workspace: path });
    if (result) {
      setManagementPage("");
      setDesktopOpen(false);
      setHome(false);
      setTabs((list) =>
        list.some((t) => t.id === result.id)
          ? list
          : [
              ...list,
              {
                id: result.id,
                kind: "editor",
                workspace: path,
                url: `http://127.0.0.1:18080/?folder=${encodeURIComponent(path)}`,
                title: "文件编辑器",
              },
            ],
      );
      setActive(result.id);
    }
  };
  const closeTab = async (tab, anchor) => {
    if (macGuest && tab.native_window) { closeBrowser(); return; }
    if (tab.cli_session_id) return archiveSession({id:tab.cli_session_id, title:tab.title}, anchor);
    // Nothing runs yet; a terminal that still arrives is closed then.
    if (tab.pending) { if (!tab.error) pendingClosed.current.add(tab.id); }
    else if (tab.kind === "terminal") {
      if (
        !tab.closed &&
        !(await confirm({
          title: "关闭这个终端会话？",
          body: "该终端中的交互命令可能被中断。网页和其他终端不受影响。",
          confirm: "关闭终端",
        }))
      )
        return;
      try {
        await api("/terminals/" + tab.sessionId, undefined, "DELETE");
      } catch (e) {
        notify(e.message);
        return;
      }
    } else {
      if (
        tab.kind === "editor" &&
        !(await confirm({
          title: "关闭文件编辑器？",
          body: "请先保存未完成的修改。文件仍保留在共享项目中。",
          confirm: "关闭编辑器",
        }))
      )
        return;
      if (!(await action("/browser/close", { id: tab.id }))) return;
      if (tab.id === tailnetLogin?.tabId) setTailnetLogin(null);
    }
    setTabs((list) => list.filter((t) => t.id !== tab.id));
    if (tab.incognito) setPrivateTabMetadata(previous => Object.fromEntries(Object.entries(previous).map(([key, value]) => [key,
      Object.fromEntries(Object.entries(value).filter(([id]) => id !== tab.id).map(([id, item]) =>
        [id, id === "order" ? item.filter(id => id !== tab.id) : item])),
    ])));
    // The browser visibility effect restores the previous view for an empty
    // browser. Do not overwrite it when a close response follows its event.
    if (tab.kind !== "terminal" && !tabs.some(item => item.id !== tab.id && item.kind !== "terminal")) return;
    if (split && (tab.id === active || tab.id === other.id)) {
      const remaining = tab.id === active ? other.id : active;
      setLayout({ active: remaining });
      await select(remaining);
    } else if (active === tab.id) {
      const group = displayTabs.filter((item) =>
        tab.kind === "terminal"
          ? item.kind === "terminal" && item.workspace === tab.workspace
          : item.kind !== "terminal",
      );
      const at = group.findIndex((item) => item.id === tab.id);
      const next = group[at + 1] || group[at - 1];
      if (next) await select(next.id);
      else setActive("");
    }
  };
  const navigate = async (id, address) => {
    try {
      const internal = /^cage:\/\/settings(?:#(general|appearance|privacy|extensions|about))?\/?$/.exec(address.trim());
      if (internal) { await openBrowserSettings(internal[1] || "general"); return; }
      const url = normalizeURL(address);
      if (network.mode !== "open" && !isGuestLocalURL(url)) {
        setSafetyOpen(true);
        notify("请先完成关键检查，再访问外部网站。");
        return;
      }
      await action("/browser/navigate", { id, url });
      remote.current?.focus();
    } catch (error) {
      notify(error.message);
    }
  };
  const stop = async () => {
    if (
      await confirm({
        title: setupProgress ? "停止安装环境？" : "停止当前环境？",
        body: setupProgress ? "当前安装任务会停止。下次启动环境时可以继续安装。" : "请先保存文件。当前终端会话会结束，浏览器登录和项目文件会保留。",
        confirm: setupProgress ? "停止安装" : "停止环境",
        danger: true,
      })
    )
      await action("/stop");
  };
  // Stop, wait for the stop job, then start again: the one-click way out of
  // "needs an environment restart" errors.
  const restarting = useRef(false);
  const restartEnvironment = async () => {
    if (restarting.current) return;
    if (
      !(await confirm({
        title: "重启环境？",
        body: "请先保存文件。环境会停止后重新启动，当前终端会话会结束；浏览器登录和项目文件会保留，重新检测通过前外网保持关闭。",
        confirm: "重启环境",
      }))
    )
      return;
    restarting.current = true;
    try {
      await stopAndExit({
        stop: () => api("/stop", {}),
        readStatus: () => api("/status"),
        exit: () => startEnvironment(),
        isCancelled: () => false,
        onStatus: setStatus,
      });
    } catch (error) {
      notify(error.message);
    } finally {
      restarting.current = false;
    }
  };
  const cancelAutomaticExit = () => {
    quitAfterStop.current = null;
    setQuitting(null);
  };
  const stopAndQuit = async () => {
    const request = { id: null };
    quitAfterStop.current = request;
    setQuitting({ error: "" });
    try {
      await stopAndExit({
        stop: () => api("/stop", {}),
        readStatus: () => api("/status"),
        exit: () => native("quitNow"),
        isCancelled: () => quitAfterStop.current !== request,
        onStatus: setStatus,
      });
    } catch (error) {
      if (quitAfterStop.current === request) {
        quitAfterStop.current = null;
        setQuitting({ error: error.message });
      }
    }
  };
  useEffect(() => {
    let live = true,
      timer,
      inflight = null,
      queued = null;
    // The last terminal list, read again only when the status reports that
    // it changed (see terminalListDue).
    let terminalItems = null, terminalRevision, terminalPolls = 0;
    const run = async (force) => {
      clearTimeout(timer);
      let jobRunning = false;
      try {
        const next = await api("/status");
        if (!live) return;
        jobRunning = next.job.running;
        setStatus(previous => keepSame(previous, next));
        setStartRequest(pending => pending?.id && pending.id === next.job.id ? null : pending);
        setConnectionError("");
        if (
          !next.job.running &&
          next.job.id &&
          lastJob.current !== next.job.id
        ) {
          lastJob.current = next.job.id;
          await refreshSettings();
          if (next.job.kind === "guest-install" && !next.job.error) {
            const install = await api("/environment/guest-systems");
            if (live && install.activate) setGuestSystem(previous => ({ ...previous, activate:true }));
          }
          if (next.job.error) notify(next.job.error);
        }
        if (next.ready) {
          const supportsBrowser = guestCapability(next, "browser");
          const fetchBrowser = supportsBrowser && !next.environment_update_required && (!browserStreamReady.current || !initialized.current);
          if (!supportsBrowser) setBrowserError("");
          terminalPolls++;
          const readTerminals = terminalListDue({ force, cached: Boolean(terminalItems), revision: next.terminals_revision, lastRevision: terminalRevision, polls: terminalPolls });
          const [browserResult, fetched] = await Promise.all([
            fetchBrowser ? api("/browser/tabs").then(result => {
              if (live) setBrowserError("");
              return result;
            }).catch(error => {
              if (live) setBrowserError(next.guest_system === "macos" ? "请在环境设置中同步 macOS 组件，并确认已登录客机图形账户。" : error.message);
              return null;
            }) : null,
            readTerminals ? api("/terminals") : null,
          ]);
          if (!live) return;
          if (fetched) {
            terminalItems = fetched;
            terminalRevision = next.terminals_revision;
            terminalPolls = 0;
          }
          const t = terminalItems;
          const b = supportsBrowser ? browserResult || browserSnapshot.current : { tabs: [] };
          const legacyMetadata = readLocal(clientStorageKey("cage-tab-layout"), {})[next.environment_identity_directory || next.environment_directory || next.workspace] || {};
          for (const item of t) {
            const saved = legacyMetadata["terminal:" + item.id];
            if (!item.cli_session_id || item.metadata_set || !saved || (!saved.title && !saved.pinned)) continue;
            try {
              const updated = await api("/terminals/" + item.id + "/metadata", {
                ...(saved.title ? {title:saved.title} : {}), pinned:Boolean(saved.pinned),
              });
              Object.assign(item, {title:updated.title, custom_title:updated.custom_title, pinned:updated.pinned, metadata_set:true});
            } catch { /* Keep the existing local label until the host accepts it. */ }
          }
          if (!live) return;
          browserSnapshot.current = b;
          setBrowser(previous => keepSame(previous, b));
          const all = [
            ...b.tabs,
            ...t.map((item) => ({
              ...item,
              id: "terminal:" + item.id,
              sessionId: item.id,
              terminalKind: item.kind,
              kind: "terminal",
            })),
          ];
          setTabs((previous) => {
            const map = new Map(all.map((item) => [item.id, item]));
            return reuseItems(previous, [
              ...previous
                .filter((item) => map.has(item.id) || item.pending)
                .map((item) => map.get(item.id) || item),
              ...all.filter((item) => !previous.some((p) => p.id === item.id)),
            ]);
          });
          if (!browserDismissed.current && initialized.current && b.active && b.active !== activeRef.current && !browserIds.current.has(b.active)) {
            setDesktopOpen(false);
            setHome(false);
          }
          // A terminal still being created is not on the server yet.
          const listed = all.some((item) => item.id === activeRef.current) || isPendingTerminal(activeRef.current);
          if (
            !activeRef.current ||
            !listed ||
            (!browserDismissed.current && initialized.current &&
              b.active &&
              !browserIds.current.has(b.active))
          ) {
            if (browserDismissed.current && !listed) setHome(true);
            // Let split reconciliation promote the remaining pane on close.
            if (!layoutRef.current.other || listed)
              setActive(b.active || all[0]?.id || "");
          }
          browserIds.current = new Set(b.tabs.map((item) => item.id));
          initialized.current = true;
        }
      } catch (e) {
        if (live) setConnectionError(e.message);
      } finally {
        if (live)
          timer = setTimeout(
            poll,
            jobRunning ? 1200 : windowVisibleRef.current ? 5000 : 15000,
          );
      }
    };
    // Timer polls join one in flight. A requested refresh (after an action
    // or when the window is shown) runs a fresh poll once it has finished.
    const poll = (force = false) => {
      if (!live) return Promise.resolve();
      if (!inflight) {
        inflight = run(force).finally(() => { inflight = null; });
        return inflight;
      }
      if (!force) return inflight;
      queued ||= inflight.then(() => { queued = null; return poll(true); });
      return queued;
    };
    (async () => {
      try {
        const token = location.hash.slice(1);
        if (token) {
          await api("/session", { token });
          history.replaceState(null, "", location.pathname);
        }
        const config = await api("/settings");
        window.dispatchEvent(new Event("cage-session-ready"));
        if (!live) return;
        setSettings(config);
        try {
          const proxyState = await api("/proxy");
          if (!live) return;
          setProxy(proxyState);
        } catch {
          // Network settings remain available when the core cannot be read.
        }
        refreshStatus.current = () => poll(true);
        await poll(true);
      } catch (e) {
        if (live) setFatal(e.message);
      }
    })();
    return () => {
      live = false;
      refreshStatus.current = null;
      clearTimeout(timer);
    };
  }, []);
  useEffect(() => {
    if (windowVisible) refreshStatus.current?.();
  }, [windowVisible]);
  useEffect(() => {
    if (!settings || !status || fatal || connectionError || launchAcknowledged.current || !nativeAvailable()) return;
    // Effects run after the workbench render commits. HTML navigation alone
    // cannot confirm a successful upgrade when session/API startup failed.
    let live = true;
    native("workbenchReady").then((accepted) => {
      if (live && accepted === true) launchAcknowledged.current = true;
    }).catch(() => {});
    return () => { live = false; };
  }, [settings, status, fatal, connectionError]);
  useEffect(() => {
    if (!status?.ready || !browserAvailable) return;
    const stream = new EventSource(runtimeURL("/browser/events"));
    stream.addEventListener("clipboard", (event) => {
      remote.current?.receiveCopy?.(JSON.parse(event.data));
    });
    stream.onopen = () => { browserStreamReady.current = true; };
    stream.onerror = () => { browserStreamReady.current = false; };
    stream.onmessage = (event) => {
      const b = JSON.parse(event.data);
      setBrowserError("");
      if (b.desktopFocus === true) {
        setDesktopOpen(true);
        setHome(false);
        setSettingsOpen(false);
      }
      const previousPair = browserSnapshot.current.split;
      const local = layoutRef.current;
      if (previousPair && !b.split && [local.active, local.other].includes(previousPair.left) &&
          [local.active, local.other].includes(previousPair.right)) {
        const survivor = b.tabs.some(tab => tab.id === local.active) ? local.active : local.other;
        setLayout({ active: survivor });
      }
      browserSnapshot.current = b;
      setBrowser((previous) => ({ ...previous, ...b }));
      setTabs((previous) => {
        const map = new Map(b.tabs.map((t) => [t.id, t]));
        return [
          ...previous
            .filter((t) => t.kind === "terminal" || map.has(t.id))
            .map((t) => map.get(t.id) || t),
          ...b.tabs.filter((t) => !previous.some((p) => p.id === t.id)),
        ];
      });
      const focusLogin = b.focus && b.tabs.some(tab => tab.native_window && tab.login);
      if (focusLogin) browserDismissed.current = false;
      if (!browserDismissed.current && b.active && b.focus) {
        setDesktopOpen(false);
        setHome(false);
      }
      if (
        b.active &&
        ((!browserDismissed.current && b.focus) ||
          !activeRef.current ||
          (!layoutRef.current.other && !activeRef.current.startsWith("terminal:")))
      )
        setActive(b.active);
      browserIds.current = new Set(b.tabs.map((t) => t.id));
    };
    return () => {
      browserStreamReady.current = false;
      stream.close();
    };
  }, [status?.ready, browserAvailable]);
  useEffect(() => {
    if (current && current.kind !== "terminal")
      lastBrowserTab.current = current.id;
    document.title =
      (current?.title && current.title !== "新标签页"
        ? current.title + " · "
        : "") + "Super Cage";
    if (active && !current?.incognito) localStorage.setItem(clientStorageKey("cage-active"), active);
  }, [current?.url, current?.title, current?.incognito, active]);
  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(""), 4500);
    return () => clearTimeout(timer);
  }, [toast]);
  useEffect(() => {
    if (!tailnetLogin) return;
    if (!status?.ready || managementPage || showHome || desktopOpen) {
      setTailnetLogin(null);
      return;
    }
    if (settingsOpen) return;
    const controller = new AbortController();
    let timer;
    const check = async () => {
      try {
        const state = await api("/tailscale", undefined, undefined, {}, { signal: controller.signal });
        if (controller.signal.aborted) return;
        if (!state.enabled) { setTailnetLogin(null); return; }
        if (["Running", "NeedsMachineAuth"].includes(state.backend) || state.return_id && state.return_id !== tailnetLogin.returnID) {
          setTailnetLogin(null);
          setManagementPage("tailscale");
          return;
        }
      } catch { /* A temporary connection failure can recover during authorization. */ }
      if (!controller.signal.aborted) timer = setTimeout(check, 1500);
    };
    void check();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [tailnetLogin, managementPage, settingsOpen, desktopOpen, showHome, status?.ready]);
  useEffect(() => {
    if (!managementPage) return;
    document.getElementById(managementPage + "-title")?.focus();
    const close = () => {
      if (!document.querySelector('[role="dialog"], .settings-page')) setManagementPage("");
    };
    document.addEventListener("cage-close-modal", close);
    return () => document.removeEventListener("cage-close-modal", close);
  }, [managementPage]);
  useEffect(() => {
    const key = (event) => {
      // Workbench shortcuts can stop the event before remote input sees it.
      window.dispatchEvent(new Event("cage-input-interrupted"));
      if (document.querySelector("[data-environment-dialog]") || !primaryModifier(event)) return;
      const k = event.key.toLowerCase();
      if (document.querySelector("[data-browser-dialog], .mac-login-fullscreen")) {
        if (k === "w") {
          event.preventDefault();
          event.stopImmediatePropagation();
          document.dispatchEvent(new Event("cage-close-modal"));
        }
        return;
      }
      if (dialog) {
        if (k === "w") {
          event.preventDefault();
          event.stopImmediatePropagation();
          void finishDialog(false);
        }
        return;
      }
      if (k === "," && !projectDialog && !rename) {
        event.preventDefault();
        event.stopImmediatePropagation();
        openSettings();
        return;
      }
      if (
        settingsOpen ||
        managementPage ||
        transfer ||
        details ||
        safetyOpen ||
        projectDialog ||
        terminalChoice ||
        rename
      ) {
        if (k === "w") {
          event.preventDefault();
          event.stopImmediatePropagation();
          document.dispatchEvent(new Event("cage-close-modal"));
        }
        return;
      }
      // The guest owns Chrome's address bar, tabs and browser shortcuts.
      if (macGuest && !showHome && !desktopOpen && !terminal && event.target.closest?.(".mac-claude-frame")) return;
      let handle;
      if (k === "o") {
        event.preventDefault();
        event.stopImmediatePropagation();
        if (event.shiftKey) {
          document.dispatchEvent(new Event("cage-show-favorites"));
          return;
        }
        if (projectsAvailable && !busy && running) setProjectDialog({ path: "" });
        return;
      }
      if (k === "t" && !event.shiftKey) handle = newPage;
      if (k === "n" && event.shiftKey) handle = () => openBrowserPage(undefined, true);
      if (k === "d") handle = () => document.dispatchEvent(new Event("cage-add-favorite"));
      if (event.shiftKey && (k === "backspace" || k === "delete") && browserAvailable && !terminal && !desktopOpen) handle = () => openBrowserData({ view: "clear", tab: liveBrowser });
      if (k === "enter" && event.shiftKey) handle = newTerminalHere;
      if (k === "l")
        handle = () => {
          const input = [...document.querySelectorAll("[data-browser-tab]")].find(node => node.dataset.browserTab === active)?.querySelector(".address-bar input");
          input?.focus();
          input?.select();
        };
      if (k === "w" && current && !desktopOpen) handle = () => closeTab(current);
      if (/^[1-9]$/.test(k)) {
        const tab = k === "9" ? switchTabs.at(-1) : switchTabs[Number(k) - 1];
        if (tab) handle = () => select(tab.id);
      }
      if (event.shiftKey && ["[", "]", "{", "}"].includes(k)) {
        const direction = ["[", "{"].includes(k) ? -1 : 1,
          at = switchTabs.findIndex((t) => t.id === active),
          tab =
            switchTabs[
              (at + direction + switchTabs.length) % switchTabs.length
            ];
        if (tab) handle = () => select(tab.id);
      }
      if (handle) {
        event.preventDefault();
        event.stopImmediatePropagation();
        if (
          !busy &&
          (status?.ready ||
            (running && k === "enter") ||
            (k === "w" && terminal))
        )
          void handle();
      }
    };
    window.addEventListener("keydown", key, true);
    window.cageEditAction = (command) => {
      if (document.querySelector(".mac-login-fullscreen")) return false;
      if (
        settingsOpen ||
        managementPage ||
        transfer ||
        dialog ||
        details ||
        safetyOpen ||
        projectDialog ||
        terminalChoice ||
        rename ||
        showHome
      )
        return false;
      if (terminal)
        return terminalControls.current.get(current.id)?.edit(command) || false;
      return remote.current?.edit(command) || false;
    };
    window.cageWorkbenchAction = async (command) => {
      window.dispatchEvent(new Event("cage-input-interrupted"));
      if (document.querySelector(".mac-login-fullscreen")) {
        if (command === "closeTab" || command === "quit") document.dispatchEvent(new Event("cage-close-modal"));
        if (command !== "quit") return;
      }
      if (document.querySelector("[data-environment-dialog]")) {
        if (command === "closeTab") document.dispatchEvent(new Event("cage-close-modal"));
        return;
      }
      if (command === "settings") {
        if (!projectDialog && !rename && !dialog) openSettings();
        return;
      }
      if (storageRecovery && ["openProject", "newTab", "newTerminal"].includes(command)) {
        openSettings("project");
        return;
      }
      if (command === "openProject") {
        if (
          projectsAvailable &&
          !busy &&
          running &&
          !settingsOpen &&
          !managementPage &&
          !dialog &&
          !projectDialog &&
          !rename
        )
          setProjectDialog({ path: "" });
        return;
      }
      if (
        (settingsOpen || managementPage || dialog || projectDialog || terminalChoice || rename) &&
        ["newTab", "newTerminal"].includes(command)
      )
        return;
      if (command === "newTab" && status?.ready) await newPage();
      if (command === "newTerminal" && status?.vm_running)
        await newTerminalHere();
      if (command === "closeTab") {
        if (document.querySelector("[data-browser-dialog], .mac-login-fullscreen")) {
          document.dispatchEvent(new Event("cage-close-modal"));
          return;
        }
        if (dialog) {
          void finishDialog(false);
        } else if (
          settingsOpen ||
          managementPage ||
          transfer ||
          details ||
          safetyOpen ||
          projectDialog ||
          terminalChoice ||
          rename
        )
          document.dispatchEvent(new Event("cage-close-modal"));
        else if (desktopOpen) setDesktopOpen(false);
        else if (macGuest && current && !terminal && !showHome) remote.current?.closeNativeTab?.();
        else if (current) await closeTab(current);
      }
      if (command === "quit") {
        if (quitAfterStop.current) return;
        if (isRemoteClient()) {
          await native("quitNow");
          return;
        }
        if (status && !connectionError && !status.vm_running && !busy) {
          await native("quitNow");
          return;
        }
        if (connectionError || fatal) {
          setDialog({
            title: "退出 Super Cage",
            body: "工作台连接已中断，暂时无法确认环境状态。仍在运行的后台任务会继续。",
            confirm: "仅退出桌面应用",
            resolve: (confirmed) => confirmed && native("quitNow"),
          });
          return;
        }
        setDialog({
          title: "退出 Super Cage",
          body: "退出前请保存文件和设置。可以保留环境，让后台任务继续运行；下次打开会重新连接。",
          confirm: "停止环境并退出",
          secondary: "保留运行并退出",
          resolve: async (result) => {
            if (result === "secondary") {
              await native("quitNow");
            } else if (result) {
              await stopAndQuit();
            }
          },
        });
      }
    };
    return () => {
      window.removeEventListener("keydown", key, true);
      delete window.cageWorkbenchAction;
      delete window.cageEditAction;
    };
  }, [
    status,
    settingsOpen,
    managementPage,
    safetyOpen,
    details,
    transfer,
    dialog,
    current,
    tabs,
    busy,
    active,
    connectionError,
    fatal,
    projectDialog,
    rename,
    showHome,
    savedTabs,
    selectedProject,
    macGuest,
    desktopOpen,
    terminalChoice,
  ]);
  const project = projectName(contentWorkspace, groups);
  const tabLayoutKey = status?.environment_identity_directory || status?.environment_directory || status?.workspace;
  const metadata = { ...(savedTabs[tabLayoutKey] || {}), ...(privateTabMetadata[tabLayoutKey] || {}) };
  for (const tab of tabs) {
    if (tab.cli_session_id && tab.metadata_set) metadata[tab.id] = {
      ...metadata[tab.id], title:tab.custom_title || tab.title, pinned:Boolean(tab.pinned),
    };
  }
  const claudeLabels = new Map(claudeProfiles.accounts?.map((account) => [account.id, account.label || account.email]));
  const claudeNumbers = new Map(claudeProfiles.accounts?.map((account, index) => [account.id, index + 1]));
  const numberedSessions = cliSessions.sessions.map((session) => ({ ...session,
    claudeAccountNumber: claudeNumbers.get(session.account_id) }));
  const terminalAccountName = (tab) => claudeLabels.get(tab.claude_account_id) ||
    (tab.terminalKind === "claude-login" ? "新账号登录" : tab.claude_account_id ? "Claude 账号" : "");
  const displayTabs = orderedTabs(tabs, metadata).map((tab) =>
    tab.claude_account_id || tab.terminalKind === "claude-login" ? { ...tab,
      claudeLabel: terminalAccountName(tab), claudeAccountNumber: claudeNumbers.get(tab.claude_account_id) }
      : codexTerminalLabel(tab, codexProfiles.accounts) ? { ...tab, claudeLabel: "Codex · " + codexTerminalLabel(tab, codexProfiles.accounts) } : tab);
  const browserTabs = displayTabs.filter((tab) => tab.kind !== "terminal");
  const switchTabs =
    terminal && !showHome
      ? displayTabs.filter(
          (tab) =>
            tab.kind === "terminal" && tab.workspace === current.workspace,
        )
      : dualBrowser ? browserTabs.filter(tab => current.id === rightId ? tab.id === rightId : tab.id !== rightId) : browserTabs;
  const renameTab = (tab) => !tab.pending &&
    setRename({ id: tab.id, title: tabName({ ...tab, incognito: false }, metadata, project) });
  const updateSharedMetadata = async (tab, changes) => {
    const updated = await api("/terminals/" + tab.sessionId + "/metadata", changes);
    setTabs(list => list.map(item => item.id === tab.id ? {
      ...item, title:updated.title, custom_title:updated.custom_title, pinned:updated.pinned, metadata_set:true,
    } : item));
  };
  const pinTab = (id) => {
    if (isPendingTerminal(id)) return;
    const tab = tabs.find(tab => tab.id === id);
    if (tab?.cli_session_id) {
      void updateSharedMetadata(tab, {pinned:!metadata[id]?.pinned}).catch(error => notify(error.message));
      return;
    }
    changeTabMetadata({
      ...metadata,
      [id]: { ...metadata[id], pinned: !metadata[id]?.pinned },
    });
  };
  const persist = (key, value, setter) => {
    try {
      localStorage.setItem(clientStorageKey(key), JSON.stringify(value));
      setter(value);
    } catch {
      notify("未能保存界面偏好，请检查本地存储空间。");
    }
  };
  const changeTabMetadata = (next) => {
    const currentPrivateIDs = new Set(tabs.filter(tab => tab.incognito).map(tab => tab.id));
    const privateIDs = new Set([...currentPrivateIDs, ...Object.keys(privateTabMetadata[tabLayoutKey] || {}).filter(id => id !== "order" && id !== "sessionOrder")]);
    const persistent = Object.fromEntries(Object.entries(next).filter(([id]) => !privateIDs.has(id)));
    if (next.order) persistent.order = next.order.filter(id => !privateIDs.has(id) && tabs.some(tab => tab.id === id));
    setPrivateTabMetadata(previous => ({ ...previous, [tabLayoutKey]: {
      ...Object.fromEntries(Object.entries(next).filter(([id]) => currentPrivateIDs.has(id))),
      ...(next.order ? { order: next.order } : {}),
    } }));
    persist(
      "cage-tab-layout",
      { ...savedTabs, [tabLayoutKey]: persistent },
      setSavedTabs,
    );
  };
  const reorder = (from, to, scopeOrder) => {
    if (scopeOrder) {
      const ids = new Set(scopeOrder);
      let index = 0;
      const order = displayTabs.map((tab) => ids.has(tab.id) ? scopeOrder[index++] : tab.id);
      changeTabMetadata({ ...metadata, order });
      return;
    }
    if (from === to) return;
    const order = displayTabs.map((tab) => tab.id).filter((id) => id !== from);
    order.splice(order.indexOf(to), 0, from);
    changeTabMetadata({ ...metadata, order });
  };
  const openedProject = async (path) => {
    await refreshSettings();
    await refreshStatus.current?.();
    setSelectedProject(path);
    setProjectDialog(null);
    setHome(true);
  };
  useEffect(() => {
    if (status?.workspace) setSelectedProject(status.workspace);
  }, [status?.workspace]);
  const finishDialog = async (value) => {
    if (!dialog || dialog.pending) return;
    if (value === true && dialog.submit) {
      setDialog({ ...dialog, pending: true, error: "" });
      try { await dialog.submit(); }
      catch (error) {
        setDialog({ ...dialog, pending: false, error: error.message });
        return;
      }
    }
    const resolve = dialog.resolve;
    setDialog(null);
    resolve(value);
  };
  const sidebarButton = side => sidebarCollapsed && side !== "right" &&
    <ExpandSidebar onClick={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)} />;
  const expandButton = tab => split && tab && <ExpandPane onClick={() => {
    setLayout({ active: tab.id });
    void select(tab.id);
  }} />;
  const focusHeader = tab => {
    if (tab && tab.id !== activeRef.current) void select(tab.id, false);
  };
  const terminalHeader = tab => {
    const path = tab?.workspace || workspace;
    const account = tab?.claude_account_id && terminalAccountName(tab);
    const detail = tab?.terminalKind === "claude-login" ? `登录 · ${account || "新账号"}`
      : account ? `Claude · ${account}${tab.claude_account_source === "default" ? "（启动时账号）" : ""}` : codexTerminalDetail(tab, codexProfiles.accounts);
    const location = split && tab ? `heading-${tab.id}` : "heading";
    return <TerminalHeader tab={tab} project={projectName(path, groups)} metadata={metadata}
      contentWorkspace={path} terminalDetail={detail} terminalDetailHint={tab?.claude_account_source === "default"
        ? "此终端按启动时的普通终端登录标记。若普通终端已换号，请重新打开工作会话确认身份。" : detail}
      busy={busy} onHome={() => setHome(true)}
      sidebarButton={sidebarButton(paneSide(tab?.id))} exitButton={expandButton(tab)}
      location={location} triggerRef={!tab || tab.id === active ? terminalTrigger : undefined}
      menu={terminalMenuAt(location)} onClose={() => closeTab(tab)} />;
  };
  const recoverEditor = async tab => {
    if (!(await confirm({ title: "重启编辑器？", body: "请先保存文件。浏览器、终端和环境会继续运行。", confirm: "重启编辑器" }))) return;
    setRecoveringEditor(true);
    try { await api("/browser/editor-restart", { workspace: tab.workspace || workspace }); notify("编辑器已恢复"); }
    catch (error) { notify(error.message); }
    finally { setRecoveringEditor(false); }
  };
  const paneTabs = tab => !dualBrowser ? browserTabs : browserTabs.filter(item =>
    item.id === tab.id || (tab.id === leftId && item.id !== rightId));
  const browserHeader = tab => <BrowserHeader vault={passwordVault} onManageVault={() => openSettings("vault")} tab={tab} tabs={paneTabs(tab)} metadata={metadata}
    vertical={browserTabLayout === "vertical"}
    ready={status.ready} busy={busy} focused={active === tab.id} compact={split} splitDrag={splitDrag}
    sidebarButton={sidebarButton(paneSide(tab.id))} exitButton={expandButton(tab)}
    triggerRef={tab.id === active ? terminalTrigger : undefined}
    location={split ? `heading-${tab.id}` : "heading"} menu={terminalMenuAt(split ? `heading-${tab.id}` : "heading")}
    back={tab.kind !== "browser-settings" && (browser.split?.history?.[tab.id]?.back ?? (tab.id === browser.active && browser.back))}
    forward={tab.kind !== "browser-settings" && (browser.split?.history?.[tab.id]?.forward ?? (tab.id === browser.active && browser.forward))}
    action={action} onSelect={select} onClose={closeTab} onRename={renameTab} onPin={pinTab} onReorder={reorder}
    onNew={newPage} onNavigate={address => navigate(tab.id, address)} onOpen={openBrowserPage}
    onNewPrivate={() => openBrowserPage(undefined, true)} onNewRegular={() => openBrowserPage(undefined, false)}
    onSettings={openBrowserSettings}
    onReload={() => tab.kind === "browser-settings" ? setBrowserSettingsRevision(value => value + 1) : action("/browser/reload", { id: tab.id })}
    onCloseBrowser={!dualBrowser || tab.id === rightId ? closeBrowser : undefined}
    downloads={downloads} onStatus={notify}
    onEditor={() => openEditor(tab.workspace || workspace)} recoveringEditor={recoveringEditor}
    onRecover={() => recoverEditor(tab)} />;
  const dialogs = (
    <>
      <BrowserDataHost tabs={browserTabs} onSettings={openBrowserSettings} onInternal={openInternalPage} onVault={() => openSettings("vault")} />
      {dialog && (
        <Modal
          title={dialog.title}
          anchor={dialog.anchor}
          onClose={dialog.pending ? undefined : () => finishDialog(false)}
          footer={
            <>
              <button disabled={dialog.pending} onClick={() => finishDialog(false)}>取消</button>
              {dialog.secondary && (
                <button disabled={dialog.pending} onClick={() => finishDialog("secondary")}>
                  {dialog.secondary}
                </button>
              )}
              <button className={dialog.danger ? "danger-fill" : "primary"} disabled={dialog.pending} onClick={() => finishDialog(true)}>
                {dialog.pending ? dialog.pendingLabel : dialog.confirm}
              </button>
            </>
          }
        >
          <div className="modal-body">
            <p>{dialog.body}</p>
            {dialog.error && <div className="error-message" role="alert">{dialog.error}</div>}
          </div>
        </Modal>
      )}
      {quitting && (
        <Modal
          title={quitting.error ? "未能完成退出" : "正在停止环境"}
          onClose={cancelAutomaticExit}
          footer={
            <>
              <button onClick={cancelAutomaticExit}>
                {quitting.error ? "留在 Super Cage" : "取消自动退出"}
              </button>
              {quitting.error && (
                <button className="primary" onClick={stopAndQuit}>
                  重试停止并退出
                </button>
              )}
            </>
          }
        >
          <div
            className="modal-body"
            role={quitting.error ? "alert" : "status"}
          >
            <p>
              {quitting.error ||
                (connectionError
                  ? "正在恢复工作台连接，恢复后会继续确认停止结果。"
                  : "停止完成后 Super Cage 会自动退出。取消自动退出只保留窗口，环境仍会停止。")}
            </p>
            {!quitting.error && (
              <p>
                <LoaderCircle size={16} className="spin" />{" "}
                {status?.job?.running
                  ? status.job.message
                  : "正在确认环境已停止…"}
              </p>
            )}
          </div>
        </Modal>
      )}
    </>
  );
  if (fatal)
    return (
      <>
        <div className="boot-screen">
          <Drawing name="plug" className="boot-drawing" />
          <h1>连接 Super Cage 工作台</h1>
          <p>{fatal}</p>
          <p className="muted">
            请从 Super Cage 应用打开，或在终端运行 <code>cage desktop</code>。
          </p>
          <button
            className="primary"
            onClick={() =>
              nativeAvailable() ? native("reconnect") : location.reload()
            }
          >
            重新连接
          </button>
        </div>
        {dialogs}
      </>
    );
  if (!status || !settings)
    return (
      <>
        <div className="boot-screen">
          <BrandMark size={56} />
          <h1>Super Cage</h1>
          <p>
            <LoaderCircle size={16} className="spin" />
            正在连接工作区…
          </p>
        </div>
        {dialogs}
      </>
    );
  return (
    <SafetyChecklistContext.Provider value={() => setSafetyOpen(true)}>
    <EnvironmentRestartContext.Provider value={running && !busy && !startRequest ? restartEnvironment : null}>
    <div className={`app ${sidebarCollapsed ? "sidebar-collapsed" : ""}`}>
      <div
        className="workbench-shell"
        inert={settingsOpen || undefined}
      >
        {!sidebarCollapsed && (
          <WorkspaceSidebar
            onClaudeAccounts={() => setManagementPage("claude-accounts")}
            claudeAccountsOpen={managementPage === "claude-accounts"}
            onCodexAccounts={() => setManagementPage("codex-accounts")}
            codexAccountsOpen={managementPage === "codex-accounts"}
            tailscaleOpen={managementPage === "tailscale"}
            updateAction={<AppUpdateButton updates={appUpdates} />}
            workspace={selectedProject || status.workspace}
            guestSystem={account?.guest_system || status.guest_system || guestSystem?.guest_system || "debian-13"}
            projects={projects}
            projectGroups={groups}
            tabs={displayTabs}
            sessions={numberedSessions}
            sessionError={cliSessions.error}
            sessionRefreshing={cliSessions.loading}
            onSessionRefresh={cliSessions.refresh}
            onProjectRefresh={projectFilesCached ? refreshProjectFiles : undefined}
            projectRefreshing={projectRefreshing}
            onSessionOpen={openSessionTerminal}
            openingSession={openingSession}
            sessionOpenError={sessionOpenError}
            onSessionArchive={archiveSession}
            onSessionPin={pinSession}
            onSessionRename={session => setRename({id:session.id, title:session.title, session:true})}
            saved={metadata}
            active={managementPage ? "" : active}
            home={!managementPage && showHome}
            busy={busy || storageRecovery}
            ready={status.ready}
            running={running}
            runtimeLabel={runtimeLabel}
            network={network}
            onHome={() => { setManagementPage(""); setDesktopOpen(false); setHome(true); }}
            browserAvailable={browserAvailable}
            editorAvailable={editorAvailable}
            projectsAvailable={projectsAvailable}
            onOpenProject={(path = "") => setProjectDialog({ path })}
            onSelect={select}
            onClose={closeTab}
            onRename={renameTab}
            onPin={pinTab}
            splitDrag={splitDrag}
            onReorder={reorder}
            onSessionReorder={(path, order) => changeTabMetadata({ ...metadata,
              sessionOrder: { ...metadata.sessionOrder, [path]: order } })}
            onProjectSelect={setSelectedProject}
            onNewTerminal={newTerminal}
            terminalMenuAt={terminalMenuAt}
            onBrowser={openBrowser}
            onTailscale={() => {
              setManagementPage("tailscale");
              if (window.matchMedia("(max-width: 520px)").matches) setSidebarCollapsed(true);
            }}
            desktopOpen={!managementPage && desktopOpen}
            onClaudeWeb={() => openBrowserPage("https://claude.ai/")}
            onClaudeDesktop={() => { setManagementPage(""); setHome(false); setDesktopOpen(true); }}
            onClaudeScience={() => setManagementPage("claude-science")}
            claudeScienceOpen={managementPage === "claude-science"}
            onEditor={openEditor}
            onSettings={openSettings}
            onAccountConfirm={confirm}
            onAccountStatus={notify}
            onSafety={() => setSafetyOpen(true)}
            onCollapse={() =>
              persist("cage-sidebar-collapsed", true, setSidebarCollapsed)
            }
          />
        )}
        <div className="workbench-main">
          {!managementPage && showHome && terminalHeader(null)}
          {!managementPage && status.ready && status.guest_system === "windows-remote" && !status.workload_ready && !connectionError && <div className="remote-capability-banner" role="status">
            <span>{browserAvailable
              ? `Windows 云端：隔离 PowerShell 和浏览器已接通，外网${guestCapability(status, "network") ? "已开放" : "尚未开放"}；桌面和外部项目仍在适配。`
              : "Windows 云端适配中：隔离 PowerShell 已接通，浏览器、桌面和外部项目尚未接通。"}</span>
            {browserAvailable && <button disabled={!status.ready || busy} onClick={() => openBrowser()}>打开浏览器</button>}
            <button disabled={!status.ready || busy} onClick={() => createTerminal(status.workspace || "", "shell").catch(e => notify(e.message))}>打开 PowerShell</button>
          </div>}
          {browserAvailable && browserError && running && !connectionError && <div className="connection-banner" role="status">
            <span>客机浏览器暂时无法连接。{browserError}</span>
            <button onClick={() => refreshStatus.current?.()}>重试</button>
          </div>}
          {connectionError && (
            <div className="connection-banner" role="alert">
              工作台连接中断，后台状态暂时未知。
              <button
                onClick={() =>
                  nativeAvailable() ? native("reconnect") : location.reload()
                }
              >
                重新连接工作台
              </button>
            </div>
          )}
          {managementPage && (
            <main className="management-page" aria-labelledby={managementPage + "-title"}>
              <header className="chrome management-header">
                {sidebarButton()}
                <h1 id={managementPage + "-title"} tabIndex={-1}>{managementPage === "tailscale" ? "Tailscale" : managementPage === "claude-science" ? "Claude Science" : managementPage === "codex-accounts" ? "Codex 账号管理" : "Claude 账号管理"}</h1>
              </header>
              <div className="management-content">
                {managementPage === "tailscale" ? <TailscalePanel status={status}
                  onOpenBrowser={async url => Boolean(await openBrowserPage(url, false))}
                  onOpenLogin={async (value) => {
                    const tab = await openBrowserPage(value.auth_url, false);
                    if (tab?.id) setTailnetLogin({ tabId: tab.id, returnID: value.return_id });
                    return Boolean(tab?.id);
                  }}
                  onConfirm={confirm} onInstall={async () => {
                    const nativeGuest = ["macos", "windows-11"].includes(status.guest_system);
                    if (nativeGuest && !await confirm({ title: "更新环境组件？", body: "更新会重新连接浏览器和显示组件，请先保存工作。设备身份、公钥和个人数据保留。", confirm: "更新组件" })) return;
                    await action(nativeGuest ? "/environment/update" : "/tailscale/install", {});
                  }} /> : managementPage === "claude-science" ? <ClaudeSciencePanel status={status} onOpen={showBrowserResult} onConfirm={confirm} onDetails={() => setDetails(true)} /> : managementPage === "codex-accounts" ? <CodexAccountsPanel store={codexProfiles} tabs={tabs} onConfirm={confirm}
                  running={status.vm_running} environmentBusy={busy || storageRecovery}
                  login={(id) => createTerminal(workspace, "codex-login", id)} /> : <ClaudeAccountsPanel store={claudeProfiles} tabs={tabs} onConfirm={confirm}
                  running={status.vm_running} environmentBusy={busy || storageRecovery}
                  login={(id) => createTerminal(workspace, "claude-login", id)} />}
              </div>
            </main>
          )}
          <main className={`workspace ${split ? "workspace-split" : ""}`} ref={workspaceBox} hidden={Boolean(managementPage)}
            style={{ "--split-left": `${100 * (dualBrowser && nativeSplit?.fraction || 0.5)}%` }}>
            {dropSide && <div aria-hidden="true" className={`split-drop split-${dropSide}`}>{dropSide === "left" ? "放到左侧" : "放到右侧"}</div>}
            {!storageRecovery && desktopOpen && macGuest && <MacOSAppWindow key="claude-desktop" uploads={desktopFiles.uploads} keyboard={status.keyboard} remoteRef={remote}
              onStatus={notify} onFallback={(text, message) => setTransfer({ text, message })} visible={!settingsOpen && !managementPage} onConfirm={confirm}
              sidebarCollapsed={sidebarCollapsed} onExpand={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)} />}
            {!storageRecovery && desktopOpen && !macGuest && <ClaudeDesktop
              uploads={desktopFiles.uploads}
              onProjectOpened={() => refreshStatus.current?.()}
              status={status} visible={!settingsOpen && !managementPage} remoteRef={remote}
              onStatus={notify} onFallback={(text, message) => setTransfer({ text, message })}
              onConfirm={confirm} onDetails={() => setDetails(true)}
              sidebarCollapsed={sidebarCollapsed}
              onExpand={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)}
            />}
            {status.environment_update_required && !busy && (
              <div className="environment-update-banner" role="status">
                <span>{status.environment_update_error || "环境组件需要更新。请先保存编辑器中的文件。"}</span>
                <button onClick={() => openSettings("project")}>查看并更新</button>
              </div>
            )}
            {!storageRecovery && (status.vm_running || terminal) && (
              <>
                {macGuest && !desktopOpen && !showHome && webPanes.length > 0 && (
                  <section className={`pane-lane browser-lane split-${paneSide(webPanes[0].id)}`}>
                    {status.ready && <MacOSAppWindow key="chrome" browser onOpenPage={openBrowserPage} visible={!settingsOpen} uploads={desktopFiles.uploads}
                      keyboard={status.keyboard} remoteRef={remote} onStatus={notify}
                      sidebarCollapsed={sidebarCollapsed} onExpand={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)}
                      onFallback={(text, message) => setTransfer({ text, message })} />}
                  </section>
                )}
                {!macGuest && !desktopOpen && !showHome && webPanes.length > 0 && (
                  <section className={`pane-lane browser-lane ${browserTabLayout === "vertical" ? "vertical-browser" : ""} ${dualBrowser ? "" : "split-" + paneSide(webPanes[0].id)}`}>
                    {browserTabLayout === "vertical" && <BrowserTabRail tabs={browserTabs} metadata={metadata}
                      active={liveBrowser?.id} ready={status.ready} busy={busy} splitDrag={splitDrag}
                      sidebarButton={sidebarButton(paneSide(webPanes[0].id))}
                      onSelect={select} onClose={closeTab} onRename={renameTab} onPin={pinTab} onReorder={reorder}
                      onNew={newPage} />}
                    <div className="browser-headers">{webPanes.map(tab =>
                      <div key={paneSide(tab.id) || "single"} className="browser-header-cell" data-focused={active === tab.id}
                        onPointerDownCapture={() => focusHeader(tab)} onFocusCapture={() => focusHeader(tab)}>
                        {browserHeader(tab)}
                      </div>)}</div>
                    <div className="pane-body" onPointerDownCapture={() => {
                      if (!dualBrowser && liveBrowser?.id !== active) focusHeader(liveBrowser);
                    }}>
                      {status.ready && liveBrowser?.kind === "browser-settings" && <BrowserSettingsPage
                        key={`${liveBrowser.id}:${browserSettingsRevision}`} tab={liveBrowser} tabs={tabs}
                        currentHome={settings.browser_home_url} layout={browserTabLayout}
                        onLayoutChange={value => persist("cage-browser-tab-layout", value, setBrowserTabLayout)}
                        onSectionChange={openBrowserSettings}
                        onSaved={next => setSettings(previous => ({ ...previous, ...next }))}
                        onInternal={openInternalPage} onOpenPage={openSettingsPageResult} onConfirm={confirm} />}
                      {status.ready && <RemoteView ref={remote} visible={Boolean(liveBrowserVisible && !settingsOpen && liveBrowser?.kind !== "browser-settings")}
                        copyScope={webPanes.map(tab => `${tab.id}:${tab.document || ""}`).join("|")}
                        focused={dualBrowser || liveBrowser?.id === active} keyboard={status.keyboard}
                        guestPlatform={status.guest_system} systemClipboard={browser.capabilities?.systemClipboard !== false}
                        tabId={liveBrowser?.id} onStatus={notify}
                        onFallback={(text, message) => setTransfer({ text, message })} />}
                      {!status.ready && <div className="view-overlay">
                        <RefreshCw /><h3>浏览器连接暂时不可用</h3>
                        <p>环境和终端仍可运行；恢复浏览器不会停止现有终端。</p>
                        <button className="primary" disabled={busy} onClick={() => startEnvironment({})}>恢复浏览器</button>
                      </div>}
                      {status.ready && !splitReady && <div className="view-overlay">正在调整浏览器布局…</div>}
                    </div>
                  </section>
                )}
                {tabs.filter(t => t.kind === "terminal").map(tab => {
                  const visible = !desktopOpen && !showHome && panes.some(pane => pane.id === tab.id);
                  return <section key={tab.id} className={`pane-lane terminal-lane split-${paneSide(tab.id)}`}
                    data-focused={active === tab.id} hidden={!visible}
                    onPointerDownCapture={() => focusHeader(tab)} onFocusCapture={() => focusHeader(tab)}>
                    {visible && terminalHeader(tab)}
                    <div className="pane-body">
                      {tab.pending && <TerminalPending error={tab.error}
                        onRetry={() => createTerminal(tab.workspace, tab.terminalKind, undefined, tab)} />}
                      {!tab.pending && <TerminalView
                        session={terminalSession(tab)}
                        onLoginTab={({ id }) => showBrowserResult({ id, kind: "web", login: true, incognito: true, title: "账号授权" })}
                        onControls={(control) =>
                          control
                            ? terminalControls.current.set(tab.id, control)
                            : terminalControls.current.delete(tab.id)
                        }
                        visible={visible && !settingsOpen && !managementPage}
                        focused={active === tab.id}
                        onStatus={notify}
                        onAccountChanged={view => { if (view?.desktop_terminal_id && view.desktop_terminal_id !== tab.sessionId) void openSessionTerminal({id:tab.cli_session_id}); }}
                      />}
                    </div>
                  </section>;
                })}
              </>
            )}
            {showHome && (
              <WorkbenchHome
                status={status}
                storageRecovery={storageRecovery}
                setupProgress={setupProgress}
                busy={busy}
                running={running}
                setupProxy={setupProxy}
                network={network}
                runtimeLabel={runtimeLabel}
                nativeGuestLabel={nativeGuestLabel}
                guestSystem={guestSystem}
                onGuestSystem={setGuestSystem}
                account={account}
                connectionError={connectionError}
                workspace={workspace}
                projectsAvailable={projectsAvailable}
                macGuest={macGuest}
                editorAvailable={editorAvailable}
                contentWorkspace={contentWorkspace}
                groups={groups}
                projects={projects}
                sessions={numberedSessions}
                tabs={tabs}
                openingSession={openingSession}
                onSessionOpen={openSessionTerminal}
                onSettings={openSettings}
                onDetails={() => setDetails(true)}
                onStop={stop}
                onRetrySetup={() => startEnvironment(status.job.kind === "guest-install" ? { guest_system: status.job.setup_progress.system } : {})}
                primaryDisabled={busy || Boolean(connectionError) || (!running && !setupProxy &&
                  ((!account?.guest_system && status.local_vm_storage && !isRemoteClient() && guestSystem === undefined) || !validGuestChoice(guestSystem)))}
                onPrimary={(event) => {
                  if (!running && guestSystem?.activate) {
                    api("/environment/guest-systems/activate", {}).then(() => native("reconnect")).catch((e) => setToast(e.message));
                  } else if (!running && setupProxy) openSettings("network");
                  else if (!running || !status.ready)
                    startEnvironment(!running && guestSystem?.guest_system === "debian-13" && !guestSystem.clone ? { guest_system: "debian-13" } : !running && guestSystem ? {
                      guest_system: guestSystem.guest_system, media: guestSystem.media, sha256: guestSystem.sha256,
                      clone_key: guestSystem.clone_key,
                      editor_terms: guestSystem.editor_terms,
                    } : {});
                  else if (!workspace && projectsAvailable)
                    setProjectDialog({ path: "" });
                  else newTerminal(workspace, event.currentTarget);
                }}
                onNewTerminal={newTerminal}
                onNewTerminalHere={newTerminalHere}
                onEditor={openEditor}
                onOpenProject={(path) => setProjectDialog({ path })}
                terminalMenuAt={terminalMenuAt}
              />
            )}
          </main>
          <footer className="status-bar">
            {sidebarCollapsed && <AppUpdateButton updates={appUpdates} />}
            <button onClick={() => setSafetyOpen(true)} title="网络与隔离检查">
              <EnvironmentShield
                size={14}
                healthy={status.ready && network.mode === "open"}
              />
              <span>{running ? network.label : "网络检查"}</span>
            </button>
            <span className="status-divider" />
            <button onClick={() => openSettings("network")}>
              <MapPin size={12} />
              <span>
                {status.network_error
                  ? "出口待确认"
                  : status.location || status.safety?.exit_ip
                    ? [
                        status.safety?.exit_ip || status.location?.ip,
                        status.location?.country,
                        status.location?.city,
                      ].filter(Boolean).join(" · ")
                    : "指定代理出口"}
              </span>
            </button>
            <span className="status-divider" />
            <button onClick={() => openSettings("region")}>
              <Clock3 size={12} />
              {settings.timezone}
              {settings.auto_timezone ? " · 自动" : ""}
            </button>
            <span className="status-divider" />
            <NetworkLatency
              key={`${proxy?.selected}:${proxy?.revision}:${running}:${busy}:${Boolean(connectionError)}`}
              enabled={running && !busy && !connectionError}
              revision={proxy?.revision}
              selected={proxy?.selected}
            />
            <span className="status-spacer" />
            <AudioControls audio={audio} />
            <EnvironmentClock timezone={settings.timezone} clock12Hour={settings.clock_12_hour} />
            <span className="status-feedback" role="status">
              {/* The footer's own restart button sits beside this text. */}
              <EnvironmentRestartContext.Provider value={null}>
              <SafetyMessage message={(busy
                  ? status.job.message
                  : running
                    ? needsEnvironmentRestart(status.job?.error)
                      ? status.job.error
                      : status.ready
                        ? isCloudWorkbench() ? "文件保存在云端" : "文件与本机同步"
                        : "环境运行中 · 浏览器待恢复"
                    : "")} />
              </EnvironmentRestartContext.Provider>
            </span>
            {status.job?.log && (
              <button title="操作详情" onClick={() => setDetails(true)}>
                <MoreHorizontal size={15} />
              </button>
            )}
            <button
              className="status-restart"
              onClick={restartEnvironment}
              disabled={Boolean(startRequest) || !running || busy}
              title="停止并重新启动当前环境"
            >
              <RotateCcw size={12} />
              <span>重启环境</span>
            </button>
            <button
              onClick={stop}
              disabled={
                Boolean(startRequest) || (!running && !busy) || (status?.job?.kind === "stop" && busy)
              }
              title="停止当前环境"
            >
              <Power size={12} />
              <span>停止环境</span>
            </button>
          </footer>
        </div>
      </div>
      <BrowserUploads requests={status.ready ? browser.uploads : []} onStatus={notify} />
      {settingsOpen && (
        <SettingsPanel
          current={settings}
          status={status}
          initialSection={settingsSection}
          onClose={() => setSettingsOpen(false)}
          onSaved={refreshSettings}
          onConfirm={confirm}
        />
      )}
      {projectDialog && (
        <ProjectDialog
          current={settings}
          status={status}
          initialPath={projectDialog.path}
          onClose={() => setProjectDialog(null)}
          onSettings={() => { setProjectDialog(null); openSettings("project"); }}
          onOpened={openedProject}
          onRemoved={async (removed, workspace) => {
            await refreshSettings();
            await refreshStatus.current?.();
            if ((groups.find(group => group.workspace === removed)?.folders || [removed]).includes(selectedProject)) setSelectedProject(workspace);
          }}
        />
      )}
      {rename && (
        <Modal
          title="重命名标签"
          onClose={() => setRename(null)}
          footer={
            <>
              <span className="save-note">仅修改工作台中显示的名称</span>
              <button onClick={() => setRename(null)}>取消</button>
              <button
                className="primary"
                form="rename-tab"
                type="submit"
                disabled={!rename.title.trim()}
              >
                保存名称
              </button>
            </>
          }
        >
          <form
            className="modal-body project-form"
            id="rename-tab"
            onSubmit={async (event) => {
              event.preventDefault();
              if (!rename.title.trim()) return;
              if (rename.session) {
                try {
                  await api("/cli/sessions/" + rename.id + "/metadata", {title:rename.title.trim()});
                  sessionsChanged(); setRename(null);
                } catch (error) { notify(error.message); }
                return;
              }
              const tab = tabs.find(tab => tab.id === rename.id);
              if (tab?.cli_session_id) {
                try {
                  await updateSharedMetadata(tab, {title:rename.title.trim()});
                  setRename(null);
                } catch (error) { notify(error.message); }
                return;
              }
              changeTabMetadata({
                ...metadata,
                [rename.id]: {
                  ...metadata[rename.id],
                  title: rename.title.trim(),
                },
              });
              setRename(null);
            }}
          >
            <label htmlFor="tab-title">标签名称</label>
            <input
              id="tab-title"
              autoFocus
              maxLength={100}
              value={rename.title}
              onChange={(event) =>
                setRename({ ...rename, title: event.target.value })
              }
            />
          </form>
        </Modal>
      )}
      {transfer && (
        <Modal
          title="文本传递"
          onClose={() => setTransfer(null)}
          footer={
            <>
              <span className="save-note">仅在你的操作下传递文本</span>
              <button onClick={() => setTransfer(null)}>关闭</button>
              <button
                className="primary"
                onClick={() => {
                  remote.current?.pasteText(transfer.text);
                  setTransfer(null);
                }}
              >
                粘贴到 Super Cage
              </button>
            </>
          }
        >
          <div className="modal-body">
            <p>{transfer.message}</p>
            <textarea
              aria-label="待传递文本"
              className="transfer-text"
              value={transfer.text}
              onChange={(e) =>
                setTransfer({ ...transfer, text: e.target.value })
              }
            />
          </div>
        </Modal>
      )}
      {details && (
        <Modal title="操作详情" wide onClose={() => setDetails(false)}>
          <div className="modal-body">
            <p><SafetyMessage message={status.job.message} /></p>
            {status.job.error && (
              <div className="error-message"><SafetyMessage message={status.job.error} /></div>
            )}
            <pre className="job-log">{status.job.log || "暂无日志"}</pre>
          </div>
        </Modal>
      )}
      {safetyOpen && (
        <SafetyPanel
          status={
            connectionError
              ? {
                  ...status,
                  safety: {
                    ...status.safety,
                    gate: null,
                    error: "控制服务连接中断，无法确认当前外网状态",
                  },
                }
              : status
          }
          onClose={() => setSafetyOpen(false)}
          onSettings={openSettings}
          onOpenPage={openBrowserPage}
          onConfirm={confirm}
        />
      )}
      {toast && (
        <div className="toast" role="status" key={toast}>
          <EnvironmentRestartContext.Provider value={null}>
            <SafetyMessage message={toast} />
          </EnvironmentRestartContext.Provider>
        </div>
      )}
      {dialogs}
    </div>
    </EnvironmentRestartContext.Provider>
    </SafetyChecklistContext.Provider>
  );
}
export default function Workbench() {
  return <AccountProvider><AppearanceProvider><AccountGate><InitializationGate><App /></InitializationGate></AccountGate></AppearanceProvider></AccountProvider>;
}
