import assert from "node:assert/strict";
import {mkdir} from "node:fs/promises";
import path from "node:path";
import {fileURLToPath} from "node:url";
import {createWorkbench} from "./workbench-fixture.mjs";

// Account scheduling and handoff, desktop and phone, against an isolated
// fixture server: the first-time question, the line after a handoff, a manual
// switch with its one confirm, and 调度设置.
const out=process.env.CAGE_TEST_CAPTURE || fileURLToPath(new URL("../../docs/test-reports/2026-11-22-account-handoff/",import.meta.url));
await mkdir(out,{recursive:true});
const shot=(page,name)=>page.screenshot({path:path.join(out,name)});
const reset=Date.UTC(2026,10,27,10,0)/1000;
const workspace="/Users/example/project";
const sid="12345678-1234-4321-aaaa-123456789abc";
const scheduling={mode:"balanced",switch_policy:"ask"};
let profiles=[{id:"a",label:"账号 A",email:"a@example.invalid",status:"ready",enabled:true},{id:"b",label:"账号 B",email:"b@example.invalid",status:"ready",enabled:true},
  {id:"c",label:"账号 C",email:"c@example.invalid",status:"ready",enabled:false}];
const labels={a:"账号 A",b:"账号 B",c:"账号 C",d:"账号 D"};
const server={account:"a",question:true,busy:false,tasks:0,switched:null,undo:false,policy:"ask",own:false,elsewhere:false,calls:[]};
const session=()=>({id:sid,workspace,title:"额度验证",terminal_available:true,desktop_terminal_id:"t1",can_send:true,revision:1,turns:[],
  account_id:server.account,account_label:labels[server.account],account_failure:server.question?"rate_limit":"",account_switch:server.switched});
const view=()=>{
  const targets=[{id:"a",label:"账号 A",status:"ready",enabled:true,exhausted:"seven_day",resets_at:reset,remaining:0},{id:"b",label:"账号 B",status:"ready",enabled:true,remaining:62},
    {id:"c",label:"账号 C",status:"ready",enabled:false,remaining:-1},{id:"d",label:"账号 D",status:"signed_out",enabled:true,remaining:-1}].filter(t=>t.id!==server.account);
  return {session:session(),source_id:"12345678-1234-4321-aaaa-123456789abd",switch_policy:server.own?server.policy:scheduling.switch_policy,switch_policy_own:server.own,supported:true,handoff_api:2,
    candidates:targets.filter(t=>t.status==="ready").map(({id,label})=>({id,label})),targets,source_busy:server.busy,source_tasks:server.tasks,undo:server.undo,
    handoff:server.question?{window:"seven_day",resets_at:reset,question:{id:"b",label:"账号 B"},automatic:false}:{automatic:false}};
};
const refuse=(route,body)=>route.fulfill({status:409,contentType:"text/plain; charset=utf-8",body});
const account=(route,body)=>{
  if(body.action!=="inspect")server.calls.push(body);
  if(body.action==="answer"){
    if(server.elsewhere){server.elsewhere=false;server.question=false;server.account="b";
      server.switched={id:"other-device",phase:"completed",reason:"seven_day",resets_at:reset,source_label:"账号 A",target_label:"账号 B"};server.undo=true;
      return refuse(route,"这个问题已在另一台设备上回答，界面已更新。");}
    if(body.answer==="once"||body.answer==="always"){server.question=false;server.account="b";server.undo=true;
      server.switched={id:body.request_id,phase:"completed",reason:"seven_day",resets_at:reset,source_label:"账号 A",target_label:"账号 B"};}
  }
  if(body.action==="switch"){
    if(server.busy&&!body.force)return refuse(route,`当前任务仍在运行：确认“停止当前任务并切换到 ${labels[body.target]}”，或等本轮结束。`);
    server.account=body.target;server.busy=false;server.tasks=0;server.question=false;server.undo=false;
    server.switched={id:body.request_id,phase:"completed",source_label:"账号 A",target_label:labels[body.target]};
  }
  if(body.action==="policy"){server.policy=body.policy;server.own=body.policy!=="default";}
  return route.fulfill({json:view()});
};
const cli=(route,path,body)=>{
  if(path.endsWith("/account"))return account(route,body);
  if(path==="/cli/sessions")return route.fulfill({json:{sessions:[session()],notice:""}});
  return route.fulfill({json:session()});
};

const fixture=await createWorkbench({cliRequest:cli,contextOptions:{timezoneId:"Asia/Shanghai"},beforeNavigate:(page,state)=>{
  Object.assign(state.status,{ready:true,vm_running:true,projects:[workspace]});
  state.terminals=[{id:"t1",kind:"claude",workspace,title:"额度验证",cli_session_id:sid}];
}});
const {browser,page,state,errors}=fixture;
try{
  // Desktop: the question is a quiet card in the session.
  await page.getByRole("tab",{name:"额度验证",exact:true}).click();
  const card=page.getByRole("group",{name:"额度已用完"});
  await card.waitFor();
  assert.match(await card.innerText(),/账号 A 的额度已用完（周限额，11\/27 18:00 重置）。要换到 账号 B 继续吗？会话历史会带到 账号 B。/);
  assert.equal(await page.getByRole("dialog").count(),0,"no modal");
  await shot(page,"handoff-question-desktop.png");
  await card.getByRole("button",{name:"切换并继续",exact:true}).click();
  const line=page.getByRole("status").filter({hasText:"已换到 账号 B 继续"});
  await line.waitFor();
  assert.equal(await card.count(),0);
  assert.deepEqual(server.calls.at(-1).answer,"once");
  assert.equal(await line.getByRole("button",{name:/撤销/}).count(),1);
  await shot(page,"handoff-line-desktop.png");

  // A manual switch while the source works: one confirm, then force. The
  // confirm names the background tasks that stop with it.
  server.busy=true;server.tasks=1;server.undo=false;server.switched=null;server.account="a";
  await page.getByRole("button",{name:/^账号 B · /}).click();
  server.calls.length=0;
  // The open menu's poll (2 s) shows A again; its targets follow.
  await page.getByRole("button",{name:/^账号 A · /}).waitFor();
  await page.getByRole("button",{name:"切换到 账号 B",exact:true}).waitFor();
  const disabled=page.getByRole("button",{name:"切换到 账号 C",exact:true});
  assert.match(await disabled.innerText(),/未参与自动分配/);
  assert.equal(await page.getByRole("button",{name:"切换到 账号 D",exact:true}).isDisabled(),true,"an account that is not logged in is not a target");
  await disabled.click();
  const confirm=page.getByRole("group",{name:"确认切换"});
  await confirm.waitFor();
  assert.match(await confirm.innerText(),/会停止当前任务（包括 1 个后台任务）并切换到 账号 C，继续吗？/);
  assert.equal(server.calls.length,0,"nothing is sent before the confirm");
  await shot(page,"manual-confirm-desktop.png");
  await confirm.getByRole("button",{name:"停止并切换",exact:true}).click();
  await page.getByRole("button",{name:/^账号 C · /}).waitFor();
  assert.equal(server.calls.length,1);assert.equal(server.calls[0].force,true);assert.equal(server.calls[0].target,"c");

  // 调度设置: the question is offered once two accounts take part.
  await page.route("**/api/claude-accounts*",async route=>{
    const body=route.request().postDataJSON();
    if(body?.action==="scheduling"){Object.assign(scheduling,body.scheduling);if(body.order)profiles=body.order.map(id=>profiles.find(a=>a.id===id));}
    if(body?.action==="update")Object.assign(profiles.find(a=>a.id===body.id),body);
    if(body?.action==="prefer")scheduling.preferred=body.id; // 参与分配 stays as it was
    return route.fulfill({json:{accounts:profiles,scheduling,detection_status:"offline",handoff_api:2}});
  });
  await page.getByRole("button",{name:/^账号管理(?:\s*\d+)?$/}).click();
  await page.getByText("调度设置 · 均衡分配 · 额度用完时待设定",{exact:true}).click();
  const offer=page.getByRole("group",{name:"额度用完时"});
  await offer.waitFor();
  await shot(page,"scheduling-offer-desktop.png");
  const select=page.getByRole("combobox",{name:"新会话分配",exact:true});
  const policy=page.getByRole("combobox",{name:"额度用完时",exact:true});
  const before=(await policy.boundingBox()).y;
  await select.click();
  assert.ok((await policy.boundingBox()).y>before+20,"options must occupy layout space");
  await select.press("End");await select.press("Enter");
  assert.equal(scheduling.mode,"sequential");
  await page.getByRole("button",{name:"提高 账号 B 的优先级"}).click();
  await page.waitForFunction(()=>document.querySelector('.claude-account-order')?.textContent.startsWith('1. 账号 B'));
  assert.equal(profiles[0].id,"b");
  await offer.getByRole("button",{name:"以后额度用完都自动接续",exact:true}).click();
  await offer.waitFor({state:"detached"});
  assert.equal(scheduling.switch_policy,"allow");assert.equal(scheduling.ungrouped_handoff,true);assert.equal(scheduling.switch_answered,true);
  await page.getByText(/没有设分组的账号之间也会互相接续/).waitFor();
  // The ungrouped yes is its own control: off, the rule says they still ask.
  const ungrouped=page.getByRole("checkbox",{name:"没有设接续分组的账号之间也自动接续",exact:true});
  assert.equal(await ungrouped.isChecked(),true);
  await ungrouped.click(); // saved first, then shown: no optimistic state
  await page.getByText(/没有设分组的账号之间仍会先问你/).waitFor();
  assert.equal(await ungrouped.isChecked(),false);
  assert.equal(scheduling.ungrouped_handoff,false);assert.equal(scheduling.switch_policy,"allow");
  await ungrouped.click();
  await page.getByText(/没有设分组的账号之间也会互相接续/).waitFor();
  assert.equal(scheduling.ungrouped_handoff,true);
  await page.getByText(/手动切换不受这些限制/).first().waitFor();
  await page.getByRole("article",{name:"账号 C",exact:true}).getByRole("button",{name:"新会话使用此账号",exact:true}).click();
  await page.getByRole("article",{name:"账号 C",exact:true}).getByText("新会话优先",{exact:true}).waitFor();
  assert.equal(scheduling.preferred,"c");assert.equal(profiles.find(a=>a.id==="c").enabled,false,"preferring does not turn on 参与分配");
  await page.getByRole("article",{name:"账号 C",exact:true}).getByText("仅新会话使用",{exact:true}).waitFor();
  await page.getByRole("article",{name:"账号 A",exact:true}).getByRole("button",{name:"设置接续分组"}).click();
  await page.getByRole("textbox",{name:"账号 A 的接续分组"}).fill("本人账号");
  await page.getByRole("article",{name:"账号 A",exact:true}).getByRole("button",{name:"保存分组"}).click();
  assert.equal(profiles.find(a=>a.id==="a").handoff_group,"本人账号");
  await shot(page,"scheduling-rule-desktop.png");
  assert.deepEqual(errors,[]);

  // Phone, 390x844: the same card; an answer given on the other device wins.
  Object.assign(server,{account:"a",question:true,busy:false,switched:null,undo:false,elsewhere:true});server.calls.length=0;
  const phoneContext=await browser.newContext({viewport:{width:390,height:844},isMobile:true,hasTouch:true,userAgent:"CageMobile/1",timezoneId:"Asia/Shanghai"});
  const phone=await phoneContext.newPage();
  const phoneErrors=[];phone.on("pageerror",e=>phoneErrors.push(e.message));
  await phone.route("**/api/**",async route=>{
    const p=new URL(route.request().url()).pathname.replace("/api/mobile/cli","/cli");const body=route.request().postDataJSON();
    if(p.endsWith("/projects"))return route.fulfill({json:{host:"测试电脑",ready:true,shared_sessions:true,projects:[{path:workspace,name:"project"}]}});
    return cli(route,p,body);
  });
  await phone.goto((process.env.CAGE_UI_URL || "http://127.0.0.1:5198")+"/mobile.html");
  await phone.getByRole("button",{name:"展开侧栏",exact:true}).click();
  const drawer=phone.getByRole("dialog",{name:"工作区侧栏"});
  await drawer.waitFor();
  const folder=drawer.getByRole("button",{name:"项目 project",exact:true});
  if(await folder.getAttribute("aria-expanded")==="false")await folder.click();
  await drawer.getByRole("list",{name:"project的会话"}).getByRole("button",{name:"额度验证",exact:true}).click();
  const phoneCard=phone.getByRole("group",{name:"额度已用完"});
  await phoneCard.waitFor();
  await drawer.waitFor({state:"hidden"});
  await phone.waitForTimeout(400); // the drawer's closing motion
  await phone.evaluate(()=>document.activeElement?.blur()); // focus back on the menu button shows its tooltip
  assert.ok(await phone.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),"the card fits the phone");
  await shot(phone,"handoff-question-phone.png");
  await phoneCard.getByRole("button",{name:"切换并继续",exact:true}).click();
  await phone.getByRole("status").filter({hasText:"已换到 账号 B 继续"}).waitFor();
  assert.equal(await phoneCard.count(),0,"the answer from the other device closed the question");
  assert.equal(await phone.getByRole("alert").count(),0,"an answer given elsewhere is no error here");
  await shot(phone,"handoff-answered-elsewhere-phone.png");

  // Phone manual switch: picking a target switches, no checkbox, no group.
  server.calls.length=0;
  await phone.getByRole("button",{name:/^账号 B · /}).click();
  const back=phone.getByRole("button",{name:"切换到 账号 A",exact:true});
  await back.waitFor();
  assert.match(await back.innerText(),/额度已用完，11\/27 18:00 重置/);
  assert.equal(await phone.getByRole("checkbox").count(),0,"no consent checkbox");
  assert.ok(await phone.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await shot(phone,"manual-targets-phone.png");
  await phone.getByRole("button",{name:"切换到 账号 C",exact:true}).click();
  await phone.getByRole("button",{name:/^账号 C · /}).waitFor();
  assert.equal(server.calls.length,1);assert.equal(server.calls[0].action,"switch");assert.equal(server.calls[0].confirm,true);
  assert.equal(server.calls[0].force,undefined,"an idle source needs no confirm");
  assert.deepEqual(errors,[]);assert.deepEqual(phoneErrors,[]);
  console.log("Account handoff: question card, handoff line with undo, manual switch with one confirm naming background tasks, 调度设置 offer, rule and the ungrouped yes, preferred account outside scheduling, phone answer from the other device and phone manual switch passed.");
}finally{await browser.close();}
