// The phone page against a host that announces held JPEG frame reads. Build the
// phone UI first with node mobile/build-ui.mjs; set CAGE_PLAYWRIGHT_MODULE and
// CAGE_TEST_MOBILE_BROWSER (default chromium) as for the other phone smokes.
// Everything the host does is a fixture: one 12x16 JPEG, answered the way the
// paired host answers a read after a tag, with and without the capability.
import assert from "node:assert/strict";
import http from "node:http";
import {readFile} from "node:fs/promises";
import {fileURLToPath} from "node:url";
const playwright = await import(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const engine = process.env.CAGE_TEST_MOBILE_BROWSER || "chromium";
const resources = new URL("../../mobile/.build/resources/mobile-ui/", import.meta.url);
const httpd = http.createServer(async (req, res) => {
  const path = new URL(req.url, "http://localhost").pathname;
  try {
    if (path !== "/" && path !== "/mobile.html" && !/^\/assets\/[\w.-]+$/.test(path)) throw Error();
    const file = new URL(path.slice(1) || "mobile.html", resources);
    res.setHeader("Content-Type", {js:"text/javascript", html:"text/html", png:"image/png", css:"text/css"}[file.pathname.split(".").pop()]);
    res.end(await readFile(fileURLToPath(file)));
  } catch {res.writeHead(404).end();}
});
await new Promise(resolve => httpd.listen(0, "127.0.0.1", resolve));

async function session(browser, {announce}) {
  const context = await browser.newContext({viewport:{width:390, height:844}, hasTouch:true, isMobile:true, deviceScaleFactor:1, userAgent:"CageMobile/1"});
  const page = await context.newPage();
  page.setDefaultTimeout(8000);
  const image = await page.evaluate(() => {
    const canvas = document.createElement("canvas");
    canvas.width = 12; canvas.height = 16;
    const c = canvas.getContext("2d"); c.fillStyle = "#555"; c.fillRect(0, 0, 12, 16);
    return canvas.toDataURL("image/jpeg").split(",")[1];
  });
  const picture = Buffer.from(image, "base64");
  let frame, version = 1, wake;
  const reads = [], actions = [], held = [];
  const tag = () => version.toString(16).padStart(32, "0");
  const meta = () => ({...frame, frame_tag:tag(), action_frame:true});
  await page.route("**/api/mobile/**", async route => {
    const request = route.request(), url = new URL(request.url()), body = request.postDataJSON();
    const reply = (value, headers = {}) => route.fulfill({json:value, headers});
    const path = url.pathname;
    if (path === "/api/mobile/cli/projects") return reply({host:"测试电脑", ready:true, projects:[{path:"/work/cage", name:"Cage"}]});
    if (path === "/api/mobile/cli/sessions") return reply({sessions:[]});
    if (path === "/api/mobile/terminals") return reply({terminals:[]});
    if (!path.startsWith("/api/mobile/browser/")) return route.fulfill({status:404});
    if (path.endsWith("/open")) {
      frame = {...body, document:"doc-1", focus:"", password:false, url:"https://example.test/文件", title:"标题：远程画面"};
      return reply({id:body.id, action_frame:true});
    }
    if (path.endsWith("/close")) return reply({id:frame.id});
    if (path.endsWith("/action")) {
      actions.push({...body, at:Date.now()});
      version++; wake?.();
      return reply({...meta(), image});
    }
    assert.ok(path.endsWith("/frame"));
    const after = url.searchParams.get("after"), headers = announce ? {"X-Cage-Frame-Stream":"1"} : {};
    reads.push({query:url.search, at:Date.now()});
    if (announce && url.searchParams.get("fmt") === "jpeg") {
      if (after === tag() && url.searchParams.get("wait")) {
        held.push(Date.now());
        // Held until the picture changes, the page gives the read up, or the bound passes.
        await new Promise(resolve => {wake = resolve; setTimeout(resolve, 3000);});
      }
      try {
        if (after === tag()) return await reply({id:frame.id, frame_tag:after, unchanged:true}, headers);
        return await route.fulfill({status:200, body:picture, headers:{...headers, "Content-Type":"image/jpeg",
          "X-Cage-Frame":Buffer.from(JSON.stringify({...meta(), frame_seq:reads.length})).toString("base64url")}});
      } catch { return; } // The page gave this read up.
    }
    if (after) return reply({id:frame.id, frame_tag:after, unchanged:true}, headers);
    return reply({...meta(), image}, headers);
  });
  await page.goto("http://127.0.0.1:" + httpd.address().port);
  await page.getByRole("button", {name:"浏览器", exact:true}).click();
  const view = page.getByRole("img", {name:"远程浏览器画面"});
  await view.waitFor();
  return {context, page, view, reads, actions, held, bump:() => {version++; wake?.();}};
}

let browser;
try {
  browser = await playwright[engine].launch();
  // A host that announces held reads: JPEG bodies, one read held at a time, input never behind it.
  {
    const s = await session(browser, {announce:true});
    for (let i = 0; i < 50 && !s.held.length; i++) await s.page.waitForTimeout(100);
    assert.ok(s.reads[0].query === "", "the first read is the original one: " + s.reads[0].query);
    assert.match(s.reads[1].query, /^\?after=[0-9a-f]{32}&wait=12000&fmt=jpeg$/);
    assert.ok(s.held.length, "no read was held");
    assert.ok((await s.view.getAttribute("src")).startsWith("blob:"), "the picture should come from an object URL");
    const count = s.reads.length;
    await s.page.waitForTimeout(800);
    assert.equal(s.reads.length, count, "a held read is not asked again");
    const box = await s.view.boundingBox();
    const before = Date.now();
    const sent = s.page.waitForRequest(request => request.url().endsWith("/action") && request.postDataJSON()?.type === "tap");
    await s.page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
    await sent;
    assert.ok(Date.now() - before < 700, "a tap waited behind the held read");
    for (let i = 0; i < 40 && s.reads.length === count; i++) await s.page.waitForTimeout(100);
    assert.ok(s.reads.length > count, "no read followed the tap");
    const shown = await s.view.getAttribute("src");
    s.bump();
    await s.page.waitForFunction(old => document.querySelector(".remote-screen")?.src !== old, shown);
    // A hidden page holds no read open and starts none.
    await s.page.evaluate(() => {Object.defineProperty(document, "hidden", {value:true, configurable:true}); document.dispatchEvent(new Event("visibilitychange"));});
    await s.page.waitForTimeout(200);
    const hidden = s.reads.length;
    s.bump();
    await s.page.waitForTimeout(900);
    assert.equal(s.reads.length, hidden, "a hidden page kept reading");
    await s.page.evaluate(() => {Object.defineProperty(document, "hidden", {value:false, configurable:true}); document.dispatchEvent(new Event("visibilitychange"));});
    for (let i = 0; i < 40 && s.reads.length === hidden; i++) await s.page.waitForTimeout(100);
    assert.ok(s.reads.length > hidden, "a shown page did not start over");
    await s.context.close();
  }
  // A host that does not announce them keeps the original polling.
  {
    const s = await session(browser, {announce:false});
    for (let i = 0; i < 50 && s.reads.length < 3; i++) await s.page.waitForTimeout(100);
    assert.ok(s.reads.length >= 3 && s.reads.every(read => !/fmt=|wait=/.test(read.query)), "an older host was sent a negotiated read");
    assert.ok((await s.view.getAttribute("src")).startsWith("blob:"));
    await s.context.close();
  }
  console.log(engine + ": held JPEG reads, input beside a held read, hidden page and older host passed");
} finally {
  await browser?.close();
  await new Promise(resolve => httpd.close(resolve));
}
