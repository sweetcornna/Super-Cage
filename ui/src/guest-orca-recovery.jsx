import React, { useEffect, useRef, useState } from "react";
import { api } from "./api";

const endpoint = "/orca/guest-recovery";
const runtimes = { independent: "客机独立 Orca", managed: "Cage 托管 Orca" };

export default function GuestOrcaRecovery({ ready }) {
  const [open, setOpen] = useState(false);
  const [runtime, setRuntime] = useState("independent");
  const [workspace, setWorkspace] = useState("");
  const [workspaces, setWorkspaces] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const editRevision = useRef(0);

  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    const revision = editRevision.current;
    api(endpoint, undefined, undefined, {}, { signal: controller.signal }).then(
      (data) => {
        if (!controller.signal.aborted && revision === editRevision.current) {
          setWorkspaces(data.workspaces);
          setError("");
        }
      },
      (failure) => {
        if (!controller.signal.aborted && revision === editRevision.current) setError(failure.message);
      },
    );
    return () => controller.abort();
  }, [open]);

  async function update(body) {
    setBusy(true);
    setError("");
    try {
      const data = await api(endpoint, body);
      setWorkspaces(data.workspaces);
    } catch (failure) {
      setError(failure.message);
    } finally {
      editRevision.current++;
      setBusy(false);
    }
  }

  return (
    <details className="orca-advanced" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary>客机 Orca 会话恢复</summary>
      <div className="orca-advanced-body">
        <p className="field-help">
          在原 Orca 中打开 Codex 会话后登记其客机工作区。Cage 会核对原账号和会话，客机重启后在同一个 Orca 工作区继续。
        </p>
        <form className="orca-server-settings" onSubmit={(event) => {
          event.preventDefault();
          void update({ action: "register", runtime, workspace: workspace.trim() });
        }}>
          <label>
            原 Orca 实例
            <select value={runtime} onChange={(event) => setRuntime(event.target.value)}>
              {Object.entries(runtimes).map(([value, label]) => (
                <option key={value} value={value}>{label}</option>
              ))}
            </select>
          </label>
          <label>
            客机工作区绝对路径
            <input type="text" value={workspace} onChange={(event) => setWorkspace(event.target.value)}
              placeholder="/home/cage/projects/my-project" maxLength={1024} required />
          </label>
          <button type="submit" className="primary" disabled={!ready || busy || !workspace.trim()}>
            登记工作区
          </button>
        </form>
        {error && <p role="alert" className="error-message">{error}</p>}
        {workspaces.length > 0 && (
          <ul aria-label="已登记的客机 Orca 工作区">
            {workspaces.map((row) => (
              <li key={JSON.stringify([row.runtime, row.runtime_id, row.workspace])}>
                <strong>{row.workspace}</strong> · {runtimes[row.runtime]} · 已核实 {row.verified} / 已发现 {row.captured}
                {row.restored > 0 && ` · 已恢复 ${row.restored}`}
                {row.closed > 0 && ` · 已关闭 ${row.closed}`}
                {" "}
                <button type="button" disabled={busy} onClick={() => void update({
                  action: "unregister", runtime: row.runtime, runtime_id: row.runtime_id, workspace: row.workspace,
                })}>停止跟踪</button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </details>
  );
}
