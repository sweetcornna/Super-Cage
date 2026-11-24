import React from "react";

const BUILTIN_BRAND = {
  codex: { glyph: "CX", accent: "#10a37f" },
  gemini: { glyph: "GM", accent: "#4285f4" },
  opencode: { glyph: "OC", accent: "#e11d48" },
  copilot: { glyph: "CP", accent: "#8b5cf6" },
  qwen: { glyph: "QW", accent: "#6366f1" },
  amp: { glyph: "AM", accent: "#f97316" },
  cline: { glyph: "CN", accent: "#0ea5e9" },
  cursor: { glyph: "CR", accent: "#14b8a6" },
  vibe: { glyph: "VB", accent: "#f59e0b" },
  kimi: { glyph: "KM", accent: "#3b82f6" },
  aider: { glyph: "AD", accent: "#22c55e" },
  droid: { glyph: "DR", accent: "#f43f5e" },
  goose: { glyph: "GS", accent: "#06b6d4" },
  pi: { glyph: "PI", accent: "#a855f7" },
  t3code: { glyph: "T3", accent: "var(--strong, #1f1e1d)" },
};

// Keep the account number legible in conversation rows.
// The outline inherits the chrome color for Claude; other mainstream/custom
// agents render a crisp themed monogram badge.
export function AgentIcon({ accountNumber, agentId = "", glyph = "", accentColor = "", size = 20 }) {
  const number = Number.isInteger(accountNumber) && accountNumber > 0 ? String(accountNumber) : "";
  if (agentId && agentId !== "claude") {
    const brand = BUILTIN_BRAND[agentId] || {};
    const text = (glyph || brand.glyph || agentId.slice(0, 2)).toUpperCase().slice(0, 3);
    const accent = accentColor || brand.accent || "var(--accent)";
    const label = agentId === "t3code"
      ? "T3 Code"
      : number ? `${agentId} · 账号 ${number}` : agentId;
    return (
      <svg
        className="agent-icon agent-icon--badge"
        width={size}
        height={size}
        viewBox="0 0 24 24"
        role="img"
        aria-label={label}
      >
        <rect
          x="2"
          y="2"
          width="20"
          height="20"
          rx="6"
          fill={accent}
          fillOpacity="0.14"
          stroke={accent}
          strokeWidth="1.6"
        />
        <text
          x="12"
          y="12.6"
          dominantBaseline="central"
          textAnchor="middle"
          fill={accent}
          fontFamily="var(--ui-font-family, system-ui, sans-serif)"
          fontSize={text.length <= 2 ? 9.5 : 7.5}
          fontWeight="750"
          letterSpacing="-0.02em"
        >
          {text}
        </text>
      </svg>
    );
  }
  return <svg className="agent-icon" width={size} height={size} viewBox="0 0 24 24"
    fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"
    role="img" aria-label={number ? `Agent · 账号 ${number}` : "Agent"}>
    <path d="M12 3v3M2 11v5m20-5v5" />
    <rect x="4" y="6" width="16" height="15" rx="4" />
    {number ? <text x="12" y="14" dominantBaseline="central" textAnchor="middle"
      fill="currentColor" stroke="none" fontFamily="var(--ui-font-family, system-ui, sans-serif)"
      fontSize={number.length === 1 ? 12 : number.length === 2 ? 10 : 8} fontWeight="700"
      {...(number.length > 2 ? { textLength: 13, lengthAdjust: "spacingAndGlyphs" } : {})}>{number}</text>
      : <><path d="M8.5 11v2m7-2v2M9 17h6" /></>}
  </svg>;
}
