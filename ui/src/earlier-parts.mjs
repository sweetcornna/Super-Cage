// What /clear left behind in a session (清除前的对话): the server keeps each
// earlier conversation as a read-only part of the same entry.
export const earlierCount = (session) => Number(session?.earlier_parts) > 0 ? Math.floor(Number(session.earlier_parts)) : 0;

// The routes, desktop (/api/cli) or phone (/api/mobile/cli).
export function earlierPath(base, id, cli = "", action = "") {
  return base + "/" + encodeURIComponent(id) + "/earlier" + (cli ? "/" + encodeURIComponent(cli) : "") + (action ? "/" + action : "");
}

const two = (value) => String(value).padStart(2, "0");

// "今天 14:05", "昨天 09:30", "11月20日 14:05", with the year when it differs.
export function clearedWhen(value, now = new Date()) {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return "";
  const time = two(date.getHours()) + ":" + two(date.getMinutes());
  const day = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).valueOf();
  const days = Math.round((day(now) - day(date)) / 86400000);
  if (days === 0) return "今天 " + time;
  if (days === 1) return "昨天 " + time;
  const year = date.getFullYear() !== now.getFullYear() ? date.getFullYear() + "年" : "";
  return year + (date.getMonth() + 1) + "月" + date.getDate() + "日 " + time;
}

export const earlierTitle = (part) => (part?.prompt || "").trim() || "（没有提问）";

export function earlierName(part, now) {
  return "清除于 " + clearedWhen(part.cleared_at, now) + "：" + earlierTitle(part) + (part.turns > 0 ? "，" + part.turns + " 轮" : "");
}
