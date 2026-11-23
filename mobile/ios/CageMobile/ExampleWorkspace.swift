// The example workspace: the phone workbench from the app bundle on its own URL scheme, answered by a script that keeps
// everything in the web view's memory. It has no network, no pairing and no stored credentials, writes nothing to the
// keychain or the saved-devices list, and is gone when the person leaves it or the app restarts. Compiled into every build.
import SwiftUI
import WebKit

enum ExampleWorkspace {
    static let deviceID = "example-workspace"
    static let deviceName = "示例工作台 · 演示数据"
    // The phone UI (remote-browser.jsx) knows this scheme and shows its own sentence instead of asking for a remote screen.
    static let scheme = "cage-preview"

    final class Pages: NSObject, WKURLSchemeHandler, WKNavigationDelegate {
        func webView(_ webView: WKWebView, start urlSchemeTask: WKURLSchemeTask) {
            guard let url = urlSchemeTask.request.url,
                  let root = Bundle.main.url(forResource: "mobile-ui", withExtension: nil) else {return}
            let file = root.appendingPathComponent(url.path == "/" ? "mobile.html" : String(url.path.dropFirst())).standardizedFileURL
            guard file.path.hasPrefix(root.path + "/"), let data = try? Data(contentsOf: file) else {
                urlSchemeTask.didFailWithError(URLError(.fileDoesNotExist)); return
            }
            let types = ["html": "text/html", "js": "text/javascript", "css": "text/css", "png": "image/png"]
            urlSchemeTask.didReceive(URLResponse(url: url, mimeType: types[file.pathExtension] ?? "application/octet-stream",
                                                 expectedContentLength: data.count, textEncodingName: "utf-8"))
            urlSchemeTask.didReceive(data); urlSchemeTask.didFinish()
        }
        func webView(_ webView: WKWebView, stop urlSchemeTask: WKURLSchemeTask) {}
        func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            decisionHandler(action.request.url?.scheme == ExampleWorkspace.scheme ? .allow : .cancel)
        }
    }
    static func makeWebView(model: ConnectionModel, id: String) -> (web: WKWebView, delegate: Pages) {
        let delegate = Pages()
        let origin = URL(string: ExampleWorkspace.scheme + "://app/")!
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        config.applicationNameForUserAgent = "CageMobile/1"
        config.setURLSchemeHandler(delegate, forURLScheme: ExampleWorkspace.scheme)
        config.userContentController.add(DeviceWebBridge(model: model, id: id, origin: origin), name: "cageDevices")
        config.userContentController.addUserScript(WKUserScript(source: fixture, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        let view = WKWebView(frame: .zero, configuration: config)
        view.scrollView.contentInsetAdjustmentBehavior = .never
        view.usePaperBackground()
        view.navigationDelegate = delegate
        view.load(URLRequest(url: origin))
        return (view, delegate)
    }

    // mobile/scripts/example-workspace-check.mjs extracts this script and walks the real phone UI through it; keep the
    // delimiters on their own lines and the closing one indented as it is.
    static let fixture = #"""
    (() => {
      // The example workspace: the phone workbench answered from memory. Nothing here reaches a network, a keychain or a
      // disk, and everything is gone with the page. Names and texts are invented. What the example cannot do answers
      // SORRY with status 422, which no caller in the phone UI treats specially.
      const NAME = "示例工作台 · 演示数据";
      const SORRY = "示例里不能这样做，连接自己的 Super Cage 后可用";
      // The page gives a held read 35 s (wait 25 s plus 10 s slack), so a quiet one is answered at 30 s.
      const HOLD_MS = 30000;
      // Polls that carry no revision (environment, terminals, archive, account) are answered this late when nothing changed.
      const SLOW_MS = 60000;
      const GENERATION = "example";
      const ROOT = "/workspace";
      const encoder = new TextEncoder(), decoder = new TextDecoder();
      const now = () => new Date().toISOString();
      const ago = minutes => new Date(Date.now() - minutes * 60000).toISOString();
      const hex = count => Array.from(crypto.getRandomValues(new Uint8Array(count)), byte => byte.toString(16).padStart(2, "0")).join("");
      const toBase64 = bytes => {
        let text = "";
        for (let i = 0; i < bytes.length; i += 0x2000) text += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x2000));
        return btoa(text);
      };
      const fromBase64 = data => Uint8Array.from(atob(data || ""), c => c.charCodeAt(0));
      // A page on a custom scheme may lack the secure-context calls the phone UI makes.
      if (!crypto.randomUUID) Object.defineProperty(crypto, "randomUUID", {configurable: true, writable: true, value: () => {
        const h = hex(16);
        return h.slice(0, 8) + "-" + h.slice(8, 12) + "-4" + h.slice(13, 16) + "-a" + h.slice(17, 20) + "-" + h.slice(20);
      }});
      if (!crypto.subtle) Object.defineProperty(crypto, "subtle", {configurable: true, value: {digest: async () => new ArrayBuffer(32)}});

      const reply = (value, status = 200) => new Response(JSON.stringify(value), {status, headers: {"Content-Type": "application/json"}});
      const refuse = (message = SORRY, status = 422, headers = {}) =>
        new Response(message, {status, headers: {"Content-Type": "text/plain; charset=utf-8", ...headers}});
      const nothing = () => new Response(null, {status: 204});

      // Waiting. A held request leaves no timer or listener behind when it is answered or aborted.
      const waiters = new Set();
      const wake = () => { for (const check of [...waiters]) check(); };
      const aborted = signal => signal.reason instanceof Error ? signal.reason : new DOMException("已取消", "AbortError");
      const hold = (ms, signal, changed) => new Promise((resolve, reject) => {
        if (signal?.aborted) { reject(aborted(signal)); return; }
        const finish = (result, error) => {
          clearTimeout(timer); waiters.delete(check); signal?.removeEventListener("abort", abort);
          if (error) reject(error); else resolve(result);
        };
        const check = () => { if (changed()) finish(true); };
        const abort = () => finish(false, aborted(signal));
        const timer = setTimeout(() => finish(false), ms);
        waiters.add(check); signal?.addEventListener("abort", abort, {once: true});
      });
      // A page that opens a screen asks once and must be answered at once; the same screen asking again with nothing
      // changed is a poll, answered at SLOW_MS (or earlier, when something changes). A screen is told apart by history.
      let visit = 0;
      for (const method of ["pushState", "replaceState"]) {
        const original = history[method];
        history[method] = function () { visit++; return original.apply(this, arguments); };
      }
      addEventListener("popstate", () => { visit++; });
      const seen = new Map();
      const slow = async (key, revision, signal) => {
        const before = seen.get(key);
        if (before && before.visit === visit && before.revision === revision()) await hold(SLOW_MS, signal, () => revision() !== before.revision);
        seen.set(key, {visit, revision: revision()});
      };

      // Resting. The page's own loops (catalog, environment, session and terminal reads) wake every 30-70 s even when
      // nothing changes. After REST_MS without a touch or a key the example reports itself hidden, like a page that has been
      // put away: the loops stop and nothing runs. The next touch brings it back and the page reads once. Nothing in the
      // example changes without a touch, so nothing is missed. A page the system really hides stays hidden.
      const REST_MS = 30000;
      const system = name => Object.getOwnPropertyDescriptor(Document.prototype, name)?.get;
      if (system("hidden") && system("visibilityState")) {
        let resting = false, timer;
        Object.defineProperty(document, "hidden", {configurable: true, get: () => resting || system("hidden").call(document)});
        Object.defineProperty(document, "visibilityState", {configurable: true, get: () => resting ? "hidden" : system("visibilityState").call(document)});
        const rest = value => {
          if (resting === value) return;
          resting = value;
          document.dispatchEvent(new Event("visibilitychange"));
        };
        const touched = () => { clearTimeout(timer); timer = setTimeout(() => rest(true), REST_MS); rest(false); };
        // "click" alone covers an activation that arrives without a pointer (VoiceOver, a keyboard, a script).
        for (const type of ["pointerdown", "touchstart", "keydown", "wheel", "input", "click"]) addEventListener(type, touched, {capture: true, passive: true});
        touched();
      }

      // Files, shared by the file browser, uploads and the example terminal.
      const files = new Map();
      const parentOf = path => path.slice(0, path.lastIndexOf("/"));
      const ensureDir = (path, minutes) => {
        for (; path.startsWith(ROOT + "/") && !files.has(path); path = parentOf(path)) files.set(path, {type: "dir", mtime: Date.now() / 1000 - minutes * 60});
      };
      const put = (path, text, minutes = 600) => {
        ensureDir(parentOf(path), minutes);
        files.set(path, {type: "file", bytes: typeof text === "string" ? encoder.encode(text) : text, mtime: Date.now() / 1000 - minutes * 60});
      };
      const demo = ROOT + "/demo-site", notes = ROOT + "/notes";
      put(demo + "/README.md", "# 示例网站\n\n这是示例工作台里的演示项目，内容都是虚构的。\n\n## 目录\n\n- `index.html` 首页\n- `styles.css` 样式\n- `src/main.js` 脚本\n", 300);
      put(demo + "/index.html", "<!doctype html>\n<html lang=\"zh-CN\">\n<head>\n  <meta charset=\"utf-8\">\n  <title>示例网站</title>\n  <link rel=\"stylesheet\" href=\"styles.css\">\n</head>\n<body>\n  <h1>你好，示例</h1>\n  <p>这是一个演示用的页面。</p>\n  <script src=\"src/main.js\"></script>\n</body>\n</html>\n", 40);
      put(demo + "/styles.css", "body {\n  margin: 0 auto;\n  max-width: 40rem;\n  padding: 2rem 1rem;\n  line-height: 1.7;\n}\n\nh1 {\n  font-size: 2.25rem;\n}\n", 8);
      put(demo + "/src/main.js", "import { greet } from \"./greet.js\";\n\ndocument.querySelector(\"h1\").textContent = greet(\"示例\");\n", 120);
      put(demo + "/src/greet.js", "export function greet(name) {\n  return \"你好，\" + name;\n}\n", 120);
      put(demo + "/docs/notes.md", "# 备忘\n\n- 标题再大一点\n- 手机上检查一遍排版\n", 1500);
      put(demo + "/assets/logo.svg", "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"120\" height=\"120\" viewBox=\"0 0 120 120\"><rect width=\"120\" height=\"120\" rx=\"24\" fill=\"#f0eee6\"/><path d=\"M30 78c10-30 30-40 60-36\" fill=\"none\" stroke=\"#1f1e1d\" stroke-width=\"8\" stroke-linecap=\"round\"/></svg>\n", 2000);
      put(notes + "/读书计划.md", "# 读书计划\n\n- [x] 第一章\n- [ ] 第二章\n- [ ] 做摘录\n", 200);
      put(notes + "/摘录.md", "# 摘录\n\n> 写下来的东西，才真正属于自己。\n\n（示例文字，出处为虚构。）\n", 3000);
      const entryPath = (project, path) => project + (path ? "/" + path : "");

      // Sessions.
      const projects = [{name: "示例网站", path: demo}, {name: "读书笔记", path: notes}];
      let listRevision = 1;
      const touchList = () => { listRevision++; wake(); };
      const seed = (id, workspace, title, turns, minutes) => ({id, workspace, title, turns, revision: 1, updated_at: ago(minutes)});
      const sessions = [
        seed("example-title", demo, "把首页标题改得更醒目", [{id: "turn-title", prompt: "把首页的标题改大一点，再检查一下手机上的排版。", status: "completed",
          tools: [{id: "tool-read", name: "Read", status: "completed", detail: "index.html"}, {id: "tool-edit", name: "Edit", status: "completed", detail: "styles.css"}],
          text: "已经把首页标题调大，并检查了窄屏下的排版。\n\n- 标题字号调到 2.25rem\n- 窄屏下左右留白收紧了一点\n\n这是示例工作台里预先写好的对话，没有连接真实的 Claude Code。"}], 5),
        seed("example-approval", demo, "运行测试（审批示例）", [{id: "turn-approval", prompt: "运行一下项目的测试。", status: "running", text: "需要你确认这次工具调用。",
          approval: {id: "approval-example", tool: "Bash", input: {command: "npm test", description: "运行示例项目的测试"}}}], 12),
        seed("example-plan", notes, "整理本周读书计划", [{id: "turn-plan", prompt: "帮我把这周的读书计划列成清单。", status: "completed",
          text: "这是本周的清单：\n\n1. 周一：读完第二章\n2. 周三：整理摘录\n3. 周五：写一段小结\n\n示例回复，内容是虚构的。"}], 90),
        {...seed("example-old", demo, "旧的实验记录", [{id: "turn-old", prompt: "记录一下上周的实验。", status: "completed", text: "已记录（示例）。"}], 4300), archived: true},
      ];
      const last = s => s.turns.at(-1);
      const view = s => ({id: s.id, workspace: s.workspace, title: s.title, turns: s.turns, revision: s.revision, generation: GENERATION,
        updated_at: s.updated_at, pinned: s.pinned, custom_title: s.custom_title, metadata_set: s.metadata_set,
        can_send: true, terminal_available: false, active: last(s)?.status === "running"});
      const item = s => { const {turns, ...rest} = view(s); return rest; };
      const touch = s => { s.revision++; s.updated_at = now(); touchList(); };
      const canned = [
        "收到。这是示例工作台里预先写好的回复，没有连接真实的 Claude Code，也不会改动任何文件。\n\n连接自己的 Super Cage 后，这里会显示真实的回复、工具调用和需要你确认的审批。",
        "下面是一段示例代码：\n\n```js\nfunction greet(name) {\n  return \"你好，\" + name;\n}\n```\n\n示例回复，没有真的修改项目。",
        "好的，示例里我会这样安排：\n\n- 先读相关文件\n- 再给出修改建议\n- 需要运行命令时请你确认\n\n这些都是预先写好的内容。",
      ];
      let replies = 0;
      const find = id => sessions.find(s => s.id === id);
      const create = body => {
        const existing = body.id && find(body.id);
        if (existing) return view(existing);
        const s = seed(body.id || crypto.randomUUID(), body.workspace, "新会话", [], 0);
        sessions.unshift(s); touchList();
        return view(s);
      };
      const send = (s, body) => {
        const text = String(body.text || "").trim();
        if (!text) return refuse("消息不能为空。");
        const id = body.request_id || crypto.randomUUID();
        if (!s.turns.some(turn => turn.id === id)) {
          const turn = {id, prompt: text, text: "", status: "running", tools: []};
          s.turns.push(turn);
          if (!s.custom_title && s.turns.length === 1) s.title = text.slice(0, 30);
          touch(s);
          // The canned reply arrives a moment later, like a short answer would.
          s.timer = setTimeout(() => {
            if (turn.status !== "running") return;
            turn.status = "completed";
            turn.tools = [{id: "tool-" + id, name: "Read", status: "completed", detail: "README.md"}];
            turn.text = canned[replies++ % canned.length];
            touch(s);
          }, 1400);
        }
        return reply(view(s));
      };
      const sessionRoute = async (method, id, action, query, body, signal) => {
        const s = find(id);
        if (!s) return refuse("示例会话不存在", 404);
        if (!action) {
          const after = query.get("after");
          if (after !== null && String(s.revision) === after) {
            await hold(HOLD_MS, signal, () => String(s.revision) !== after);
            if (String(s.revision) === after) return nothing();
          }
          return reply(view(s));
        }
        if (method !== "POST") return refuse();
        const turn = last(s);
        switch (action) {
        case "messages": return send(s, body || {});
        case "stop":
          clearTimeout(s.timer);
          if (turn?.status === "running") { turn.status = "stopped"; delete turn.approval; touch(s); }
          return reply(view(s));
        case "approval":
          if (turn?.approval && turn.approval.id === body?.approval_id) {
            delete turn.approval; turn.status = "completed";
            turn.text = body.allow ? "已允许这次调用。示例里不会真的运行命令，下面是预先写好的输出：\n\n```\n12 个测试通过（演示数据）\n```"
              : "已拒绝这次调用，没有运行任何命令。";
            touch(s);
          }
          return reply(view(s));
        case "open": return reply(view(s));
        case "media": {
          // A picture the person attached, or one of the sample files, shown inline in a message.
          const reference = String(body?.reference || "");
          const mime = {png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp"}[reference.split(".").pop().toLowerCase()];
          const entry = reference && !reference.startsWith("/") && !reference.split("/").includes("..") ? files.get(s.workspace + "/" + reference) : null;
          if (entry?.type !== "file" || !mime || !entry.bytes.length) return refuse("未找到媒体文件。", 404);
          return reply({size: entry.bytes.length, mime, version: "example", data: toBase64(entry.bytes.subarray(Number(body.offset) || 0))});
        }
        case "metadata":
          if (body?.title !== undefined) { s.title = s.custom_title = String(body.title); s.metadata_set = true; }
          if (body?.pinned !== undefined) { s.pinned = !!body.pinned; s.metadata_set = true; }
          touch(s);
          return reply(view(s));
        case "archive": clearTimeout(s.timer); s.archived = true; touchList(); return reply({ok: true});
        case "restore": s.archived = false; touchList(); return reply({ok: true});
        case "purge": sessions.splice(sessions.indexOf(s), 1); touchList(); return reply({ok: true});
        case "account": await slow("account", () => 0, signal); return reply({supported: false, candidates: []});
        default: return refuse(SORRY, 422, {"X-Example-Unhandled": "1"});
        }
      };
      const listRoute = async (query, signal) => {
        const after = query.get("after");
        if (after !== null && String(listRevision) === after) {
          await hold(HOLD_MS, signal, () => String(listRevision) !== after);
          if (String(listRevision) === after) return nothing();
        }
        return reply({sessions: sessions.filter(s => !s.archived).map(item), revision: String(listRevision)});
      };

      // Environment: start, stop and restart run as short jobs.
      const nodes = [{id: "node-a", name: "示例节点 A", type: "演示"}, {id: "node-b", name: "示例节点 B", type: "演示"}, {id: "node-c", name: "示例节点 C", type: "演示"}];
      const env = {ready: true, vm: true, job: null, node: "node-b", revision: 1, sequence: 0};
      const touchEnvironment = () => { env.revision++; touchList(); };
      const environmentStatus = () => ({ready: env.ready, vm_running: env.vm, job: env.job, guest_system: "示例系统（演示数据）",
        network: env.ready ? {mode: "open", exit_ip: "203.0.113.8"} : {mode: "off", reason: "环境没有运行"},
        proxy: {selected: env.node, selected_name: nodes.find(node => node.id === env.node).name}});
      const runJob = kind => {
        const id = "job-" + ++env.sequence;
        env.job = {id, kind, running: true, message: kind === "stop" ? "正在停止演示环境…" : "正在启动演示环境…"};
        if (kind !== "start") env.ready = false;
        if (kind === "start") env.vm = true;
        touchEnvironment();
        setTimeout(() => {
          const stopped = kind === "stop";
          env.ready = !stopped; env.vm = !stopped; env.job = {id, kind, running: false};
          touchEnvironment();
        }, 1800);
        return env.job;
      };

      // Terminals: a tiny shell over the example files.
      const terminals = new Map();
      let terminalCount = 0;
      const promptOf = t => "demo@example:" + t.cwd.split("/").pop() + "$ ";
      const print = (t, text) => { t.out += text; wake(); };
      const resolvePath = (t, argument) => {
        const parts = [];
        for (const part of (argument.startsWith("/") ? argument : t.cwd + "/" + argument).split("/")) {
          if (part === "..") parts.pop(); else if (part && part !== ".") parts.push(part);
        }
        return "/" + parts.join("/");
      };
      const children = path => [...files].filter(([key]) => parentOf(key) === path).sort((a, b) => (a[1].type === "dir" ? 0 : 1) - (b[1].type === "dir" ? 0 : 1) || a[0].localeCompare(b[0]));
      const runLine = (t, line) => {
        const [command, ...rest] = line.split(/\s+/), argument = rest.join(" ");
        let text = "";
        if (!command) text = "";
        else if (command === "help") text = "可用命令：help、ls、pwd、cd、cat、echo、clear、exit\r\n其余命令在示例里不会运行。\r\n";
        else if (command === "pwd") text = t.cwd + "\r\n";
        else if (command === "echo") text = argument + "\r\n";
        else if (command === "clear") { print(t, "\x1b[2J\x1b[H" + promptOf(t)); return; }
        else if (command === "ls") {
          const path = argument ? resolvePath(t, argument) : t.cwd;
          text = files.get(path)?.type === "dir" ? children(path).map(([key, entry]) => key.split("/").pop() + (entry.type === "dir" ? "/" : "")).join("  ") + "\r\n"
            : files.has(path) ? path.split("/").pop() + "\r\n" : "ls: " + argument + ": 没有这个文件或目录\r\n";
        } else if (command === "cd") {
          const path = argument ? resolvePath(t, argument) : demo;
          if (files.get(path)?.type === "dir" || path === ROOT) t.cwd = path; else text = "cd: " + argument + ": 没有这个目录\r\n";
        } else if (command === "cat") {
          const entry = files.get(resolvePath(t, argument));
          text = entry?.type === "file" ? decoder.decode(entry.bytes).replace(/\r?\n/g, "\r\n") : "cat: " + argument + ": 没有这个文件\r\n";
        } else if (command === "exit") { t.closed = true; terminalRevision++; print(t, "logout\r\n"); return; }
        else text = command + ": " + SORRY + "\r\n";
        print(t, text + promptOf(t));
      };
      const typeInto = (t, data) => {
        for (let i = 0; i < data.length; i++) {
          const c = data[i];
          if (c === "\x1b") {
            if (data[i + 1] === "[") { i += 2; while (i < data.length && /[0-9;]/.test(data[i])) i++; } else i += data[i + 1] === "O" ? 2 : 1;
          } else if (c === "\r" || c === "\n") {
            const line = t.line.trim(); t.line = ""; print(t, "\r\n"); runLine(t, line);
          } else if (c === "\x7f" || c === "\b") {
            if (t.line) { t.line = t.line.slice(0, -1); print(t, "\b \b"); }
          } else if (c === "\x03") { t.line = ""; print(t, "^C\r\n" + promptOf(t)); }
          else if (c >= " ") { t.line += c; print(t, c); }
        }
      };
      let terminalRevision = 1;
      const newTerminal = (id, workspace, out = "") => {
        const t = {id, title: "终端 " + ++terminalCount, kind: "shell", workspace, cwd: workspace, line: "", out, closed: false};
        t.out = out || "示例终端：这里不会运行真实命令。输入 help 查看可用的几个。\r\n" + promptOf(t);
        terminals.set(id, t); terminalRevision++;
        return t;
      };
      const terminalItem = t => ({id: t.id, title: t.title, kind: t.kind, workspace: t.workspace, closed: t.closed, exit_code: t.closed ? 0 : undefined});
      {
        const t = newTerminal("0a1b2c3d4e5f6071", demo);
        print(t, "ls\r\n" + children(demo).map(([key, entry]) => key.split("/").pop() + (entry.type === "dir" ? "/" : "")).join("  ") + "\r\n" + promptOf(t));
      }
      const terminalRoute = async (method, path, query, body, signal) => {
        if (path === "/terminals") {
          if (method === "POST") return reply(terminalItem(newTerminal(hex(8), body?.workspace || demo)));
          await slow("terminals", () => terminalRevision, signal);
          return reply({kinds: ["shell"], terminals: [...terminals.values()].map(terminalItem)});
        }
        const match = /^\/terminals\/([0-9a-f]{16})\/(output|input|resize|close)$/.exec(path);
        const t = match && terminals.get(match[1]);
        if (!t) return refuse("终端已关闭。", 404);
        if (match[2] === "output") {
          const after = Number(query.get("after")) || 0;
          if (after >= t.out.length && !t.closed) await hold(HOLD_MS, signal, () => t.out.length > after || t.closed);
          if (t.out.length > after) return reply({data: toBase64(encoder.encode(t.out.slice(after))), next: t.out.length, closed: t.closed, exit_code: t.closed ? 0 : undefined});
          return t.closed ? reply({data: "", next: t.out.length, closed: true, exit_code: 0}) : nothing();
        }
        if (match[2] === "input" && !t.closed) typeInto(t, String(body?.data || ""));
        if (match[2] === "close" && !t.closed) { t.closed = true; terminalRevision++; wake(); }
        return reply({ok: true});
      };

      // Files.
      const uploads = new Map();
      const fileRoute = (path, body) => {
        const dir = entryPath(body?.project || "", body?.path ?? body?.dir ?? "");
        switch (path) {
        case "/files/list": {
          if (files.get(dir)?.type !== "dir") return refuse("这个文件夹不存在。");
          return reply({entries: children(dir).map(([key, entry]) => ({name: key.split("/").pop(), type: entry.type, size: entry.bytes?.length || 0, mtime: entry.mtime})), truncated: false});
        }
        case "/files/read": {
          const entry = files.get(entryPath(body?.project || "", body?.path || ""));
          if (entry?.type !== "file") return refuse("这个文件不存在。");
          const offset = Number(body.offset) || 0;
          const mime = /\.svg$/.test(body.path) ? "image/svg+xml" : /\.md$/.test(body.path) ? "text/markdown" : "text/plain";
          return reply({size: entry.bytes.length, mime, version: "example", data: toBase64(entry.bytes.subarray(offset))});
        }
        case "/files/mkdir": {
          const target = entryPath(body?.project || "", body?.path || "");
          if (files.has(target)) return refuse("已有同名文件或文件夹。", 409);
          ensureDir(target, 0);
          return reply({ok: true});
        }
        case "/files/upload/start": {
          const name = String(body?.name || "");
          if (!name || name.includes("/")) return refuse(SORRY);
          if (!(body.size <= 2 << 20)) return refuse("示例里最多上传 2 MB 的文件，连接自己的 Super Cage 后可用更大的。");
          if (files.has(dir + "/" + name) && !body.overwrite) return refuse("已有同名文件。", 409);
          const id = hex(8);
          uploads.set(id, {target: dir + "/" + name, chunks: []});
          return reply({id});
        }
        case "/files/upload/chunk": {
          const upload = uploads.get(body?.id);
          if (!upload) return refuse(SORRY);
          upload.chunks.push(fromBase64(body.data));
          return reply({ok: true});
        }
        case "/files/upload/finish": {
          const upload = uploads.get(body?.id);
          if (!upload) return refuse(SORRY);
          uploads.delete(body.id);
          const size = upload.chunks.reduce((sum, chunk) => sum + chunk.length, 0), bytes = new Uint8Array(size);
          let at = 0;
          for (const chunk of upload.chunks) { bytes.set(chunk, at); at += chunk.length; }
          put(upload.target, bytes, 0);
          return reply({path: upload.target.slice(upload.target.indexOf("/", ROOT.length + 1) + 1), size});
        }
        case "/files/upload/abort": uploads.delete(body?.id); return reply({ok: true});
        default: return refuse(SORRY, 422, {"X-Example-Unhandled": "1"});
        }
      };

      const hostName = () => window.__cageDevices?.devices?.find(device => device.id === window.__cageDevices.current)?.name || NAME;
      const handle = async (method, path, query, body, signal) => {
        let match;
        if (path === "/cli/projects") return reply({host: hostName(), ready: env.ready, shared_sessions: true, projects});
        if (path === "/cli/sessions") {
          if (method === "POST") return reply(create(body || {}));
          if (query.get("archived") === "true") {
            await slow("archived", () => listRevision, signal);
            return reply({sessions: sessions.filter(s => s.archived).map(item)});
          }
          return listRoute(query, signal);
        }
        if ((match = /^\/cli\/sessions\/([^/]+)(?:\/([a-z]+))?$/.exec(path))) return sessionRoute(method, decodeURIComponent(match[1]), match[2], query, body, signal);
        if (path === "/environment") {
          await slow("environment", () => env.revision, signal);
          return reply(environmentStatus());
        }
        if (method === "POST" && (match = /^\/environment\/(start|stop|restart)$/.exec(path))) return reply(runJob(match[1]));
        if (path === "/environment/proxy") return reply({sources: [{id: "source-demo", name: "示例订阅", nodes}]});
        if (path === "/environment/proxy/delay") return reply({delay: 60 + nodes.findIndex(node => node.id === body?.node_id) * 90});
        if (path === "/environment/proxy/select") {
          if (nodes.some(node => node.id === body?.node_id)) { env.node = body.node_id; touchEnvironment(); }
          return reply({ok: true});
        }
        if (path.startsWith("/terminals")) return terminalRoute(method, path, query, body, signal);
        if (path.startsWith("/files/")) return fileRoute(path, body);
        // The remote browser, Claude.ai and Claude Desktop need a paired computer.
        if (path.startsWith("/browser/") || path.startsWith("/desktop/")) return refuse();
        return refuse(SORRY, 422, {"X-Example-Unhandled": "1"});
      };
      window.fetch = (input, options = {}) => {
        const signal = options.signal;
        if (signal?.aborted) return Promise.reject(aborted(signal));
        const url = new URL(typeof input === "string" ? input : input.url, location.href);
        let body = null;
        try { body = options.body ? JSON.parse(options.body) : null; } catch { return Promise.resolve(refuse("请求格式无效。", 422)); }
        const path = url.pathname.startsWith("/api/mobile/") ? url.pathname.slice("/api/mobile".length) : "";
        return handle((options.method || "GET").toUpperCase(), path, url.searchParams, body, signal).catch(error => {
          if (signal?.aborted) throw error;
          return refuse(SORRY, 422, {"X-Example-Unhandled": "1"});
        });
      };
    })();
    """#
}
