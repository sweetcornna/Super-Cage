import test from "node:test";
import assert from "node:assert/strict";
import { categoryMeta, dayLabel, formatBytes, groupByDay, keptBy, keptDomain, relativeVisit, resultLine, siteOf, siteOfURL, autoLine, categories, isLoopbackSite, keptCoverage, momentLabel } from "./browser-data-state.mjs";

test("sizes read like a browser's", () => {
  assert.equal(formatBytes(0), "0 B");
  assert.equal(formatBytes(1023), "1023 B");
  assert.equal(formatBytes(1536), "1.5 KB");
  assert.equal(formatBytes(48.6 * 1024 * 1024), "48.6 MB");
  assert.equal(formatBytes(300 * 1024 * 1024), "300 MB");
  assert.equal(formatBytes(2 * 1024 ** 3), "2 GB");
});

test("category lines say how much and, within a range, at most", () => {
  const summary = { history: { count: 12034, more: true }, sites: { count: 38, bytes: 12 * 1024 * 1024 }, cache: { bytes: 120 * 1024 * 1024 }, downloads: { count: 3 } };
  assert.equal(categoryMeta("history", summary, "day"), "12,034+ 条记录");
  assert.equal(categoryMeta("site", summary, "all"), "38 个网站 · 12 MB");
  assert.equal(categoryMeta("site", summary, "hour"), "38 个网站 · 最多 12 MB");
  assert.equal(categoryMeta("cache", summary, "week"), "最多释放 120 MB");
  assert.equal(categoryMeta("downloads", summary), "3 条记录");
  assert.equal(categoryMeta("site", null), "正在统计…");
});

test("the result line names what was cleared and what came back", () => {
  assert.equal(resultLine({ cleared: ["history", "site", "cache"], freedBytes: 5 * 1024 * 1024, reloaded: 2, kept: ["https://github.com"] }),
    "已清除浏览记录、Cookie 和网站数据、缓存的图片和文件，释放约 5 MB，2 个网页已重新加载，保持登录的网站和内置页面未受影响。");
  assert.equal(resultLine({ cleared: ["site"], sites: 3, freedBytes: 0, reloaded: 0 }), "已清除 3 个网站的数据。");
  assert.equal(resultLine({ cleared: [] }), "没有需要清除的内容。");
});

test("history groups by local day, newest first", () => {
  const now = new Date(2026, 10, 21, 15, 0).getTime();
  const items = [
    { url: "https://a.test/", time: new Date(2026, 10, 21, 14, 5).getTime() },
    { url: "https://b.test/", time: new Date(2026, 10, 21, 9, 0).getTime() },
    { url: "https://c.test/", time: new Date(2026, 10, 20, 23, 59).getTime() },
    { url: "https://d.test/", time: new Date(2026, 10, 17, 8, 0).getTime() },
  ];
  const groups = groupByDay(items, now);
  assert.deepEqual(groups.map(group => [group.label, group.items.length]), [["今天 · 11月21日", 2], ["昨天 · 11月20日", 1], ["11月17日 周二", 1]]);
  assert.equal(groups[0].end - groups[0].start, 86_400_000);
  assert.equal(dayLabel(new Date(2025, 0, 2).getTime(), now), "2025年1月2日 周四");
  assert.equal(relativeVisit(now - 3 * 3_600_000, now), "3 小时前访问");
});

test("sites and kept domains", () => {
  assert.equal(siteOf("mail.google.com"), "google.com");
  assert.equal(siteOf("news.bbc.co.uk"), "bbc.co.uk");
  assert.equal(siteOfURL("http://127.0.0.1:18080/?folder=/w"), "127.0.0.1");
  assert.equal(siteOfURL("chrome://settings"), "");
  assert.equal(keptDomain("https://www.GitHub.com/login"), "github.com");
  assert.equal(keptDomain("*.example.org:8443"), "example.org");
  assert.equal(keptDomain("com"), "");
  assert.equal(keptDomain("not a domain"), "");
  assert.ok(keptBy("gist.github.com", [{ domain: "github.com" }]));
  assert.ok(keptBy("github.com", [{ domain: "gist.github.com" }]));
  assert.ok(!keptBy("notgithub.com", [{ domain: "github.com" }]));
});

// Review 3.
test("finding 1: unsaved downloads are counted, kept and reported", () => {
  assert.equal(categoryMeta("downloads", { downloads: { count: 4, unsaved: 2 } }), "4 条记录 · 2 个未保存成功的会保留");
  assert.match(resultLine({ cleared: ["downloads"], keptDownloads: 2 }), /2 个未保存成功的下载已保留/);
  assert.match(categories.find(item => item.id === "downloads").detail, /未保存成功的下载/);
});

test("finding 2: skipped origins are reported; automatic cleaning failures show before a first success", () => {
  assert.match(resultLine({ cleared: ["site"], skippedOrigins: 3 }), /3 个无法识别的网站地址已跳过/);
  const at = Date.UTC(2026, 10, 20, 8, 0), format = () => "某时";
  assert.equal(autoLine({ attempted: at, result: "failed" }, format), "自动清理还没有成功过：最近一次（某时）没有完成（浏览器没有完成清除），下次再试。");
  assert.equal(autoLine({ last: at, attempted: at, result: "timeout" }, format), "上次成功清理：某时。最近一次（某时）没有完成（用时过长），下次再试。");
  assert.equal(autoLine({ last: at, attempted: at, result: "done", freed: 2048 }, format), "上次自动清理：某时，释放约 2 KB。");
  assert.equal(autoLine({}, format), "尚未自动清理过。");
});

test("finding 11: a kept domain says what it covers", () => {
  assert.equal(keptCoverage("github.com"), "包含子域名的 Cookie 和网站存储");
  assert.equal(keptCoverage("gist.github.com"), "包含子域名；github.com 的全部 Cookie 也会保留");
  assert.equal(keptCoverage("192.168.1.10"), "此地址所有端口的 Cookie 和网站存储");
  assert.ok(isLoopbackSite("127.0.0.1") && isLoopbackSite("localhost") && !isLoopbackSite("192.168.1.10"));
});

test("automatic cleaning names its moments the way a person says them", () => {
  const now = new Date(2026, 10, 21, 20, 0).getTime();
  assert.equal(momentLabel(new Date(2026, 10, 21, 14, 4).getTime(), now), "今天 14:04");
  assert.equal(momentLabel(new Date(2026, 10, 20, 9, 30).getTime(), now), "昨天 09:30");
  assert.equal(momentLabel(new Date(2026, 10, 2, 8, 0).getTime(), now), "11月2日 08:00");
  assert.equal(momentLabel(new Date(2025, 11, 31, 23, 59).getTime(), now), "2025年12月31日 23:59");
  assert.match(autoLine({ attempted: new Date(Date.now() - 3_600_000).toISOString(), result: "timeout" }), /^自动清理还没有成功过：最近一次（(今天|昨天) \d\d:\d\d）没有完成（用时过长），下次再试。$/);
});
