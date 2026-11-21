// Pure helpers for browser data management (browser-data.jsx): ranges,
// categories, sizes, result sentences and the day grouping of history.

export const ranges = [
  { id: "hour", label: "最近一小时" },
  { id: "day", label: "过去 24 小时" },
  { id: "week", label: "过去 7 天" },
  { id: "month", label: "过去 4 周" },
  { id: "all", label: "全部时间" },
];

export const categories = [
  { id: "history", title: "浏览记录", detail: "访问过的网址和标题，地址栏也不再推荐它们。" },
  { id: "site", title: "Cookie 和网站数据", detail: "Cookie、localStorage、IndexedDB、缓存存储和 Service Worker。大多数网站会退出登录。" },
  { id: "cache", title: "缓存的图片和文件", detail: "网页再次打开时重新下载，首次加载会慢一些。" },
  { id: "downloads", title: "下载记录", detail: "下载面板和浏览器中的记录。已下载的文件保留在原处；未保存成功的下载连同文件保留，可以重试。" },
];

// The private session lives in memory and has no history: it is cleared as a
// whole, for its site data and cache.
export const privateCategories = ["site", "cache"];

export const sorts = [
  { id: "size", label: "按占用空间" },
  { id: "recent", label: "按最近访问" },
  { id: "name", label: "按名称" },
];

const units = ["B", "KB", "MB", "GB", "TB"];
export function formatBytes(bytes) {
  let value = Math.max(0, Number(bytes) || 0), unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  if (unit === 0) return `${Math.round(value)} B`;
  return `${value >= 100 ? Math.round(value) : value.toFixed(1).replace(/\.0$/, "")} ${units[unit]}`;
}

export const formatCount = count => Number(count || 0).toLocaleString("zh-CN");

// What each category holds, from a summary of the regular session. Sizes are
// for all time; within a range the clear frees at most that much.
export function categoryMeta(id, summary, range = "all") {
  if (!summary) return "正在统计…";
  const ranged = range !== "all";
  if (id === "history") return summary.history ? `${formatCount(summary.history.count)}${summary.history.more ? "+" : ""} 条记录` : "";
  if (id === "site") return summary.sites ? `${formatCount(summary.sites.count)} 个网站 · ${ranged ? "最多 " : ""}${formatBytes(summary.sites.bytes)}` : "";
  if (id === "cache") return summary.cache ? `${ranged ? "最多释放 " : "释放 "}${formatBytes(summary.cache.bytes)}` : "";
  if (id === "downloads") return summary.downloads ? `${formatCount(summary.downloads.count)} 条记录${summary.downloads.unsaved ? ` · ${formatCount(summary.downloads.unsaved)} 个未保存成功的会保留` : ""}` : "";
  return "";
}

const titles = Object.fromEntries(categories.map(item => [item.id, item.title]));
export function resultLine(result) {
  if (!result) return "";
  const cleared = (result.cleared || []).map(id => titles[id]).filter(Boolean);
  const parts = [cleared.length ? `已清除${cleared.join("、")}` : "没有需要清除的内容"];
  if (result.sites) parts[0] = `已清除 ${result.sites} 个网站的数据`;
  if (result.freedBytes > 0) parts.push(`释放约 ${formatBytes(result.freedBytes)}`);
  if (result.reloaded > 0) parts.push(`${result.reloaded} 个网页已重新加载`);
  if (result.kept?.length) parts.push("保持登录的网站和内置页面未受影响");
  if (result.keptDownloads > 0) parts.push(`${formatCount(result.keptDownloads)} 个未保存成功的下载已保留`);
  if (result.skippedOrigins > 0) parts.push(`${formatCount(result.skippedOrigins)} 个无法识别的网站地址已跳过`);
  return parts.join("，") + "。";
}

// The last automatic cleaning, from what the host stored: a failure shows
// even before a first success.
const autoFailures = { failed: "浏览器没有完成清除", timeout: "用时过长", unsupported: "需要更新环境组件" };
export function autoLine(auto, format = time => momentLabel(time)) {
  const failure = auto?.attempted && autoFailures[auto.result];
  if (failure && auto.last) return `上次成功清理：${format(auto.last)}。最近一次（${format(auto.attempted)}）没有完成（${failure}），下次再试。`;
  if (failure) return `自动清理还没有成功过：最近一次（${format(auto.attempted)}）没有完成（${failure}），下次再试。`;
  if (auto?.last) return `上次自动清理：${format(auto.last)}${auto.freed > 0 ? `，释放约 ${formatBytes(auto.freed)}` : ""}。`;
  return "尚未自动清理过。";
}

const dayMs = 86_400_000;
const startOfDay = time => { const day = new Date(time); day.setHours(0, 0, 0, 0); return day.getTime(); };
const weekdays = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
export function dayLabel(time, now = Date.now()) {
  const day = startOfDay(time), today = startOfDay(now);
  const date = new Date(time), text = `${date.getMonth() + 1}月${date.getDate()}日`;
  if (day === today) return `今天 · ${text}`;
  if (day === today - dayMs) return `昨天 · ${text}`;
  if (new Date(time).getFullYear() !== new Date(now).getFullYear()) return `${date.getFullYear()}年${text} ${weekdays[date.getDay()]}`;
  return `${text} ${weekdays[date.getDay()]}`;
}

export const timeLabel = time => new Date(time).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });

// A moment inside a sentence: 今天 14:04, 昨天 09:30, 11月20日 14:04, 2025年11月20日 14:04.
export function momentLabel(time, now = Date.now()) {
  const day = startOfDay(time), today = startOfDay(now), date = new Date(time);
  const year = date.getFullYear() !== new Date(now).getFullYear() ? `${date.getFullYear()}年` : "";
  return `${day === today ? "今天" : day === today - dayMs ? "昨天" : `${year}${date.getMonth() + 1}月${date.getDate()}日`} ${timeLabel(time)}`;
}

// Items arrive newest first; consecutive items of one day share a heading.
export function groupByDay(items, now = Date.now()) {
  const groups = [];
  for (const item of items) {
    const start = startOfDay(item.time);
    let group = groups.at(-1);
    if (!group || group.start !== start) {
      group = { start, end: start + dayMs, label: dayLabel(item.time, now), items: [] };
      groups.push(group);
    }
    group.items.push(item);
  }
  return groups;
}

export function relativeVisit(time, now = Date.now()) {
  if (!time) return "";
  const minutes = Math.round((now - time) / 60_000);
  if (minutes < 1) return "刚刚访问";
  if (minutes < 60) return `${minutes} 分钟前访问`;
  if (minutes < 1440) return `${Math.round(minutes / 60)} 小时前访问`;
  if (minutes < 43200) return `${Math.round(minutes / 1440)} 天前访问`;
  return `${new Date(time).toLocaleDateString("zh-CN")} 访问`;
}

export function hostOf(url) {
  try { const parsed = new URL(url); return /^https?:$/.test(parsed.protocol) ? parsed.hostname : ""; } catch { return ""; }
}

// The same grouping the guest uses for its site list (browser-data.mjs).
const hostingSuffixes = new Set(["github.io", "gitlab.io", "vercel.app", "netlify.app", "pages.dev", "workers.dev", "herokuapp.com",
  "appspot.com", "web.app", "firebaseapp.com", "blogspot.com", "azurewebsites.net", "cloudfront.net", "onrender.com", "fly.dev", "glitch.me"]);
const secondLevels = new Set(["com", "net", "org", "gov", "edu", "ac", "co", "ne", "or", "go", "mil", "gen", "biz", "info", "nom", "ltd", "plc", "sch"]);
export function siteOf(host) {
  host = String(host || "").toLowerCase().replace(/^\./, "").replace(/\.$/, "");
  if (!host || /^\d+(\.\d+){3}$/.test(host) || host.includes(":") || host.startsWith("[")) return host;
  const labels = host.split(".");
  if (labels.length <= 2) return host;
  const last2 = labels.slice(-2).join(".");
  if (hostingSuffixes.has(last2) || (labels.at(-1).length === 2 && secondLevels.has(labels.at(-2)))) return labels.slice(-3).join(".");
  return last2;
}

export const siteOfURL = url => siteOf(hostOf(url));

// What someone types for a kept site becomes a domain, as the server does.
export function keptDomain(value) {
  let text = String(value || "").trim().toLowerCase();
  if (text.includes("://")) { try { text = new URL(text).hostname; } catch { return ""; } }
  text = text.replace(/^\*\./, "").replace(/^\./, "").replace(/:\d+$/, "").replace(/^www\./, "").replace(/\.$/, "");
  if (!/^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(text) || text.length > 253) return "";
  return text.includes(".") || text === "localhost" ? text : "";
}

// Super Cage's own pages live on these: they are kept by default and never
// offered as a site to add.
export const isLoopbackSite = site => ["localhost", "127.0.0.1", "::1", "[::1]"].includes(site);

// What a kept domain covers: Chromium keeps cookies by registrable domain,
// so a kept subdomain keeps its whole site's cookies.
export function keptCoverage(domain) {
  if (/^\d+(\.\d+){3}$/.test(domain)) return "此地址所有端口的 Cookie 和网站存储";
  const site = siteOf(domain);
  return site && site !== domain ? `包含子域名；${site} 的全部 Cookie 也会保留` : "包含子域名的 Cookie 和网站存储";
}

export const keptBy = (site, keep = []) => keep.find(entry => site === entry.domain || site.endsWith("." + entry.domain) || entry.domain.endsWith("." + site));


export const builtInNote = "本机地址的 Cookie 不区分端口：保护文件编辑器和 Claude Science 时，127.0.0.1 和 localhost 上其他预览服务的 Cookie 也会保留，它们的网站存储仍会清除。";
