import React, { useEffect, useState } from "react";
import { api } from "./api";
import { assertConnection, getClientContext } from "./client-context.mjs";

const appNames = {
  orca: "Orca",
  "deepseek-desktop": "DeepSeek Harness 桌面端",
  "deepseek-tui": "DeepSeek Harness TUI",
};

export async function externalSessionAPI(body, { connections = false, signal } = {}) {
  const context = getClientContext();
  const path = context.desktop_gateway ? "/client/external-session-restore" : "/external-session-restore";
  const connection = context.connection_id || "disconnected";
  const result = await api(path + (connections ? "?connections=1" : ""), body, undefined,
    { "X-Cage-Connection": connection }, { signal });
  assertConnection(context.connection_id);
  return result;
}

export default function ExternalSessionRestore({ workspace }) {
  const [records, setRecords] = useState([]);
  const [connections, setConnections] = useState([]);
  const [application, setApplication] = useState("orca");
  const [path, setPath] = useState(workspace || "");
  const [connection, setConnection] = useState("local");
  const [continuation, setContinuation] = useState("");
  const [profile, setProfile] = useState("tui");
  const [account, setAccount] = useState("");
  const [activeID, setActiveID] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [result, setResult] = useState(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    externalSessionAPI(undefined, { connections: true, signal: controller.signal }).then((data) => {
      setRecords(data.records);
      setConnections(data.orca_connections);
    }, (failure) => {
      if (!controller.signal.aborted) setError(failure.message);
    });
    return () => controller.abort();
  }, []);

  async function update(action, payload) {
    setBusy(true);
    setError("");
    setResult(null);
    try {
      setResult(await externalSessionAPI({ action, ...payload }));
      if (action !== "resume") {
        setRecords((await externalSessionAPI()).records);
      }
    } catch (failure) {
      setError(failure.message);
    } finally {
      setBusy(false);
    }
  }

  function record(event) {
    event.preventDefault();
    const shared = { connection, workspace: path };
    if (application === "orca") {
      void update("watch_orca", shared);
    } else {
      void update("record_deepseek", {
        ...shared, connection: application === "deepseek-desktop" ? "local" : connection,
        application, continuation_id: continuation, profile: application === "deepseek-tui" ? profile : "", account,
      });
    }
  }

  return <section className="settings-card" aria-label="原应用会话恢复">
    <h2>原应用会话恢复</h2>
    <form onSubmit={record}>
      <label>原应用<select value={application} onChange={(event) => {
        setApplication(event.target.value);
        setConnection("local");
      }}>
        {Object.entries(appNames).map(([id, name]) => <option key={id} value={id}>{name}</option>)}
      </select></label>
      {application === "orca" && <label>Orca 连接<select value={connection} onChange={(event) => setConnection(event.target.value)}>
        {connections.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}
      </select></label>}
      {application === "deepseek-tui" && <label>原连接标记<input required value={connection} onChange={(event) => setConnection(event.target.value)} /></label>}
      <label>原应用中的工作区绝对路径<input required value={path} onChange={(event) => setPath(event.target.value)} /></label>
      {application !== "orca" && <>
        <label>原会话 ID<input required value={continuation} onChange={(event) => setContinuation(event.target.value)} /></label>
        <label>原账号标记（不要填写密码或密钥）<input required value={account} onChange={(event) => setAccount(event.target.value)} /></label>
      </>}
      {application === "deepseek-tui" && <label>原 dsh profile<input required value={profile} onChange={(event) => setProfile(event.target.value)} /></label>}
      <button type="submit" disabled={busy || (application === "orca" && connections.length === 0)}>
        {application === "orca" ? "记录此工作区的 Orca 会话" : "登记 DeepSeek 会话"}
      </button>
    </form>
    {records.map((entry) => <article key={entry.id}>
      <strong>{appNames[entry.application]}</strong>
      <div>{entry.workspace}</div>
      <code>{entry.continuation_id}</code>
      {entry.account && <div>账号标记　{entry.account}</div>}
      {entry.application !== "orca" && activeID === entry.id &&
        <label>确认已在原应用选择此账号<input value={confirmation} onChange={(event) => setConfirmation(event.target.value)} /></label>}
      <button type="button" disabled={busy || (activeID === entry.id && entry.account && confirmation.trim() !== entry.account)} onClick={() => {
        if (entry.account && activeID !== entry.id) {
          setActiveID(entry.id);
          setConfirmation("");
        } else {
          void update("resume", { id: entry.id, account: entry.account ? confirmation : "" });
        }
      }}>回到原应用会话</button>
      <button type="button" disabled={busy} onClick={() => void update("close", { id: entry.id })}>不再恢复</button>
    </article>)}
    {error && <p role="alert">{error}</p>}
    {result?.state === "reattached" && <p role="status">已切到 Orca 中原会话的现有窗格。</p>}
    {result?.message && <p role="status">{result.message}</p>}
    {result?.command && <pre>{result.command}</pre>}
    {Number.isInteger(result?.captured) && <p role="status">已记录 {result.captured} 条可识别的 Orca 会话。</p>}
    {result?.closed && <p role="status">已停止恢复该会话。</p>}
  </section>;
}
