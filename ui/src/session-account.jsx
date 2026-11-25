import React, { useEffect, useRef, useState } from "react";
import { InlineSelect } from "./inline-select";
import { answerBody, chipState, confirmText, handoffLine, handoffQuestion, needsConfirm, refusalNeedsConfirm, SESSION_RULES, sessionRule,
  switchBody, switchTargets, targetNote, targetSelectable, undoBody, waitingLine } from "./session-account-state.mjs";
import { useWindowVisibility } from "./window-visibility";
import "./session-account.css";

const failures = {rate_limit:"请求受到限流，尚未确认额度用完：可以等待，或在下方直接选择另一个账号。",authentication_failed:"账号登录失效：在账号管理中重新登录，或在下方选择另一个账号。",account_on_hold:"账号受限：在下方选择另一个账号继续。",overloaded:"服务暂时过载，保留原账号：稍后重试即可。",server_error:"服务暂时异常，保留原账号：稍后重试即可。"};

// Desktop and mobile use the same session operation and durable request ID.
export function SessionAccount({ id, request, onChanged, compact = false }) {
  const windowVisible = useWindowVisibility();
  const [open,setOpen]=useState(false), [view,setView]=useState(null), [busy,setBusy]=useState(false), [error,setError]=useState("");
  const [confirming,setConfirming]=useState(null);
  const pending=useRef(null), current=useRef({request,onChanged}); current.current={request,onChanged};
  const terminalID=useRef(null);
  const path="/sessions/"+id+"/account";
  useEffect(() => {
    if (!windowVisible) return;
    let live=true,timer;
    const load=async()=>{
      try {
        const result=await current.current.request(path,{body:{action:"inspect"}});
        if(live) {
          setView(result);
          const next=result.session?.desktop_terminal_id;
          if(terminalID.current && next && next!==terminalID.current)current.current.onChanged?.(result.session);
          terminalID.current=next;
        }
      } catch(e) { if(live && open) setError(e.message); }
      if(live) timer=setTimeout(load,open?2000:10000);
    };
    void load(); return()=>{live=false;clearTimeout(timer);};
  },[id,path,open,windowVisible]);
  // One durable request per decision: a retry after a lost reply repeats it.
  const send=async(key,make,after)=>{
    if(busy)return; setBusy(true);setError("");
    if(pending.current?.key!==key)pending.current={key,body:make(crypto.randomUUID())};
    const body=pending.current.body;
    try {
      const result=await current.current.request(path,{body}); setView(result);current.current.onChanged?.(result.session);
      pending.current=null; setConfirming(null); after?.();
    } catch(e){
      const fresh=await current.current.request(path,{body:{action:"inspect"}}).catch(()=>null);
      if(fresh)setView(fresh);
      // The server saw work this view did not: ask the one confirm instead.
      if(body.action==="switch" && body.target && !body.force && refusalNeedsConfirm(view,e.message))setConfirming(body.target);
      // Answered on the other device: the refreshed view already shows the outcome.
      else if(!(body.action==="answer" && fresh && !handoffQuestion(fresh)))setError(e.message);
    } finally{setBusy(false);}
  };
  if(!view?.supported)return null;
  const session=view.session, sw=session.account_switch;
  const switching=sw && sw.phase!=="completed";
  const question=handoffQuestion(view), line=handoffLine(session), waiting=waitingLine(view);
  const targets=switchTargets(view);
  const label=(target)=>targets.find(t=>t.id===target)?.label || "目标账号";
  const pick=(target)=>{
    if(needsConfirm(view) && confirming!==target){setConfirming(target);return;}
    const force=confirming===target;
    void send(`switch:${target}:${force}`,requestID=>switchBody(view,target,requestID,{force}),()=>setOpen(false));
  };
  const answer=(choice)=>send("answer:"+choice,requestID=>answerBody(view,choice,requestID));
  return <section className={`session-account ${compact?"session-account--compact":""}`} aria-label="会话账号">
    {question && <div className="session-account-card" role="group" aria-label="额度已用完">
      <p>{question.text}</p>
      <div className="session-account-choices">
        <button type="button" className="session-account-primary" disabled={busy} onClick={()=>answer("once")}>切换并继续</button>
        <button type="button" disabled={busy} onClick={()=>answer("always")}>以后额度用完都自动接续</button>
        <button type="button" disabled={busy} onClick={()=>answer("wait_once")}>等 {question.source} 恢复</button>
        <button type="button" className="text-button" disabled={busy} onClick={()=>answer("wait_always")}>以后都等待</button>
      </div>
      {error && <p role="alert">{error}</p>}
    </div>}
    {!question && (line || waiting) && <p className="session-account-line" role="status">
      <span>{line || waiting}</span>
      {line && view.undo && <button type="button" className="text-button" disabled={busy} onClick={()=>send("undo",requestID=>undoBody(view,requestID,view.source_busy))}>撤销 · 回到 {sw.source_label} 等待</button>}
    </p>}
    <button type="button" className="quiet-button" aria-expanded={open} onClick={()=>setOpen(v=>!v)}>
      {session.account_label || "Claude 账号"} · {chipState(view)}
    </button>
    {open && <div className="session-account-options">
      <p>当前账号：{session.account_label || session.account_id}。会话重连会保持此账号。</p>
      {session.account_failure && !question && !waiting && <p role="status">{failures[session.account_failure] || "本轮发生错误，原会话已保留：可以重试，或选择另一个账号。"}</p>}
      {switching && <p role="status">{sw.phase==="starting"?"正在等待 "+sw.target_label+" 连接。":"正在交接到 "+sw.target_label}{sw.error && ` · ${sw.error}`}</p>}
      {!switching && <div className="session-account-targets" role="group" aria-label="切换到">
        <span>切换到</span>
        {targets.length===0 && <p>账号管理里还没有其他账号。</p>}
        {targets.map(t=><button type="button" key={t.id} className="session-account-target" disabled={busy || !targetSelectable(t)}
          aria-label={`切换到 ${t.label}`} onClick={()=>pick(t.id)}>
          <strong>{t.label}</strong><small>{targetNote(t)}</small>
        </button>)}
      </div>}
      {confirming && !switching && <div className="session-account-confirm" role="group" aria-label="确认切换">
        <p>{confirmText(label(confirming), view.source_tasks)}</p>
        <div className="session-account-choices">
          <button type="button" className="session-account-primary" disabled={busy} onClick={()=>pick(confirming)}>停止并切换</button>
          <button type="button" className="text-button" disabled={busy} onClick={()=>setConfirming(null)}>取消</button>
        </div>
      </div>}
      <label>额度用完时<InlineSelect aria-label="此会话额度用完时" disabled={busy || switching} value={sessionRule(view)} onValueChange={policy=>send("policy:"+policy,()=>({action:"policy",policy}))}>
        {SESSION_RULES.filter(([value])=>value!=="default" || view.handoff_api>=2).map(([value,text])=><option key={value} value={value}>{text}</option>)}
      </InlineSelect></label>
      {switching && sw.phase!=="starting" && <button type="button" disabled={busy} onClick={()=>send("retry:"+sw.id,()=>({action:"switch",request_id:sw.id,source_id:view.source_id,confirm:true}))}>重试账号交接</button>}
      {switching && sw.phase==="preparing" && <button type="button" disabled={busy} onClick={()=>send("cancel:"+sw.id,()=>({action:"cancel",request_id:sw.id}))}>取消交接，保留原账号</button>}
      {error && !question && <p role="alert">{error}</p>}
    </div>}
  </section>;
}
