import React, { useEffect, useReducer, useRef, useState } from "react";
import { Check, Copy, LoaderCircle, RefreshCw } from "lucide-react";
import { api, writeClipboard } from "./api";
import { Modal } from "./modal";
import { requestDeadline, requestDeadlineMs } from "./session-poll.mjs";
import {
  FAILED_TEXT, approvalText, countdownText, customRelayLine, groupDigits, holdReads, initialInvite, initialJoin, inviteReducer, joinReducer,
  joinedText, limitText, NAME_LIMIT, readCodeInput, readJoinName, requestHintParts, requestTitle,
} from "./devices-state.mjs";
import { openComputer } from "./devices-open.mjs";

// A held read: the host (or the gateway) answers when something changes or after 25 s, so nothing ticks here.
const heldRead = (path) => (wait, signal) => {
  const deadline = requestDeadline(signal, requestDeadlineMs(wait));
  return api(path + "?wait=" + wait, undefined, undefined, {}, { signal: deadline.signal }).finally(deadline.done);
};
const decisions = new Set(["joined", "failed", "expired", "none"]);
// A host that cannot answer a held read any more (the code is gone, the route is not there) ends the wait.
const lastRead = (status) => status === 404 || status === 410;

// 添加设备: this computer shows a code and a QR; another device enters or scans it; this computer approves.
export function AddDeviceDialog({ savedRelay, customRelay, admin, onClose }) {
  const [view, dispatch] = useReducer(inviteReducer, initialInvite);
  const [note, setNote] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const phase = useRef(view.phase);
  phase.current = view.phase;
  const minting = useRef(0);

  async function mint(refreshed = false) {
    const mine = ++minting.current;
    dispatch({ type: "restart" });
    setNote("");
    try {
      // As the pairing card always did: the saved relay with the token, so the host can check the two belong together.
      const reply = await api("/devices/invite", { relay: savedRelay, admin });
      if (mine === minting.current) dispatch({ type: "minted", reply, now: Date.now(), refreshed });
    } catch (error) {
      if (mine === minting.current) dispatch({ type: "error", error });
    }
  }
  useEffect(() => {
    void mint();
    return () => {
      minting.current++;
      // The code dies with the dialog (also when the settings page is left meanwhile).
      if (phase.current === "code" || phase.current === "request") api("/devices/invite", undefined, "DELETE", {}, { empty: true }).catch(() => {});
    };
  }, []);

  // One loop per code, through the approval, until the host decides.
  const code = view.phase === "code" || view.phase === "request" ? view.manual : "";
  useEffect(() => {
    if (!code) return undefined;
    const controller = new AbortController();
    void holdReads({
      signal: controller.signal,
      read: heldRead("/devices/invite"),
      onResult: (result) => { dispatch({ type: "poll", result }); return decisions.has(result?.state); },
      onError: (error, failures) => {
        if (lastRead(error.status)) { dispatch({ type: "poll", result: { state: "expired" } }); return true; }
        if (failures < 6) return false;
        dispatch({ type: "error", error });
        return true;
      },
    });
    return () => controller.abort();
  }, [code]);

  // The countdown runs only while a code is on show.
  useEffect(() => {
    if (view.phase !== "code") return undefined;
    const timer = setInterval(() => {
      if (document.hidden) return;
      const time = Date.now();
      setNow(time);
      dispatch({ type: "tick", now: time });
    }, 1000);
    setNow(Date.now());
    return () => clearInterval(timer);
  }, [view.phase, view.manual]);

  async function decide(kind) {
    dispatch({ type: "deciding" });
    try {
      await api("/devices/invite/" + kind, {}, "POST", {}, { empty: true });
      if (kind === "reject") dispatch({ type: "rejected" });
    } catch (error) { dispatch({ type: "error", error }); }
  }
  async function copyLong() {
    try {
      await writeClipboard(view.manual);
      setNote("长配对码已复制，可以粘贴到另一台电脑或手机。");
    } catch { setNote("复制失败，请使用二维码。"); }
  }

  const { request } = view;
  const again = <button type="button" className="primary" onClick={() => mint()}><RefreshCw size={15} aria-hidden="true" />重新生成</button>;
  const close = <button type="button" onClick={onClose}>关闭</button>;
  let footer = close;
  let body;
  if (view.phase === "minting") {
    body = <p className="devices-status" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在生成配对码…</p>;
  } else if (view.phase === "code") {
    body = (
      <div className="devices-code">
        <div className="devices-code-text">
          {view.short && <p className="devices-digits" aria-label={"配对码 " + view.short.split("").join(" ")}>{groupDigits(view.short)}</p>}
          <p className="devices-countdown" role="timer">{countdownText(view.expires, now)}</p>
          <ul className="devices-steps">
            <li><strong>手机</strong>：打开 Super Cage 手机端，点「连接电脑」→「扫码连接」，扫这个二维码。</li>
            <li><strong>电脑</strong>：{view.short
              ? "在那台电脑的 Super Cage 里点「设备」→「加入其他电脑」，输入上面的 8 位数字。"
              : "点「复制长配对码」，在那台电脑的 Super Cage 里点「设备」→「加入其他电脑」，粘贴。"}</li>
          </ul>
          {customRelay && <p className="devices-relay">{customRelayLine(customRelay)}</p>}
          <p className="devices-privacy">只给你自己的设备看这个配对码；关闭窗口后它立即失效。</p>
          {view.refreshed && !note && <p className="devices-status" role="status">已换成新的配对码，之前的已失效。</p>}
          {note && <p className="devices-status" role="status">{note}</p>}
        </div>
        <img className="devices-qr" src={view.image} width="320" height="320" alt="配对二维码" />
      </div>
    );
    footer = (
      <>
        <button type="button" className="devices-copy" onClick={copyLong}><Copy size={15} aria-hidden="true" />复制长配对码</button>
        <button type="button" onClick={() => mint(true)}><RefreshCw size={15} aria-hidden="true" />重新生成</button>
        {close}
      </>
    );
  } else if (view.phase === "request") {
    body = (
      <div className="devices-request" role="group" aria-label="加入请求">
        <h3>{requestTitle(request)}</h3>
        <p>{requestHintParts(request.check)[0]}<strong className="devices-check">{request.check}</strong>{requestHintParts(request.check)[2]}</p>
      </div>
    );
    footer = (
      <>
        <button type="button" disabled={view.deciding} onClick={() => decide("reject")}>拒绝</button>
        <button type="button" className="primary" disabled={view.deciding} onClick={() => decide("approve")}>
          {view.deciding && <LoaderCircle size={15} className="spin" aria-hidden="true" />}允许
        </button>
      </>
    );
  } else if (view.phase === "joined") {
    body = <p className="devices-outcome" role="status"><Check size={18} aria-hidden="true" />已添加 {view.device.name || "新设备"}</p>;
    footer = <button type="button" className="primary" onClick={onClose}>完成</button>;
  } else if (view.phase === "expired") {
    body = <p className="devices-outcome" role="alert">配对码已过期</p>;
    footer = <>{close}{again}</>;
  } else if (view.phase === "failed") {
    body = <p className="devices-outcome" role="alert">{FAILED_TEXT}</p>;
    footer = <>{close}{again}</>;
  } else if (view.phase === "rejected") {
    body = <p className="devices-outcome" role="status">已拒绝{request?.name ? " " + request.name : ""}，这个配对码已失效。</p>;
    footer = <>{close}{again}</>;
  } else if (view.phase === "limit") {
    body = <p className="devices-outcome" role="alert">{limitText(view.limit)}</p>;
  } else {
    body = <p className="devices-outcome" role="alert">{view.message}</p>;
    footer = <>{close}{again}</>;
  }
  return (
    <Modal title="添加设备" wide onClose={onClose} footer={footer}>
      <div className="modal-body devices-dialog" aria-busy={view.phase === "minting"}>{body}</div>
    </Modal>
  );
}

// 加入其他电脑: this computer types the eight digits (or pastes a long code) that another computer shows.
export function JoinDialog({ onClose, onJoined }) {
  const [view, dispatch] = useReducer(joinReducer, initialJoin);
  const [text, setText] = useState("");
  const [label, setLabel] = useState("");
  const parsed = readCodeInput(text);
  const [opening, setOpening] = useState(false);
  const [openError, setOpenError] = useState("");
  const input = useRef(null);
  useEffect(() => { if (view.phase === "input") input.current?.focus(); }, [view.phase]);

  async function submit(event) {
    event?.preventDefault();
    if (!parsed.ready || view.phase !== "input") return;
    dispatch({ type: "submit" });
    const name = label.trim();
    try { dispatch({ type: "reply", reply: await api("/client/devices/join", name ? { code: parsed.code, name } : { code: parsed.code }) }); }
    catch (error) { dispatch({ type: "error", error }); }
  }
  // The other computer's approval: one held read until it answers.
  const waiting = view.phase === "approval";
  useEffect(() => {
    if (!waiting) return undefined;
    const controller = new AbortController();
    void holdReads({
      signal: controller.signal,
      read: heldRead("/client/devices/join"),
      onResult: (reply) => { dispatch({ type: "reply", reply }); return reply?.state !== "approval"; },
      onError: (error, failures) => {
        if (lastRead(error.status) || failures >= 6) { dispatch({ type: "error", error }); return true; }
        return false;
      },
    });
    return () => controller.abort();
  }, [waiting]);
  useEffect(() => { if (view.phase === "joined") onJoined?.(); }, [view.phase]);

  async function open() {
    setOpening(true);
    setOpenError("");
    try { await openComputer(view.computer.id, view.computer.name); }
    catch (error) { setOpenError(error.message); setOpening(false); }
  }
  const edit = (value) => {
    const next = readCodeInput(value);
    setText(next.kind === "short" || next.kind === "empty" ? next.display : value);
    dispatch({ type: "edit" });
  };

  let footer;
  let body;
  if (view.phase === "joined") {
    body = (
      <>
        <p className="devices-outcome" role="status"><Check size={18} aria-hidden="true" />{joinedText(view.computer.name)}</p>
        {openError && <p className="devices-error" role="alert">{openError}</p>}
      </>
    );
    footer = (
      <>
        <button type="button" onClick={onClose}>完成</button>
        <button type="button" className="primary" disabled={opening || !view.computer.id} onClick={open}>
          {opening && <LoaderCircle size={15} className="spin" aria-hidden="true" />}打开
        </button>
      </>
    );
  } else {
    const busy = view.phase !== "input";
    body = (
      <form className="devices-join" onSubmit={submit}>
        <input
          ref={input}
          className={"devices-code-input" + (parsed.kind === "long" || parsed.kind === "invalid" ? " long" : "")}
          aria-label="配对码"
          aria-describedby="devices-join-help"
          autoComplete="off" autoCorrect="off" autoCapitalize="off" spellCheck={false}
          placeholder="0000 0000"
          value={text}
          disabled={busy}
          onChange={(event) => edit(event.target.value)}
        />
        <p id="devices-join-help" className="field-help">在那台电脑的 Super Cage 里点“设备”→“添加设备”，输入显示的 8 位数字。也可以粘贴长配对码。</p>
        <label className="devices-name">
          给它起个名字（可选）
          <input aria-label="电脑名称" placeholder="例如：家里的 Mac mini" maxLength={NAME_LIMIT} autoComplete="off" value={label} disabled={busy}
            onChange={(event) => setLabel(readJoinName(event.target.value))} />
        </label>
        {view.phase === "searching" && <p className="devices-status" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在查找…</p>}
        {view.phase === "approval" && <p className="devices-status devices-waiting" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />{approvalText(view.check)}</p>}
        {view.error && <p className="devices-error" role="alert">{view.error}</p>}
        {parsed.kind === "invalid" && !view.error && <p className="devices-error" role="alert">配对码是 8 位数字，或以 CAGE 开头的长配对码。</p>}
      </form>
    );
    footer = (
      <>
        <button type="button" onClick={onClose}>{busy ? "取消" : "关闭"}</button>
        <button type="button" className="primary" disabled={!parsed.ready || busy} onClick={submit}>加入</button>
      </>
    );
  }
  return (
    <Modal title="加入其他电脑" onClose={onClose} footer={footer}>
      <div className="modal-body devices-dialog">{body}</div>
    </Modal>
  );
}
