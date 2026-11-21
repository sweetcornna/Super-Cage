import React, { lazy, Suspense } from "react";
import { createRoot } from "react-dom/client";
import { ArrowLeft, Info, Palette, Settings, SquareTerminal, UserRound } from "lucide-react";
import "../src/desktop-styles.js";
import "../src/settings.css";

// A standalone page for screenshots of Settings → 本机终端应用. It answers the
// desktop's client routes from memory; ?theme=dark, ?state=loading|error|
// unsupported|gone and ?apps=few pick the case.
const query = new URLSearchParams(location.search);
document.documentElement.dataset.theme = query.get("theme") === "dark" ? "dark" : "light";
const state = query.get("state") || "ready";
const table = [
  ["ghostty", "Ghostty", true, true], ["iterm2", "iTerm2", true, true], ["terminal", "终端", false, true],
  ["warp", "Warp", false, true], ["wezterm", "WezTerm", false, true], ["kitty", "kitty", false, true],
  ["alacritty", "Alacritty", false, true], ["herdr", "Herdr", false, false],
];
const installed = query.get("apps") === "few" ? new Set(["terminal", "herdr"]) : new Set(["ghostty", "iterm2", "terminal", "kitty", "herdr"]);
let settings = { app: state === "gone" ? "wezterm" : "ghostty", image_upload: true, clipboard_write: true };
const reply = (body, status = 200) => new Response(typeof body === "string" ? body : JSON.stringify(body), { status, headers: { "Content-Type": typeof body === "string" ? "text/plain; charset=utf-8" : "application/json" } });
const realFetch = window.fetch.bind(window);
window.fetch = async (input, init = {}) => {
  const path = typeof input === "string" ? input : input.url;
  if (!path.startsWith("/api/client/local-terminal")) return realFetch(input, init);
  if (state === "loading") return new Promise(() => {});
  if (state === "error") return reply("本机终端设置无法读取，请重新打开设置", 500);
  await new Promise(resolve => setTimeout(resolve, 120));
  if (path.endsWith("/apps")) {
    return reply({ apps: state === "unsupported" ? [] : table.map(([id, name, automation, hosts]) => ({ id, name, automation, hosts, installed: installed.has(id), ...(hosts ? {} : { note: "Herdr 通过本机终端的 SSH 入口连接，稍后的版本提供" }) })) });
  }
  if (init.method === "PUT") settings = JSON.parse(init.body);
  return reply({ settings, supported: state !== "unsupported" });
};

const LocalTerminalSettings = lazy(() => import("../src/local-terminal-settings.jsx"));
const sections = [["account", "账户", UserRound], ["general", "通用", Settings], ["appearance", "外观", Palette], ["local-terminal", "本机终端应用", SquareTerminal], ["about", "关于", Info]];

function Fixture() {
  return <div className="app">
    <section className="settings-page" aria-label="设置">
      <aside className="settings-sidebar">
        <button className="back-to-workspace"><ArrowLeft size={17} />返回工作台</button>
        <h2>设置</h2>
        <nav aria-label="设置分组">
          {sections.map(([id, label, Icon]) => <button key={id} className={id === "local-terminal" ? "selected" : ""} aria-current={id === "local-terminal" ? "page" : undefined}><Icon size={17} />{label}</button>)}
        </nav>
      </aside>
      <div className="settings-main">
        <div className="settings-content">
          <Suspense fallback={null}><LocalTerminalSettings /></Suspense>
        </div>
      </div>
    </section>
  </div>;
}
createRoot(document.getElementById("root")).render(<Fixture />);
