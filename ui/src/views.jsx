import { isMac, terminalModifier } from "./platform.mjs";
import React, {
  useEffect,
  useRef,
  useState,
  useImperativeHandle,
  forwardRef,
} from "react";
import { useAppearance } from "./appearance";
import { terminalTheme, systemCodeFont } from "./appearance-state.mjs";
import { Terminal } from "../.generated/xterm.mjs";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { agentLoginLinkRegex, codexDeviceLink, isAgentLoginLink, isCodexDeviceLink } from "./terminal-login-link.mjs";
import { FitAddon } from "@xterm/addon-fit";
import { Unicode11Addon } from "@xterm/addon-unicode11";
import { attachTouchScroll } from "./terminal-touch-scroll.mjs";
import { answeredBytes, quietReplayAnswers } from "./terminal-replay-answers.mjs";
import { api, writeClipboard, nativeAvailable } from "./api";
import { runtimeSocketURL } from "./client-context.mjs";
import { readClipboardContent, transferFiles, hasFiles, imagePayload, prepareClipboardPaste, sendImage } from "./image-transfer";
import { terminalClipboardText } from "./terminal-clipboard.mjs";
import {
  terminalConnectTimeoutMs, terminalHistoryPlan, terminalKeepaliveMessage, terminalKeepaliveMs, terminalLiveness,
  terminalNoticeDelayMs, terminalProbeMs, terminalReconnectPlan, terminalStreamPath, terminalSuspendMs, terminalViewer, terminalWatchdogMs,
} from "./terminal-reconnect.mjs";
import { sendFiles, shellFilePaths } from "./file-transfer";
import { installRemoteInput } from "./remote-input";
import { useWindowVisibility } from "./window-visibility";
import { displayGeometry, displayRatio, displaySizeSync, watchPixelRatio } from "./remote-display.mjs";
import { displayRestoreAfterMs } from "./display-session.mjs";
import { displayKey, displaySessions } from "./remote-session.js";
import { RefreshCw, Clipboard, ArrowUpRight } from "lucide-react";
import { SessionAccount } from "./session-account";
import { AgentTerminalBar } from "./agent-terminal-bar";
import { codexLaunchRefused, createTerminalRenderer, terminalLastLine } from "./terminal-renderer.mjs";
import { cagePerf } from "./display-perf.mjs";
import { EmptyState } from "./drawings";
import "@xterm/xterm/css/xterm.css";
import "./terminal-pending.css";

const terminalAccountRequest = (path, options) => api("/cli" + path, options.body);

export const RemoteView = forwardRef(function RemoteView(
  { visible, focused = true, keyboard, guestPlatform = "linux", systemClipboard = true, tabId, copyScope = tabId, onStatus, onFallback, desktop = false, zoom = 1 },
  ref,
) {
  const windowVisible = useWindowVisibility();
  const screenVisible = visible && windowVisible;
  const box = useRef(),
    rfb = useRef(),
    input = useRef(),
    current = useRef({ visible, keyboard, onStatus, onFallback }),
    displayResize = useRef();
  current.current = { visible: screenVisible && focused, keyboard, guestPlatform, systemClipboard, tabId, onStatus, onFallback, zoom };
  const failures = useRef(0);
  // A stream kept from the previous visit shows its picture at once.
  const [connection, setConnection] = useState(() =>
    displaySessions.get(displayKey(desktop))?.state === "connected" ? "connected" : "connecting"),
    [attempt, setAttempt] = useState(0),
    [sizeError, setSizeError] = useState(false);
  useEffect(() => { displayResize.current?.update(); }, [zoom]);
  useEffect(() => { input.current?.cancelCopy(); }, [copyScope]);
  useImperativeHandle(ref, () => ({
    focus: () => rfb.current?.focus(),
    edit: (action) =>
      input.current?.focused()
        ? input.current?.edit(action)
        : false,
    copy: () => input.current?.copy(),
    receiveCopy: (value) => input.current?.receiveCopy(value),
    paste: () => input.current?.readAndPaste(),
    pasteText: (text) => input.current?.paste(text),
    transfer: () =>
      onFallback(input.current?.text() || "", "可在此使用本机复制、粘贴。"),
  }));
  useEffect(() => {
    if (!screenVisible) return;
    const container = box.current;
    if (!container) return;
    let disposed = false,
      resizeObserver,
      unwatchDensity,
      reconnectTimer;
    // The stream outlives this pane: hiding it (or switching to a terminal)
    // keeps the stream when the server supports that, see display-session.mjs.
    const session = displaySessions.acquire(displayKey(desktop), { desktop, guestPlatform });
    if (session.state !== "connected") setConnection("connecting");
    setSizeError(false);
    const retry = () => {
      if (disposed) return;
      setConnection("disconnected");
      clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(
        () => setAttempt((v) => v + 1),
        Math.min(1000 * 2 ** failures.current++, 5000),
      );
    };
    const release = () => {
      input.current?.dispose();
      input.current = null;
      resizeObserver?.disconnect();
      resizeObserver = null;
      unwatchDensity?.();
      unwatchDensity = null;
      session.info?.keys?.stop();
      displayResize.current = null;
      rfb.current = null;
    };
    const view = {
      connected(session, { resumed, hiddenFor }) {
        if (disposed) return;
        const client = session.client;
        rfb.current = client;
        failures.current = 0;
        setConnection("connected");
        const resize = () => {
          if (disposed) return;
          const rect = container.getBoundingClientRect();
          const options = {
            desktop, zoom: current.current.zoom, density: session.info.density,
            pixelRatio: session.info.followScreen ? window.devicePixelRatio : undefined,
          };
          session.fitRatio = displayRatio(rect, options) ?? session.fitRatio;
          session.refit();
          session.sizeSync.update(displayGeometry(rect, options));
        };
        view.refresh = resize;
        // One size request queue per stream, so a short visit does not ask
        // the guest again for the size it already has.
        session.sizeSync ??= displaySizeSync(
          async (size) => {
            const reply = await api(desktop ? "/claude-desktop/resize" : "/browser/resize", size);
            // The guest kept its own density: size the display for that one.
            if (size.scale !== undefined && reply?.scale !== size.scale && session.info.followScreen) {
              session.info.followScreen = false;
              session.view?.refresh?.();
            }
          },
          { onError: () => session.view?.sizeError(true), onSuccess: () => session.view?.sizeError(false) },
        );
        displayResize.current = { update: resize, retry: () => session.sizeSync.retry() };
        resizeObserver = new ResizeObserver(resize);
        resizeObserver.observe(container);
        unwatchDensity = watchPixelRatio(resize);
        resize();
        // Back after a long hide: the guest has lowered its refresh rate, and
        // a request for the unchanged size restores it.
        if (resumed && hiddenFor >= displayRestoreAfterMs) session.sizeSync.retry();
        const preparePaste = () => {
          const id = current.current.tabId;
          return prepareClipboardPaste(desktop ? "desktop" : "browser", id, {
            valid: () => !disposed && current.current.visible && current.current.tabId === id,
          });
        };
        input.current = installRemoteInput({
          events: client,
          setKeyboardTarget: target => client.cageSetKeyboardTarget(target),
          sendKey: (...key) => client.sendKey(...key),
          focus: () => client.focus(),
          pasteText: text => {
            if (desktop || current.current.systemClipboard) return client.cagePasteText(text);
            const prepared = preparePaste();
            return client.cageRunInput(() => prepared.send({ text })).finally(() => prepared.dispose());
          },
          runKeyAction: action => client.cageRunInput(action),
        }, container, {
          guestPlatform: () => current.current.guestPlatform,
          systemClipboard: () => current.current.systemClipboard,
          copyText: !desktop
            ? (_sendCopy, valid, { id, context, key }) => {
              if (current.current.systemClipboard !== false) return undefined;
              if (!valid()) throw new Error("复制位置已切换，请重新复制");
              return api("/browser/" + (key === "x" ? "cut" : "copy"), { id, context });
            }
            : undefined,
          setCopyContext: desktop ? undefined : (context, valid) => client.cageRunInput(async () => {
            if (!valid()) return;
            try { await api("/browser/copy-context", { id: current.current.tabId, context }); }
            catch {
              if (valid()) {
                input.current?.cancelCopy();
                current.current.onStatus("复制按钮同步暂未就绪，可使用复制快捷键或文本传递");
              }
              // A metadata failure must not cancel the user's queued click.
            }
          }),
          preparePaste,
          pasteContent: (content, prepared) => {
            const id = current.current.tabId;
            const valid = () => !disposed && current.current.visible && current.current.tabId === id;
            return client.cagePasteContent(content,
              (payload) => {
                if (!valid()) throw new Error("输入目标已切换，请重新粘贴图片");
                return payload.files || payload.fileSelection
                  ? sendFiles(payload, desktop ? "desktop" : "browser", id, { valid })
                  : sendImage(payload, desktop ? "desktop" : "browser", id, { prepared });
              }, valid, !desktop && !current.current.systemClipboard ? text => prepared.send({ text }) : undefined);
          },
          insertText: (text) => {
            if (desktop) {
              if (!window.cageSharedClipboardActive) return client.cagePasteText(text);
              return client.cageRunInput(async () => {
                const valid = () => !disposed && current.current.visible;
                const state = await api("/input", { target: "desktop", id: "desktop", mode: "status" });
                if (!state.available || !valid()) throw new Error("输入位置已失去焦点，请重新点击输入框");
                const session = crypto.randomUUID().replaceAll("-", "");
                const begun = await api("/input", { target: "desktop", id: "desktop", mode: "begin", context: state.context, session, sequence: 0 });
                if (!begun.available || !valid()) throw new Error("输入位置已失去焦点，请重新点击输入框");
                const result = await api("/input", { target: "desktop", id: "desktop", mode: "commit", session, sequence: 1, text });
                if (!result.available) throw new Error("文字输入未完成，请重新点击输入框");
              });
            }
            const id = current.current.tabId;
            return client.cageRunInput(() => {
              if (disposed || !current.current.visible || current.current.tabId !== id)
                throw new Error("输入位置已切换，请重新输入");
              return api("/browser/insert-text", { id, text });
            });
          },
          insertComposition: (id, value) => client.cageRunInput(async () => {
            if (value.mode !== "cancel" && (disposed || !current.current.visible ||
                (desktop ? "desktop" : current.current.tabId) !== id))
              throw new Error("输入位置已切换，请重新输入");
            const result = await api("/input", { target: desktop ? "desktop" : "browser", id, ...value });
            if (value.mode !== "cancel" && !result.available)
              throw new Error("输入位置已失去焦点，请重新输入");
            return result;
          }),
          tabId: () => desktop ? "desktop" : current.current.tabId,
          caretRect: async (session) => {
            const id = desktop ? "desktop" : current.current.tabId;
            const value = await api("/input", { target: desktop ? "desktop" : "browser", id, mode: "status", session });
            return !disposed && current.current.visible &&
              (desktop ? "desktop" : current.current.tabId) === id ? value : null;
          },
          mode: () => current.current.keyboard,
          isActive: () => current.current.visible,
          onStatus: (t) => current.current.onStatus(t),
          onFallback: (...args) => current.current.onFallback(...args),
        });
        if (current.current.visible && !document.activeElement?.closest(".chrome")) client.focus();
      },
      lost() {
        if (disposed) return;
        release();
        retry();
      },
      sizeError(value) { if (!disposed) setSizeError(value); },
    };
    // Counts input that should produce a frame: the time to the next painted
    // frame is in window.__cagePerf.
    const noteInput = () => session.perf?.input();
    container.addEventListener("pointerdown", noteInput, true);
    container.addEventListener("wheel", noteInput, { capture: true, passive: true });
    container.addEventListener("keydown", noteInput, true);
    const detach = session.attach(container, view);
    return () => {
      disposed = true;
      clearTimeout(reconnectTimer);
      container.removeEventListener("pointerdown", noteInput, true);
      container.removeEventListener("wheel", noteInput, { capture: true, passive: true });
      container.removeEventListener("keydown", noteInput, true);
      release();
      detach();
    };
  }, [attempt, screenVisible, desktop]);
  useEffect(() => {
    if (focused && screenVisible && connection === "connected") {
      const t = setTimeout(() => { if (!document.activeElement?.closest(".chrome")) rfb.current?.focus(); }, 80);
      return () => clearTimeout(t);
    }
  }, [focused, screenVisible, connection]);
  return (
    <div className={`remote-view ${visible ? "" : "concealed"}`}>
      <div className="remote-canvas" ref={box} />
      {sizeError && connection === "connected" && (
        <div className="remote-size-warning connection-banner" role="alert">
          画面尺寸未能同步，显示比例可能不准确。
          <button onClick={() => displayResize.current?.retry()}>重试调整</button>
        </div>
      )}
      {connection !== "connected" && (
        <div className="view-overlay">
          <RefreshCw className={connection === "connecting" ? "spin" : ""} />
          <h3>
            {connection === "connecting" ? "正在连接画面" : "画面连接已断开"}
          </h3>
          <p>后台任务不会因画面断开而自动结束。</p>
          {connection === "disconnected" && (
            <button
              className="primary"
              onClick={() => setAttempt((v) => v + 1)}
            >
              重新连接画面
            </button>
          )}
        </div>
      )}
    </div>
  );
});

export function TerminalView({
  session,
  visible,
  focused = true,
  agentConfig = null,
  agentSpec = null,
  agentOAuthDomains = [],
  onStatus,
  onControls,
  onAccountChanged,
  onLoginTab,
  onOutput,
  onLaunchAgent,
  onOpenAgentAccounts,
  onReattachHerdr,
}) {
  const { appearance, theme, variant } = useAppearance();
  const box = useRef(),
    instance = useRef(),
    fit = useRef(),
    socket = useRef(),
    renderer = useRef(),
    streamVisibility = useRef(),
    activateLoginRef = useRef(null);
  // "connecting" until the first stream opens, then "open" or "retrying";
  // "stalled" after repeated quick failures and "ended" once the session exits.
  const [link, setLink] = useState("connecting"),
    [notice, setNotice] = useState(false),
    [preparing, setPreparing] = useState(Boolean(session.cli_session_id && !session.closed));
  const revealStartup = useRef();
  const reconnectNow = useRef();
  const closedRef = useRef(session.closed);
  closedRef.current = session.closed;
  const visibleRef = useRef(visible);
  visibleRef.current = visible;
  const focusedRef = useRef(focused);
  focusedRef.current = focused;
  // Told of every output frame (main.jsx: "正在打开 Codex…" until the first).
  const outputRef = useRef(onOutput);
  outputRef.current = onOutput;
  const agentOptsRef = useRef({ agentConfig, agentOAuthDomains, sessionOAuthDomains: session.oauth_domains });
  agentOptsRef.current = { agentConfig, agentOAuthDomains, sessionOAuthDomains: session.oauth_domains };
  const domainsKey = [...new Set([...(session.oauth_domains || []), ...(agentOAuthDomains || [])])].sort().join(",");
  useEffect(() => {
    let disposed = false, emittingPaste = false, inputChain = Promise.resolve();
    const getAllowedHosts = () => [
      ...(agentOptsRef.current.sessionOAuthDomains || []),
      ...(agentOptsRef.current.agentOAuthDomains || []),
    ];
    const enqueue = (action) => {
      const result = inputChain.then(action);
      inputChain = result;
      const reset = () => { if (inputChain === result) inputChain = Promise.resolve(); };
      result.then(reset, reset);
      return result;
    };
    const openExternalTerminalLink = (uri) => {
      let parsed;
      try {
        parsed = new URL(uri);
      } catch {
        return;
      }
      if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return;
      if (window.confirm(`打开此终端链接？\n${uri}`)) {
        window.open(uri, "_blank", "noopener,noreferrer");
      }
    };
    const activateLogin = async (_event, uri) => {
      if (disposed || !visibleRef.current || !focusedRef.current) return;
      if (isCodexDeviceLink(uri)) {
        window.open(uri, "_blank", "noopener,noreferrer");
        return;
      }
      if (!isAgentLoginLink(uri, getAllowedHosts())) {
        openExternalTerminalLink(uri);
        return;
      }
      try {
        const tab = await api(`/terminals/${session.id}/login-link`, { url: uri });
        if (!disposed && visibleRef.current && focusedRef.current) onLoginTab?.(tab);
      } catch (error) { if (!disposed) onStatus(error.message); }
    };
    activateLoginRef.current = activateLogin;
    const terminal = new Terminal({
      linkHandler: { activate(event, uri) {
        if (isCodexDeviceLink(uri)) return void window.open(uri, "_blank", "noopener,noreferrer");
        if (isAgentLoginLink(uri, getAllowedHosts())) return void activateLogin(event, uri);
        // Keep xterm's user confirmation for other explicit HTTP(S) links.
        openExternalTerminalLink(uri);
      } },
      fontFamily: theme.fonts.code || systemCodeFont,
      fontSize: appearance.codeFontSize,
      lineHeight: 1.3,
      cursorBlink: true,
      macOptionClickForcesSelection: true,
      minimumContrastRatio: 4.5,
      scrollback: 6000,
      theme: terminalTheme(theme, variant),
      // terminal.unicode, for the Unicode 11 widths below.
      allowProposedApi: true,
    });
    const fitter = new FitAddon();
    terminal.loadAddon(fitter);
    // Emoji and other wide characters take two cells, as Codex, Claude Code
    // and the shell's line editor count them; xterm's default (Unicode 6)
    // gives many one, which shifts the rest of their line.
    terminal.loadAddon(new Unicode11Addon());
    terminal.unicode.activeVersion = "11";
    // The Codex device page opens in the user's own browser; the one-time
    // code stays in this terminal and no credential crosses into the page.
    terminal.loadAddon(new WebLinksAddon((_event, uri) => {
      if (!disposed && isCodexDeviceLink(uri)) window.open(uri, "_blank", "noopener,noreferrer");
    }, { urlRegex: codexDeviceLink }));
    terminal.open(box.current);
    instance.current = terminal;
    fit.current = fitter;
    // A finger dragged over the terminal scrolls it as a wheel would.
    const stopTouch = attachTouchScroll(terminal, box.current);
    // WebGL draws the grid off the main thread's layout work; the DOM
    // renderer stays as the fallback. See terminal-renderer.mjs.
    const perf = cagePerf().terminal(session.id);
    // A renderer change can change the cell width: fit and tell the server.
    const draw = createTerminalRenderer(terminal, { onChange: (kind) => { perf.renderer = kind; resize(); } });
    renderer.current = draw;
    if (visibleRef.current) draw.show();
    // One xterm lives as long as the view; streams come and go beneath it.
    let ws = null;
    const isOpen = () => ws?.readyState === WebSocket.OPEN;
    let startupPending = Boolean(session.cli_session_id && !session.closed);
    let startupVisible = !startupPending;
    setPreparing(startupPending);
    const reveal = () => {
      if (disposed || startupVisible) return;
      startupVisible = true;
      clearTimeout(startupTimer);
      setPreparing(false);
      if (visibleRef.current && focusedRef.current) terminal.focus();
    };
    revealStartup.current = reveal;
    // A stalled launch or login prompt must remain accessible. The output is
    // retained in xterm throughout preparation, including on reconnect.
    const startupTimer = startupPending ? setTimeout(reveal, 8000) : null;
    const painted = () => {
      if (disposed || startupPending || startupVisible) return;
      const buffer = terminal.buffer.active;
      const screen = Array.from({ length: terminal.rows }, (_, row) =>
        buffer.getLine(buffer.viewportY + row)?.translateToString(true) || "",
      ).join("").replace(/\s/g, "");
      // Channel registration can precede the TUI's final repaint. Reveal only
      // after the confirmation has left the visible screen.
      if (!screen.includes("WARNING:Loadingdevelopmentchannels")) reveal();
    };
    const startupRender = terminal.onRender(painted);
    const resize = () => {
      if (!visibleRef.current) return;
      try {
        fitter.fit();
        if (isOpen())
          ws.send(
            JSON.stringify({
              type: "resize",
              cols: terminal.cols,
              rows: terminal.rows,
            }),
          );
      } catch {}
    };
    // Stream state survives reconnects; the xterm keeps everything it showed.
    // offset is the absolute position after the last byte received, or null
    // before any history (or from a server that cannot resume).
    const stream = { offset: null, heartbeats: false, lastReceived: 0, lastPing: 0, probe: 0 };
    let failures = 0, openedAt = 0, ended = false;
    let retryTimer = null, noticeTimer = null, probeTimer = null, openTimer = null, watchdog = null;
    // hiddenSince is when the view was last hidden (0 while shown); a view
    // hidden for terminalSuspendMs is suspended: no stream until it is shown.
    let hiddenSince = visibleRef.current ? 0 : Date.now(), suspended = false, suspendTimer = null;
    // A view mounted hidden (a tab restored behind another) opens its stream
    // when it is first shown: until then its xterm cannot take its size, and
    // a replay drawn at another size stays garbled.
    let waiting = !visibleRef.current;
    // Bytes still to come in the current replay, or one whole frame from a
    // server without offsets. Replayed output never writes the clipboard.
    // answeredRemaining: the replay's leading bytes a viewer was shown before;
    // their queries were answered then (terminal-replay-answers.mjs).
    let replayRemaining = 0, replayFrame = false, replaying = 0, answeredRemaining = 0;
    const writeReplay = quietReplayAnswers(terminal);
    const clipboardHandler = terminal.parser.registerOscHandler(52, async (data) => {
      if (disposed || replaying > 0 || !visibleRef.current || !focusedRef.current || !box.current?.contains(document.activeElement)) return true;
      const text = terminalClipboardText(data);
      if (text === undefined) return true;
      try {
        await writeClipboard(text);
        if (!disposed) onStatus("已复制终端选中文本");
      } catch { if (!disposed) onStatus("无法写入本机剪贴板"); }
      return true;
    });
    // A reconnect that finishes quickly is never shown.
    const armNotice = () => {
      if (!noticeTimer) noticeTimer = setTimeout(() => { if (!disposed) setNotice(true); }, terminalNoticeDelayMs);
    };
    const clearNotice = () => {
      clearTimeout(noticeTimer);
      noticeTimer = null;
      setNotice(false);
    };
    const receiveFrame = (event) => {
      stream.lastReceived = Date.now();
      if (typeof event.data !== "string") perf.wrote(event.data.byteLength);
      stream.probe = 0;
      if (typeof event.data === "string") {
        let state;
        try { state = JSON.parse(event.data); } catch { return; }
        if (state.type === "cli-startup") {
          startupPending = state.pending;
          terminal.refresh(0, terminal.rows - 1);
        } else if (state.type === "history") {
          const plan = terminalHistoryPlan(stream.offset, state);
          if (plan.reset) terminal.reset();
          // The modes the kept output was written in, when the server no
          // longer keeps the output that set them.
          if (plan.reset && typeof state.state === "string" && state.state) writeReplay(new TextEncoder().encode(state.state), Infinity);
          stream.offset = plan.offset;
          if (plan.resumable) stream.heartbeats = true;
          replayRemaining = plan.resumable ? plan.size : 0;
          answeredRemaining = plan.resumable ? answeredBytes(plan.offset, plan.size, state.answered) : 0;
          replayFrame = !plan.resumable;
        } else if (state.type === "pong" || state.type === "heartbeat") stream.heartbeats = true;
        else if (state.type === "exit") ended = true;
        return;
      }
      const data = new Uint8Array(event.data);
      if (data.byteLength) outputRef.current?.();
      const replay = replayFrame || replayRemaining > 0;
      replayFrame = false;
      replayRemaining = Math.max(0, replayRemaining - data.byteLength);
      if (stream.offset !== null) stream.offset += data.byteLength;
      if (!replay) return void terminal.write(data);
      const answered = Math.min(answeredRemaining, data.byteLength);
      answeredRemaining -= answered;
      replaying++;
      writeReplay(data, answered, () => { replaying--; });
    };
    // lost handles the end of the current stream once, whether it closed or
    // the watchdog gave up on it, and schedules the next attempt.
    // detach forgets the current stream and its timers and closes it.
    const detach = (current) => {
      ws = null;
      socket.current = null;
      clearInterval(watchdog);
      clearTimeout(probeTimer);
      clearTimeout(openTimer);
      watchdog = probeTimer = openTimer = null;
      current.onopen = current.onmessage = current.onclose = null;
      // A dead path may never finish the closing handshake; do not wait for it.
      try { current.close(); } catch { /* already closed */ }
    };
    const lost = (current) => {
      if (ws !== current) return;
      detach(current);
      if (disposed) return;
      reveal();
      if (ended || closedRef.current) {
        clearNotice();
        setLink("ended");
        return;
      }
      const plan = terminalReconnectPlan(failures, openedAt ? Date.now() - openedAt : 0);
      failures = plan.failures;
      if (!plan.delay) {
        clearNotice();
        setLink("stalled");
        return;
      }
      setLink((state) => (state === "connecting" ? state : "retrying"));
      armNotice();
      retryTimer = setTimeout(connect, plan.delay);
    };
    const keepalive = (probe) => {
      const now = Date.now();
      if (probe) stream.probe = now;
      try {
        ws.send(terminalKeepaliveMessage);
        stream.lastPing = now;
      } catch { /* the close event follows */ }
    };
    const check = () => {
      if (!isOpen()) return;
      const action = terminalLiveness({ ...stream, now: Date.now(), visible: document.visibilityState === "visible" });
      if (action === "dead") lost(ws);
      else if (action !== "idle") keepalive(action === "probe");
    };
    // Only a stream that can resume after its last byte (its server reports
    // offsets) is given up; a server without offsets would replay everything.
    const dormant = () => !visibleRef.current && hiddenSince > 0 && !closedRef.current &&
      stream.offset !== null && Date.now() - hiddenSince >= terminalSuspendMs;
    const suspendStream = () => {
      suspended = true;
      clearTimeout(retryTimer);
      retryTimer = null;
      if (ws) detach(ws);
      clearNotice();
      // A slow resume then shows the quiet reconnect note, as after a drop.
      setLink((state) => (state === "open" ? "retrying" : state));
      perf.stream("suspended");
    };
    // A stalled view (no stream, no retry pending) keeps waiting for its button.
    const suspend = () => {
      if (!suspended && !disposed && !ended && (ws || retryTimer) && dormant()) suspendStream();
    };
    streamVisibility.current = (shown) => {
      clearTimeout(suspendTimer);
      suspendTimer = null;
      if (!shown) {
        if (!hiddenSince) hiddenSince = Date.now();
        suspendTimer = setTimeout(suspend, Math.max(0, hiddenSince + terminalSuspendMs - Date.now()));
        return;
      }
      hiddenSince = 0;
      if (waiting) {
        waiting = false;
        resize();
        return void connect();
      }
      if (!suspended) return;
      suspended = false;
      failures = 0;
      perf.stream("resumed");
      // Directly, not through resume(): a session that ended meanwhile still
      // has its last output and exit to deliver.
      connect();
    };
    const connect = () => {
      clearTimeout(retryTimer);
      retryTimer = null;
      if (disposed || ended || ws || suspended || waiting) return;
      // A retry or a wake-up of a view hidden long enough suspends it instead.
      if (dormant()) return void suspendStream();
      const current = new WebSocket(runtimeSocketURL(terminalStreamPath(session.id, stream.offset, terminalViewer(session.id))));
      current.binaryType = "arraybuffer";
      ws = current;
      socket.current = current;
      openedAt = 0;
      replayRemaining = answeredRemaining = 0;
      replayFrame = false;
      // Each connection proves again that its server sends heartbeats.
      stream.heartbeats = false;
      setLink((state) => (state === "stalled" ? "retrying" : state));
      armNotice();
      openTimer = setTimeout(() => lost(current), terminalConnectTimeoutMs);
      current.onopen = () => {
        if (ws !== current) return;
        clearTimeout(openTimer);
        openTimer = null;
        openedAt = stream.lastReceived = stream.lastPing = Date.now();
        stream.probe = 0;
        perf.stream("opened");
        clearNotice();
        setLink("open");
        watchdog = setInterval(check, terminalWatchdogMs);
        resize();
        if (startupVisible && visibleRef.current && focusedRef.current) terminal.focus();
      };
      current.onmessage = (event) => { if (ws === current) receiveFrame(event); };
      current.onclose = () => lost(current);
    };
    // Back from a hidden page, a sleeping computer or a network change:
    // reconnect now, or confirm that an open but quiet stream still works.
    const resume = (event) => {
      if (disposed || ended || (event?.type === "visibilitychange" && document.visibilityState !== "visible")) return;
      if (!ws) {
        if (closedRef.current) return;
        failures = 0;
        connect();
      } else if (isOpen() && stream.heartbeats && !stream.probe && Date.now() - stream.lastReceived >= terminalKeepaliveMs) {
        keepalive(true);
        clearTimeout(probeTimer);
        probeTimer = setTimeout(check, terminalProbeMs + 100);
      }
    };
    reconnectNow.current = () => {
      failures = 0;
      connect();
    };
    document.addEventListener("visibilitychange", resume);
    window.addEventListener("online", resume);
    const send = (data) => {
      if (disposed || !isOpen()) throw new Error("终端已断开，请重新连接后重试");
      ws.send(JSON.stringify({ type: "input", data }));
    };
    terminal.onData((data) => {
      if (writeReplay.stale(data)) return;
      if (emittingPaste) send(data);
      else void enqueue(() => send(data)).catch(() => {});
    });
    const receive = (content) => {
      // Attach rejection immediately, even while another image is in flight.
      const ready = Promise.resolve(content).then(value => ({ value }), error => ({ error }));
      onStatus("正在传递剪贴板内容…");
      void enqueue(async () => {
        const { value, error } = await ready;
        if (error) throw error;
        if (disposed || !visibleRef.current || !focusedRef.current || !isOpen()) throw new Error("终端已切换或断开，请重新粘贴");
        let text = value.text || "";
        if (value.files || value.fileSelection) {
          const result = await sendFiles(value, "terminal", session.id, { valid: () => !disposed && visibleRef.current && focusedRef.current && isOpen() });
          text = shellFilePaths(result.paths);
        }
        if (value.png) {
          const result = await sendImage(value, "terminal", session.id, { valid: () => !disposed && visibleRef.current && focusedRef.current && isOpen() });
          text = shellFilePaths([result.path]);
        }
        if (disposed || !visibleRef.current || !focusedRef.current) throw new Error("终端已切换，请回到原终端重新粘贴");
        // Honor xterm's bracketed-paste mode without enqueuing behind this job.
        emittingPaste = true;
        try { terminal.paste(text); } finally { emittingPaste = false; }
        onStatus(value.png || value.files || value.fileSelection ? "附件已保存到 Super Cage，路径已插入终端" : "已粘贴");
      }).catch((error) => { if (!disposed) onStatus(error.message); });
    };
    const host = box.current;
    const onPaste = (event) => {
      if (!visibleRef.current || !focusedRef.current) return;
      event.preventDefault(); event.stopImmediatePropagation();
      const files = transferFiles(event.clipboardData);
      if (files.length === 1 && files[0].type.startsWith("image/")) receive(imagePayload(files[0]));
      else if (files.length) receive({ files });
      else if (event.clipboardData?.types.includes("text/plain")) receive({ text: event.clipboardData.getData("text/plain") });
    };
    const onDragOver = (event) => {
      if (!visibleRef.current || !hasFiles(event.dataTransfer)) return;
      event.preventDefault(); event.dataTransfer.dropEffect = "copy";
    };
    const onDrop = (event) => {
      if (!visibleRef.current || !hasFiles(event.dataTransfer)) return;
      event.preventDefault(); event.stopPropagation();
      receive({ files: transferFiles(event.dataTransfer), kind: "drop" });
      terminal.focus();
    };
    host.addEventListener("paste", onPaste, true);
    host.addEventListener("dragover", onDragOver);
    host.addEventListener("drop", onDrop);
    const copySelection = () => {
      const text = terminal.getSelection();
      if (!text) {
        // Claude owns its TUI selection while mouse reporting is enabled. CSI-u
        // preserves Ctrl+Shift+C, which cannot be represented as plain Ctrl+C.
        if (terminal.modes.mouseTrackingMode !== "none")
          void enqueue(() => send("\x1b[99;6u")).catch(() => {});
        onStatus(terminal.modes.mouseTrackingMode === "none"
          ? "请先拖选要复制的终端文字"
          : `当前程序接管了鼠标。请按住 ${isMac ? "Option" : "Shift"} 拖选文字，再复制。`);
        return;
      }
      void writeClipboard(text)
        .then(() => onStatus("已复制终端选中文本"))
        .catch(() => onStatus("无法写入本机剪贴板"));
    };
    const onCopy = (event) => {
      if (!visibleRef.current || !focusedRef.current) return;
      event.preventDefault(); event.stopImmediatePropagation();
      const text = terminal.getSelection();
      if (text) event.clipboardData?.setData("text/plain", text);
      copySelection();
    };
    host.addEventListener("copy", onCopy, true);
    onControls?.({
      edit(action) {
        if (!box.current?.contains(document.activeElement)) return false;
        if (action === "copy") {
          copySelection();
          return true;
        }
        if (action === "paste") {
          receive(readClipboardContent());
          return true;
        }
        if (action === "selectAll") {
          terminal.selectAll();
          return true;
        }
        return false;
      },
    });
    terminal.attachCustomKeyEventHandler((event) => {
      if (event.key === "Enter" && event.shiftKey && !event.metaKey && !event.ctrlKey && !event.altKey &&
          !event.isComposing && event.keyCode !== 229) {
        event.preventDefault();
        if (event.type === "keydown") {
          const shiftMode = agentOptsRef.current.agentConfig?.shift_enter_mode || "bracketed_newline";
          const seq = shiftMode === "plain"
            ? "\n"
            : shiftMode === "csi_u"
              ? "\x1b[13;2u"
              : (terminal.modes.bracketedPasteMode ? "\x1b[200~\n\x1b[201~" : "\x1b[13;2u");
          void enqueue(() => send(seq)).catch(() => {});
        }
        return false;
      }
      if (event.type !== "keydown") return true;
      const tKind = session.terminalKind || session.kind;
      const isAgentTerm = tKind === "claude" || tKind === "agent" || Boolean(session.cli_session_id);
      const imagePasteEnabled = agentOptsRef.current.agentConfig?.image_paste !== false;
      const claudeImagePaste = isAgentTerm && imagePasteEnabled &&
        event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey && event.key.toLowerCase() === "v";
      if (claudeImagePaste && nativeAvailable()) {
        event.preventDefault();
        receive(readClipboardContent());
        return false;
      }
      if (!terminalModifier(event)) return true;
      const key = event.key.toLowerCase();
      if (key === "c") {
        event.preventDefault();
        copySelection();
        return false;
      }
      if (key === "v") {
        if (!nativeAvailable()) return true;
        event.preventDefault();
        receive(readClipboardContent());
        return false;
      }
      if (key === "a") {
        event.preventDefault();
        terminal.selectAll();
        return false;
      }
      if (["+", "=", "-"].includes(key)) {
        event.preventDefault();
        terminal.options.fontSize = Math.max(
          10,
          Math.min(26, terminal.options.fontSize + (key === "-" ? -1 : 1)),
        );
        draw.align();
        resize();
        return false;
      }
      return !terminalModifier(event);
    });
    const observer = new ResizeObserver(resize);
    observer.observe(box.current);
    resize();
    connect();
    return () => {
      disposed = true;
      clearTimeout(startupTimer);
      clearTimeout(suspendTimer);
      streamVisibility.current = null;
      clearTimeout(retryTimer);
      clearTimeout(noticeTimer);
      clearTimeout(probeTimer);
      clearTimeout(openTimer);
      clearInterval(watchdog);
      document.removeEventListener("visibilitychange", resume);
      window.removeEventListener("online", resume);
      reconnectNow.current = null;
      revealStartup.current = null;
      startupRender.dispose();
      clipboardHandler.dispose();
      writeReplay.dispose();
      stopTouch();
      host.removeEventListener("paste", onPaste, true);
      host.removeEventListener("copy", onCopy, true);
      host.removeEventListener("dragover", onDragOver);
      host.removeEventListener("drop", onDrop);
      onControls?.(null);
      observer.disconnect();
      draw.dispose();
      renderer.current = null;
      if (ws) {
        const current = ws;
        ws = null;
        socket.current = null;
        current.onopen = current.onmessage = current.onclose = null;
        current.close();
      }
      terminal.dispose();
      instance.current = null;
      activateLoginRef.current = null;
    };
  }, [session.id]);
  useEffect(() => {
    const terminal = instance.current;
    const activateLogin = activateLoginRef.current;
    if (!terminal || !activateLogin) return;
    const hosts = domainsKey ? domainsKey.split(",") : [];
    const addon = new WebLinksAddon(activateLogin, { urlRegex: agentLoginLinkRegex(hosts) });
    terminal.loadAddon(addon);
    return () => { addon.dispose(); };
  }, [domainsKey, session.id]);
  useEffect(() => {
    if (visible) renderer.current?.show();
    else renderer.current?.hide();
    streamVisibility.current?.(visible);
  }, [visible]);
  useEffect(() => {
    if (visible) {
      fit.current?.fit();
      if (focused && !preparing) instance.current?.focus();
      const ws = socket.current,
        t = instance.current;
      if (ws?.readyState === WebSocket.OPEN && t)
        ws.send(JSON.stringify({ type: "resize", cols: t.cols, rows: t.rows }));
    }
  }, [visible, focused, preparing]);
  useEffect(() => {
    if (!instance.current) return;
    instance.current.options.theme = terminalTheme(theme, variant);
    instance.current.options.fontFamily = theme.fonts.code || systemCodeFont;
    instance.current.options.fontSize = appearance.codeFontSize;
    renderer.current?.align();
    if (visibleRef.current) fit.current?.fit();
  }, [theme, variant, appearance.codeFontSize]);
  const ended = session.closed || link === "ended";
  // A refused Codex launch is not an ordinary close: the note says it did not open and quotes the
  // reason, the last line the launch printed.
  return (
    <div className={`terminal-view ${visible ? "" : "concealed"}`}>
      {session.workspace_notice && <p className="connection-banner" role="status">{session.workspace_notice}</p>}
      {visible && session.cli_session_id && <SessionAccount id={session.cli_session_id} request={terminalAccountRequest} compact
        onChanged={onAccountChanged} />}
      {visible && session.terminalKind === "agent" && session.agent_id && (
        <AgentTerminalBar
          session={session}
          agentSpec={agentSpec}
          onLaunchAgent={onLaunchAgent}
          onOpenAccounts={onOpenAgentAccounts}
          onStatus={onStatus}
        />
      )}
      <div className={`terminal-host ${preparing ? "terminal-preparing" : ""}`} ref={box} aria-hidden={preparing || undefined} />
      {preparing && (
        <div className="terminal-startup" role="status">
          <span>正在准备 Claude 会话…</span>
          <button onClick={() => revealStartup.current?.()}>显示启动过程</button>
        </div>
      )}
      {link !== "open" && (ended || link === "stalled" || notice) && (
        <div className={`terminal-note ${ended || link === "stalled" ? "" : "terminal-note-quiet"}`} role="status">
          {ended
            ? codexLaunchRefused(session)
              ? "未能打开：" + (terminalLastLine(instance.current?.buffer.active) || "原因见终端中的提示")
              : session.herdr_live && onReattachHerdr
                ? "已和 Herdr 断开，终端里的程序仍在运行。"
                : "终端会话已结束。可点击项目右侧的终端图标新建终端。"
            : link === "stalled" ? "终端连接已断开。" : link === "connecting" ? "正在连接终端…" : "正在重新连接…"}
          {ended && session.herdr_live && onReattachHerdr && (
            <button className="primary" onClick={() => onReattachHerdr(session)}>
              <RefreshCw size={13} />
              重新连接
            </button>
          )}
          {!ended && link === "stalled" && (
            <button onClick={() => reconnectNow.current?.()}>
              <RefreshCw size={13} />
              重新连接
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// A terminal whose tab is shown while the server creates it (main.jsx and
// terminal-pending.mjs): a quiet line while the request runs, then the
// server's error in its place, with a retry. The record replaces this view.
export function TerminalPending({ error, onRetry }) {
  return (
    <div className="terminal-view">
      {error ? (
        <div className="terminal-pending-error" role="alert">
          <EmptyState compact title="终端没有打开"
            action={<button className="primary" onClick={onRetry}>重试</button>}>
            {error}
          </EmptyState>
        </div>
      ) : (
        <div className="terminal-startup" role="status">
          <span>正在准备终端…</span>
        </div>
      )}
    </div>
  );
}

export { Modal } from "./modal";
