package workbench

import "encoding/json"

// The front's own pages (/_cage/*). They run under the front's own strict
// policy (scienceFront.ownPolicy): scripts from these paths only, no inline
// code, no network but the front.

const scienceFrontCSS = ":root{color-scheme:light dark;--paper:#faf9f5;--ink:#1f1e1d;--muted:#6b655b;--clay:#d97757}" +
	"@media (prefers-color-scheme:dark){:root{--paper:#1f1e1d;--ink:#f3efe5;--muted:#aba597}}" +
	"html,body{height:100%;margin:0}body{display:grid;place-items:center;background:var(--paper);color:var(--ink);" +
	"font:14px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI Variable Text','Segoe UI','Microsoft YaHei UI',sans-serif}" +
	"main{display:grid;justify-items:center;gap:8px;max-width:420px;padding:24px;text-align:center}" +
	"h1{margin:0;font:500 22px/1.3 'New York',ui-serif,Georgia,'Songti SC','Microsoft YaHei UI',serif}" +
	"p{margin:0;color:var(--muted)}.dot{width:10px;height:10px;border-radius:50%;background:var(--clay)}"

func sciencePage(title, status, script string) string {
	return "<!doctype html>\n<html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>Claude Science</title><link rel=\"stylesheet\" href=\"/_cage/front.css\"><script defer src=\"" + script + "\"></script></head>" +
		"<body><main><span class=\"dot\" aria-hidden=\"true\"></span><h1>" + title + "</h1><p id=\"status\" role=\"status\">" + status + "</p></main></body></html>\n"
}

// scienceProbeReportJS reports one context's identity. It is the same code in
// every context so that all of them are measured alike.
const scienceProbeReportJS = "const report = (ctx) => { const hints = navigator.userAgentData; return { ctx, " +
	"tz: Intl.DateTimeFormat().resolvedOptions().timeZone, offset: new Date().getTimezoneOffset(), " +
	"language: navigator.language, languages: [...navigator.languages], ua: navigator.userAgent, platform: navigator.platform, " +
	"ua_platform: hints ? hints.platform : null, shared_worker: typeof SharedWorker !== \"undefined\" }; };\n"

const scienceEnterJS = `"use strict";
(async () => {
  // The label in this origin is the entry; it works once.
  const response = await fetch("/_cage/enter", {
    method: "POST", credentials: "omit", cache: "no-store", redirect: "error",
    headers: { "Content-Type": "application/json" }, body: "{}",
  });
  if (response.status !== 204) throw new Error();
  location.replace("/_cage/probe");
})().catch(() => {
  document.getElementById("status").textContent = "入口已失效，请回到工作台重新打开 Claude Science。";
});
`

func scienceProbeJS() string {
	source, _ := json.Marshal(scienceProbeReportJS + "postMessage(report(\"blob\"));\n")
	return "\"use strict\";\n" + scienceProbeReportJS + `const status = document.getElementById("status");
const reports = [report("main")];
// The shell restricts WebRTC to the (dead) proxy: no candidate may appear.
const rtc = (async () => {
  if (typeof RTCPeerConnection !== "function") return -1;
  const peer = new RTCPeerConnection({ iceServers: [] });
  let count = 0;
  peer.onicecandidate = (event) => { if (event.candidate && event.candidate.candidate) count++; };
  peer.createDataChannel("probe");
  await peer.setLocalDescription(await peer.createOffer());
  await new Promise((resolve) => {
    if (peer.iceGatheringState === "complete") resolve();
    peer.onicegatheringstatechange = () => { if (peer.iceGatheringState === "complete") resolve(); };
    setTimeout(resolve, 2000);
  });
  peer.close();
  return count;
})().catch(() => -2);
let sent = false;
const send = async () => {
  if (sent) return;
  sent = true;
  reports[0].rtc = await rtc;
  try {
    const response = await fetch("/_cage/probe", {
      method: "POST", credentials: "omit", cache: "no-store", redirect: "error",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify({ reports }),
    });
    if (response.status === 204) { location.replace("/"); return; }
    status.textContent = "时区与语言核对未通过，已改用隔离浏览器打开 Claude Science。";
  } catch {
    status.textContent = "时区与语言核对未能完成，请回到工作台重试。";
  }
};
const add = (value) => {
  if (!value || typeof value !== "object" || typeof value.ctx !== "string" || reports.some((item) => item.ctx === value.ctx)) return;
  reports.push(value);
  if (reports.length >= 4) send();
};
const frame = document.createElement("iframe");
addEventListener("message", (event) => {
  if (event.origin === location.origin && event.source === frame.contentWindow && event.data && event.data.cageProbe) add(event.data.cageProbe);
});
const dedicated = new Worker("/_cage/probe-worker.js", { name: "dedicated" });
dedicated.onmessage = (event) => add(event.data);
const blob = new Worker(URL.createObjectURL(new Blob([` + string(source) + `], { type: "text/javascript" })), { name: "blob" });
blob.onmessage = (event) => add(event.data);
frame.hidden = true;
frame.src = "/_cage/probe-frame";
document.body.append(frame);
setTimeout(send, 5000);
`
}

var scienceOwnAssets = map[string]string{
	"/_cage/front.css":       scienceFrontCSS,
	"/_cage/enter":           sciencePage("正在打开 Claude Science", "正在核对入口…", "/_cage/enter.js"),
	"/_cage/enter.js":        scienceEnterJS,
	"/_cage/probe":           sciencePage("正在打开 Claude Science", "正在核对页面的时区与语言…", "/_cage/probe.js"),
	"/_cage/probe.js":        scienceProbeJS(),
	"/_cage/probe-worker.js": "\"use strict\";\n" + scienceProbeReportJS + "postMessage(report(\"dedicated\"));\n",
	"/_cage/probe-frame":     "<!doctype html>\n<html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><script defer src=\"/_cage/probe-frame.js\"></script></head><body></body></html>\n",
	"/_cage/probe-frame.js":  "\"use strict\";\n" + scienceProbeReportJS + "parent.postMessage({ cageProbe: report(\"nested\") }, location.origin);\n",
}
