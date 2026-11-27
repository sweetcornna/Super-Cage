import React, { useState } from "react";
import { Bot } from "lucide-react";
import aider from "./harness-marks/aider.png?no-inline";
import amp from "./harness-marks/amp.svg?no-inline";
import antigravity from "./harness-marks/antigravity.png?no-inline";
import claude from "./harness-marks/claude.png?no-inline";
import cline from "./harness-marks/cline.svg?no-inline";
import codex from "./harness-marks/codex.png?no-inline";
import codexDark from "./harness-marks/codex-dark.png?no-inline";
import copilot from "./harness-marks/copilot.svg?no-inline";
import cursor from "./harness-marks/cursor.svg?no-inline";
import cursorDark from "./harness-marks/cursor-dark.svg?no-inline";
import deepseekHarness from "./harness-marks/deepseek-harness.png?no-inline";
import droid from "./harness-marks/droid.svg?no-inline";
import goose from "./harness-marks/goose.svg?no-inline";
import grok from "./harness-marks/grok.png?no-inline";
import herdr from "./harness-marks/herdr.svg?no-inline";
import kimi from "./harness-marks/kimi.png?no-inline";
import magpie from "./harness-marks/magpie.png?no-inline";
import opencode from "./harness-marks/opencode.svg?no-inline";
import orca from "./harness-marks/orca.png?no-inline";
import pi from "./harness-marks/pi.svg?no-inline";
import qwen from "./harness-marks/qwen.svg?no-inline";
import t3code from "./harness-marks/t3code.svg?no-inline";
import vibe from "./harness-marks/vibe.svg?no-inline";
import "./harness-mark.css";

// Upstream files and license notices are recorded in public/harness-marks-notice.txt.
const marks = {
  aider: { name: "Aider", src: aider },
  amp: { name: "Amp", src: amp },
  antigravity: { name: "Antigravity CLI", src: antigravity },
  claude: { name: "Claude Code", src: claude },
  cline: { name: "Cline", src: cline, lightBackground: true },
  codex: { name: "Codex", src: codex, darkSrc: codexDark },
  copilot: { name: "GitHub Copilot", src: copilot, lightBackground: true },
  cursor: { name: "Cursor", src: cursor, darkSrc: cursorDark },
  "deepseek-harness": { name: "DeepSeek Harness", src: deepseekHarness },
  droid: { name: "Factory Droid", src: droid },
  goose: { name: "Goose", src: goose },
  grok: { name: "Grok", src: grok },
  herdr: { name: "Herdr", src: herdr },
  kimi: { name: "Kimi Code", src: kimi },
  magpie: { name: "Magpie", src: magpie },
  opencode: { name: "OpenCode", src: opencode },
  orca: { name: "Orca", src: orca },
  pi: { name: "Pi Agent", src: pi },
  qwen: { name: "Qwen Code", src: qwen, lightBackground: true },
  t3code: { name: "T3 Code", src: t3code },
  vibe: { name: "Mistral Vibe", src: vibe, lightBackground: true },
};

const aliases = new Map(Object.entries({
  "aider-chat": "aider",
  "antigravity-cli": "antigravity",
  "claude-code": "claude",
  "codex-cli": "codex",
  "copilot-cli": "copilot",
  "cursor-agent": "cursor",
  "cursor-cli": "cursor",
  dsh: "deepseek-harness",
  "factory-droid": "droid",
  "github-copilot": "copilot",
  "grok-cli": "grok",
  "herdr-core": "herdr",
  "kimi-code": "kimi",
  "mistral-vibe": "vibe",
  "openai-codex": "codex",
  "pi-agent": "pi",
  "pi-coding-agent": "pi",
  "qwen-code": "qwen",
  "t3-code": "t3code",
}));

export function harnessMarkId(id) {
  const key = typeof id === "string" ? id.trim().toLowerCase() : "";
  return aliases.get(key) || (Object.hasOwn(marks, key) ? key : "");
}

function MarkGraphic({ src, lightBackground, fallbackColor, variant = "" }) {
  const [failed, setFailed] = useState(false);
  const className = `harness-mark-graphic${variant ? ` harness-mark-graphic--${variant}` : ""}`;
  if (!src || failed) {
    return <Bot className={className} style={{ color: fallbackColor }} aria-hidden="true" focusable="false" />;
  }
  return (
    <img
      className={className}
      data-light-background={lightBackground || undefined}
      src={src}
      alt=""
      draggable={false}
      onError={() => setFailed(true)}
    />
  );
}

export function HarnessMark({ agentId, accountNumber, accentColor, decorative, size, label, slot, className = "" }) {
  const id = harnessMarkId(agentId);
  const mark = marks[id];
  const name = label || mark?.name || (typeof agentId === "string" && agentId.trim()) || "Agent";
  const number = Number.isInteger(accountNumber) && accountNumber > 0 ? String(accountNumber) : "";
  const modifier = id ? ` ${slot}-${id}` : "";
  return (
    <i
      className={`harness-mark ${slot}${modifier}${className ? ` ${className}` : ""}`}
      style={{ width: size, height: size }}
      data-number={number || undefined}
      role={decorative ? undefined : "img"}
      aria-label={decorative ? undefined : number ? `${name} · 账号 ${number}` : name}
      aria-hidden={decorative || undefined}
    >
      <MarkGraphic
        key={mark?.src || id}
        src={mark?.src}
        lightBackground={mark?.lightBackground}
        fallbackColor={mark ? undefined : accentColor}
        variant={mark?.darkSrc ? "light" : ""}
      />
      {mark?.darkSrc && <MarkGraphic key={mark.darkSrc} src={mark.darkSrc} variant="dark" />}
    </i>
  );
}

export function AgentIcon({ size = 20, ...props }) {
  return <HarnessMark {...props} size={size} slot="agent-icon" />;
}
