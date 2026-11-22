// A stand-in for the routes behind Settings → 设备 (the host's /api/devices* and the joining side's /api/client/devices*,
// design §3.8), for smokes: state the test can change, held reads that answer when it says so, and a log of every request.
// Nothing here reaches a real service. Register it in createWorkbench's beforeNavigate: devicesRoutes(page, host).
import { deflateSync } from "node:zlib";

// A pseudo QR code as a PNG data URL: finder squares and seeded noise on white, with a quiet zone.
export function qrImage(seed = 1, modules = 37, scale = 8) {
  let state = seed * 2654435761 >>> 0;
  const random = () => ((state = (state * 1664525 + 1013904223) >>> 0) / 4294967296);
  const size = (modules + 8) * scale;
  const grid = Array.from({ length: modules }, () => Array.from({ length: modules }, () => random() < 0.5));
  const finder = (x, y) => {
    for (let i = -1; i <= 7; i++) for (let j = -1; j <= 7; j++) {
      const inside = i >= 0 && i <= 6 && j >= 0 && j <= 6;
      if (x + i < 0 || y + j < 0 || x + i >= modules || y + j >= modules) continue;
      grid[y + j][x + i] = inside && (i === 0 || i === 6 || j === 0 || j === 6 || (i >= 2 && i <= 4 && j >= 2 && j <= 4));
    }
  };
  finder(0, 0); finder(modules - 7, 0); finder(0, modules - 7);
  const raw = Buffer.alloc((size * 3 + 1) * size, 255);
  for (let y = 0; y < size; y++) {
    raw[y * (size * 3 + 1)] = 0;
    for (let x = 0; x < size; x++) {
      const mx = Math.floor(x / scale) - 4, my = Math.floor(y / scale) - 4;
      const dark = mx >= 0 && my >= 0 && mx < modules && my < modules && grid[my][mx];
      const at = y * (size * 3 + 1) + 1 + x * 3;
      if (dark) { raw[at] = 31; raw[at + 1] = 30; raw[at + 2] = 29; }
    }
  }
  const crcTable = Array.from({ length: 256 }, (_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
  const crc = (buffer) => { let c = 0xffffffff; for (const byte of buffer) c = crcTable[(c ^ byte) & 255] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
  const chunk = (type, data) => {
    const body = Buffer.concat([Buffer.from(type), data]);
    const out = Buffer.alloc(body.length + 8);
    out.writeUInt32BE(data.length, 0); body.copy(out, 4); out.writeUInt32BE(crc(body), body.length + 4);
    return out;
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(size, 0); header.writeUInt32BE(size, 4); header[8] = 8; header[9] = 2;
  const png = Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", header), chunk("IDAT", deflateSync(raw)), chunk("IEND", Buffer.alloc(0))]);
  return "data:image/png;base64," + png.toString("base64");
}

const wake = (list, value) => { for (const waiter of list.splice(0)) waiter(value); };
const ago = (ms) => new Date(Date.now() - ms).toISOString();
export const sampleDevices = () => [
  { id: "d1", kind: "phone", platform: "ios", name: "iPhone 15", added_at: ago(20 * 86400000), last_seen: ago(40000), connected: true },
  // last_seen is null until the device first connected (the host sends a pointer to the time).
  { id: "d2", kind: "phone", platform: "android", name: "Google Pixel 9 Pro", added_at: ago(9 * 86400000), last_seen: ago(3 * 3600000), connected: false },
  { id: "d3", kind: "computer", platform: "windows", name: "办公室的 Windows 台式机", added_at: ago(2 * 86400000), last_seen: ago(2 * 86400000), connected: false },
];
// The gateway reports "open" or "closed", an empty platform until the host reports one, and no last_open before the first open.
export const sampleComputers = () => [
  { id: "h1", name: "家里的 Mac mini", platform: "darwin", relay: "", added_at: ago(5 * 86400000), last_open: ago(5 * 3600000), state: "closed" },
  { id: "h2", name: "实验室的 Linux 工作站（一台名字特别长的电脑）", platform: "", relay: "", added_at: ago(3 * 86400000), last_open: null, state: "closed" },
];

// options: devices, computers, limit, short (the host has slice 2), keepAwake, relay (the saved custom relay), joinAvailable.
export function createDevicesHost(options = {}) {
  const host = {
    devices: options.devices ?? [], computers: options.computers ?? [], limit: options.limit ?? 5, short: options.short ?? true,
    keepAwake: options.keepAwake ?? { active: true }, joinAvailable: options.joinAvailable ?? true,
    invite: null, mints: 0, joinMode: options.joinMode ?? "approval", joinState: null,
    log: [], inviteWaiters: [], joinWaiters: [], failNext: {}, now: () => Date.now(),
  };
  host.count = (pattern) => host.log.filter((entry) => pattern.test(entry)).length;
  // The test says what the other device did.
  // A held read that is waiting gets the answer at once; otherwise the next read does.
  host.say = (state, extra = {}) => {
    if (host.invite) host.invite = { ...host.invite, state, ...extra, delivered: host.inviteWaiters.length > 0 };
    wake(host.inviteWaiters, { state, ...extra });
  };
  host.joinSay = (state, extra = {}) => { host.joinState = { state, ...extra }; wake(host.joinWaiters, host.joinState); };
  // The list as it is now, answered again until thawed: a page whose list is out of date.
  host.freeze = () => { host.frozen = { devices: structuredClone(host.devices), limit: host.limit, keep_awake: host.keepAwake, invite: null }; };
  host.thaw = () => { host.frozen = null; };
  // The next GET /devices is held until release() and then answers with the list as it was when it was asked.
  host.holdNextList = () => { let release; const gate = new Promise((resolve) => { release = resolve; }); host.heldList = false; host.holding = { gate }; return release; };
  host.reply = (route, status, body, type) => route.fulfill(typeof body === "string" ? { status, contentType: type || "text/plain; charset=utf-8", body } : { status, json: body }).catch(() => {});
  return host;
}

const held = (list) => new Promise((resolve) => { list.push(resolve); });

export async function devicesRoutes(page, host) {
  await page.route("**/api/devices**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname.replace("/api", "");
    const method = request.method();
    const body = method === "POST" && request.postData() ? request.postDataJSON() : undefined;
    host.log.push(method + " " + path);
    const fail = host.failNext[method + " " + path];
    if (fail) { delete host.failNext[method + " " + path]; return host.reply(route, fail.status, fail.body); }
    if (path === "/devices" && method === "GET") {
      if (host.frozen) return host.reply(route, 200, host.frozen);
      // A list read that is answered late, with the list as it was when asked (see holdNextList).
      if (host.holding) {
        const { gate } = host.holding;
        host.holding = null;
        const snapshot = { devices: structuredClone(host.devices), limit: host.limit, keep_awake: host.keepAwake, invite: null };
        host.heldList = true;
        await gate;
        return host.reply(route, 200, snapshot);
      }
      return host.reply(route, 200, { devices: host.devices, limit: host.limit, keep_awake: host.devices.length ? host.keepAwake : { active: false }, invite: host.invite ? { state: host.invite.state, expires: host.invite.expires } : null, ...(host.problem ? { problem: host.problem } : {}) });
    }
    if (path === "/devices/invite" && method === "POST") {
      host.inviteBody = body;
      // As the host checks it: a token goes with the saved custom relay it was typed for.
      if (body?.admin && !body?.relay) return host.reply(route, 400, "请先保存自建中继地址");
      if (host.devices.length >= host.limit) return host.reply(route, 409, "最多添加 " + host.limit + " 台设备。请先移除不用的设备，再添加新的设备。");
      host.mints++;
      const digits = String(48219037 + host.mints * 1111).padStart(8, "0").slice(0, 8);
      host.invite = { state: "waiting", expires: new Date(Date.now() + 107000).toISOString(), manual: "CAGE-FIXTURE-" + host.mints };
      wake(host.inviteWaiters, { state: "expired" });
      return host.reply(route, 200, { image: qrImage(host.mints), manual: host.invite.manual, ...(host.short ? { short: digits } : {}), expires: host.invite.expires });
    }
    if (path === "/devices/invite" && method === "GET") {
      if (!host.invite) return host.reply(route, 200, { state: "none" });
      if (host.invite.state !== "waiting" && !host.invite.delivered) { host.invite.delivered = true; return host.reply(route, 200, { state: host.invite.state, request: host.invite.request, device: host.invite.device }); }
      const answer = await held(host.inviteWaiters);
      return host.reply(route, 200, answer);
    }
    if (path === "/devices/invite" && method === "DELETE") {
      host.invite = null;
      wake(host.inviteWaiters, { state: "none" });
      return host.reply(route, 200, { state: "none" });
    }
    if (path === "/devices/invite/approve" && method === "POST") {
      const device = { id: "d" + (host.devices.length + 4), kind: "computer", platform: "darwin", name: host.invite?.request?.name || "MacBook Air", added_at: new Date().toISOString(), last_seen: new Date().toISOString(), connected: true };
      host.devices.push(device);
      host.say("joined", { device: { id: device.id, name: device.name } });
      return host.reply(route, 204, "");
    }
    if (path === "/devices/invite/reject" && method === "POST") {
      host.say("failed");
      host.invite = null;
      return host.reply(route, 204, "");
    }
    const removal = path.match(/^\/devices\/([^/]+)$/);
    if (removal && method === "DELETE") {
      const id = decodeURIComponent(removal[1]);
      if (!host.devices.some((device) => device.id === id)) return host.reply(route, 404, "这台设备已经移除");
      host.devices = host.devices.filter((device) => device.id !== id);
      return host.reply(route, 200, { removed: id });
    }
    return host.reply(route, 404, "未知的路由");
  });
  await page.route("**/api/client/devices**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace("/api", "");
    const method = request.method();
    const body = method === "POST" && request.postData() ? request.postDataJSON() : undefined;
    host.log.push(method + " " + path + (body?.code ? " " + body.code : ""));
    if (!host.joinAvailable) return host.reply(route, 404, "没有这个功能");
    const fail = host.failNext[method + " " + path];
    if (fail) { delete host.failNext[method + " " + path]; return host.reply(route, fail.status, fail.body); }
    if (path === "/client/devices" && method === "GET") return host.reply(route, 200, { computers: host.computers });
    if (path === "/client/devices/join" && method === "POST") {
      host.joinState = null;
      host.joinBody = body;
      if (host.joinMode === "notfound") return host.reply(route, 404, "未找到");
      if (host.joinMode === "used") return host.reply(route, 409, "已使用");
      // A 409 naming both relays (devices-join contract change 5).
      if (host.joinMode === "relay") return host.reply(route, 409, "这个配对码属于中继 relay.example.com，而这台电脑使用官方中继。请在“设备”里选择同一个中继。");
      // The gateway names the computer as asked, or 「另一台电脑」: the other side does not tell its name.
      const computer = { id: "h3", name: body?.name || "另一台电脑", platform: "", relay: "", added_at: new Date().toISOString(), last_open: null, state: "closed" };
      if (host.joinMode === "approval") { host.joinState = { state: "approval", check: "3817" }; return host.reply(route, 200, host.joinState); }
      host.computers.push(computer);
      return host.reply(route, 200, { id: computer.id, name: computer.name });
    }
    if (path === "/client/devices/join" && method === "GET") {
      const answer = await held(host.joinWaiters);
      if (answer.state === "joined") {
        answer.id ??= "h3";
        answer.name ??= host.joinBody?.name || "另一台电脑";
        if (!host.computers.some((item) => item.id === answer.id)) host.computers.push({ id: answer.id, name: answer.name, platform: "", relay: "", added_at: new Date().toISOString(), last_open: null, state: "closed" });
      }
      return host.reply(route, 200, answer);
    }
    const open = path.match(/^\/client\/devices\/([^/]+)\/(open|close)$/);
    if (open && method === "POST") {
      if (open[2] === "close") return host.reply(route, 204, "");
      return host.reply(route, 200, { connection: "device:" + decodeURIComponent(open[1]) + ":1", url: "/mobile.html" });
    }
    const forget = path.match(/^\/client\/devices\/([^/]+)$/);
    if (forget && method === "DELETE") {
      host.computers = host.computers.filter((item) => item.id !== decodeURIComponent(forget[1]));
      return host.reply(route, 204, "");
    }
    return host.reply(route, 404, "未知的路由");
  });
}
