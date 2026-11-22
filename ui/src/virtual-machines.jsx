import React, { useEffect, useId, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Check, LoaderCircle, Monitor, Plus } from "lucide-react";
import { api } from "./api";
import { getClientContext } from "./client-context.mjs";
import { Modal } from "./modal";
import { EmptyState } from "./drawings";
import "./virtual-machines.css";

const endpoint = "/client/virtual-machines";

export function VirtualMachines({ onClose }) {
  const [inventory, setInventory] = useState(null);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [loadAttempt, setLoadAttempt] = useState(0);
  const nameID = useId();
  const pending = useRef(false);
  const context = getClientContext();
  const connectionID = context.connection_id;
  const portalOrigin = context.tenant_workspace ? context.portal_origin : "";
  const connectionHeaders = () => ({ "X-Cage-Connection": connectionID });

  useEffect(() => {
    const controller = new AbortController();
    api(endpoint, undefined, undefined, { "X-Cage-Connection": connectionID }, { signal: controller.signal })
      .then((result) => { if (!controller.signal.aborted) setInventory(result); })
      .catch((err) => { if (!controller.signal.aborted) setError(err.message); });
    return () => controller.abort();
  }, [connectionID, loadAttempt]);

  const handoff = useRef(null);
  useEffect(() => () => handoff.current?.abort(), []);

  function switchPortalVM(id) {
    if (!portalOrigin || window.parent === window) {
      throw new Error("请从 Super Cage 门户打开工作台后再切换虚拟机。");
    }
    const controller = new AbortController();
    handoff.current = controller;
    return new Promise((resolve, reject) => {
      const fail = () => {
        controller.abort();
        reject(new Error("门户未能打开这台虚拟机，请刷新列表后重试。"));
      };
      const timer = setTimeout(fail, 12000);
      controller.signal.addEventListener("abort", () => clearTimeout(timer), { once: true });
      window.addEventListener("message", (event) => {
        if (event.source !== window.parent || event.origin !== portalOrigin || event.data?.vm_id !== id) return;
        if (event.data.type === "cage:open-virtual-machine-failed") fail();
        if (event.data.type === "cage:open-virtual-machine-accepted") {
          controller.abort();
          resolve();
        }
      }, { signal: controller.signal });
      window.parent.postMessage({ type: "cage:open-virtual-machine", vm_id: id }, portalOrigin);
    });
  }

  async function run(action, operation) {
    if (pending.current) return;
    pending.current = true;
    setBusy(action);
    setError("");
    try {
      await operation();
    } catch (err) {
      setError(err.message);
    } finally {
      pending.current = false;
      setBusy("");
    }
  }

  function create(event) {
    event.preventDefault();
    const value = name.trim();
    if (!value) return;
    if (Array.from(value).length > 64 || /\p{Cc}/u.test(value)) {
      setError("虚拟机名称需要 1–64 个字符，不能包含控制字符。");
      return;
    }
    void run("create", async () => {
      const { vm } = await api(endpoint, { name: value }, "POST", connectionHeaders());
      setInventory((previous) => ({ ...previous, vms: [...previous.vms, vm] }));
      setName("");
      if (portalOrigin && window.parent !== window) {
        window.parent.postMessage({ type: "cage:virtual-machines-changed" }, portalOrigin);
      }
    });
  }

  async function open(id) {
    if (context.tenant_workspace && (!portalOrigin || window.parent === window)) {
      throw new Error("请从 Super Cage 门户打开工作台后再切换虚拟机。");
    }
    const result = await api(`${endpoint}/${encodeURIComponent(id)}/open`, {}, "POST", connectionHeaders());
    if (context.tenant_workspace) {
      if (result.vm_id !== id) throw new Error("门户返回的虚拟机与选择不一致。");
      await switchPortalVM(id);
    } else if (result.current_id !== id) {
      throw new Error("后台返回的虚拟机与选择不一致。");
    }
    onClose();
  }

  return createPortal(<Modal title="虚拟机" onClose={busy ? undefined : onClose}>
    <div className="modal-body project-form vm-dialog">
      {error && <p className="error-message" role="alert">{error}</p>}
      {!inventory && (error
        ? <button type="button" className="vm-retry" onClick={() => { setError(""); setLoadAttempt((value) => value + 1); }}>重新读取</button>
        : <p role="status">正在读取虚拟机…</p>)}
      {inventory && <>
        {inventory.vms.length > 0 ? <fieldset className="project-picker-list" disabled={Boolean(busy)}>
          <legend>我的虚拟机</legend>
          {inventory.vms.map((vm) => {
            const current = vm.id === inventory.current_id;
            return <button type="button" className="ssh-host-row" key={vm.id} aria-current={current || undefined}
              disabled={current} onClick={() => void run(vm.id, () => open(vm.id))}>
              <i className="vm-mark" aria-hidden="true"><Monitor size={16} /></i>
              <span><strong>{vm.name}</strong>{current && <small>当前虚拟机</small>}</span>
              {current && <Check size={15} aria-hidden="true" />}
              {busy === vm.id && <LoaderCircle size={15} className="spin" aria-label="正在切换" />}
            </button>;
          })}
        </fieldset> : <EmptyState compact drawing="server" title="还没有虚拟机">还没有虚拟机。新建一台，为不同的项目分开环境。</EmptyState>}
        <form className="project-form" onSubmit={create}>
          <label htmlFor={nameID}>虚拟机名称</label>
          <div className="folder-input">
            <input id={nameID} value={name} autoComplete="off" maxLength={256} disabled={Boolean(busy)}
              onChange={(event) => setName(event.target.value)} />
            <button className="primary" disabled={Boolean(busy) || !name.trim()}>
              {busy === "create" ? <LoaderCircle size={15} className="spin" /> : <Plus size={15} />}
              创建
            </button>
          </div>
        </form>
      </>}
    </div>
  </Modal>, document.body);
}
