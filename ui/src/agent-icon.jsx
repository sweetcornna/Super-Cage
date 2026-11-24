import React from "react";

// Claude mark: Simple Icons (CC0-1.0), matches provider-mark.jsx / claude-logo.svg.
const CLAUDE_PATH =
  "m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z";

// Codex mark: matches provider-mark.jsx braces-around-core silhouette.
const CODEX_PATH =
  "M10.1 2.6L9.2 2.6L8.3 2.7L7.5 3.1L6.7 3.6L6.2 4.4L5.8 5.3L5.6 6.2L5.6 7.1L5.6 7.9L5.6 8.6L5.5 9.3L5.3 10L4.7 10.7L3.5 12L4.7 13.3L5.3 14L5.5 14.7L5.6 15.4L5.6 16.1L5.6 16.9L5.6 17.8L5.8 18.7L6.2 19.6L6.7 20.4L7.5 20.9L8.3 21.3L9.2 21.4L10.1 21.4A0.68 0.68 0 0 1 10.3 20L10.3 20L9.5 19.9L9 19.6L8.7 19.3L8.6 19L8.5 18.6L8.5 18.2L8.6 17.6L8.6 16.7L8.4 15.7L8.1 14.7L7.5 13.7L6.7 12.8L5.7 12L4.9 12L5.7 12L6.7 11.2L7.5 10.3L8.1 9.3L8.4 8.3L8.6 7.3L8.6 6.4L8.5 5.8L8.5 5.4L8.6 5L8.7 4.7L9 4.4L9.5 4.1L10.3 4A0.68 0.68 0 0 1 10.1 2.6ZM13.7 4L14.5 4.1L15 4.4L15.3 4.7L15.4 5L15.5 5.4L15.5 5.8L15.4 6.4L15.4 7.3L15.6 8.3L15.9 9.3L16.5 10.3L17.3 11.2L18.3 12L19.1 12L18.3 12L17.3 12.8L16.5 13.7L15.9 14.7L15.6 15.7L15.4 16.7L15.4 17.6L15.5 18.2L15.5 18.6L15.4 19L15.3 19.3L15 19.6L14.5 19.9L13.7 20A0.68 0.68 0 0 1 13.9 21.4L13.9 21.4L14.8 21.4L15.7 21.3L16.5 20.9L17.3 20.4L17.8 19.6L18.2 18.7L18.4 17.8L18.4 16.9L18.4 16.1L18.4 15.4L18.5 14.7L18.7 14L19.3 13.3L20.5 12L19.3 10.7L18.7 10L18.5 9.3L18.4 8.6L18.4 7.9L18.4 7.1L18.4 6.2L18.2 5.3L17.8 4.4L17.3 3.6L16.5 3.1L15.7 2.7L14.8 2.6L13.9 2.6A0.68 0.68 0 0 1 13.7 4ZM12 10.2a1.8 1.8 0 1 1 0 3.6a1.8 1.8 0 1 1 0-3.6Z";

const BUILTIN_ACCENT = {
  claude: "#cd745b",
  codex: "#10a37f",
  antigravity: "#4285f4",
  opencode: "#e11d48",
  copilot: "#8b5cf6",
  qwen: "#6366f1",
  amp: "#f97316",
  cline: "#0ea5e9",
  cursor: "#14b8a6",
  vibe: "#f59e0b",
  kimi: "#3b82f6",
  aider: "#22c55e",
  droid: "#f43f5e",
  goose: "#06b6d4",
  pi: "#a855f7",
  grok: "var(--strong, #1f1e1d)",
  t3code: "var(--strong, #1f1e1d)",
};

const BUILTIN_LABEL = {
  claude: "Claude Code",
  codex: "Codex",
  antigravity: "Antigravity CLI",
  opencode: "OpenCode",
  copilot: "GitHub Copilot",
  qwen: "Qwen Code",
  amp: "Amp",
  cline: "Cline",
  cursor: "Cursor Agent",
  vibe: "Mistral Vibe",
  kimi: "Kimi Code",
  aider: "Aider",
  droid: "Factory Droid",
  goose: "Goose",
  pi: "Pi Agent",
  grok: "Grok",
  t3code: "T3 Code",
};

function renderBrandMark(id, color) {
  switch (id) {
    case "claude":
      return <path d={CLAUDE_PATH} fill={color} />;

    case "codex":
      return <path d={CODEX_PATH} fill={color} />;

    case "antigravity":
      // Sweeping aerodynamic upward arch with a levitating orbital core.
      return (
        <>
          <path
            d="M12 2.4c1.35 0 2.25.85 3.05 2.45l5.45 11.45c.7 1.48.25 3.1-1.05 3.8-1.2.65-2.65.12-3.55-1.15-1.1-1.56-2.25-2.55-3.9-2.55s-2.8.99-3.9 2.55c-.9 1.27-2.35 1.8-3.55 1.15-1.3-.7-1.75-2.32-1.05-3.8L8.95 4.85C9.75 3.25 10.65 2.4 12 2.4Zm0 4.55L8.75 13.7c1.05-.65 2.15-1 3.25-1s2.2.35 3.25 1L12 6.95Z"
            fill={color}
          />
          <circle cx="12" cy="19.1" r="2.15" fill="#34a853" />
        </>
      );

    case "opencode":
      // Architectural nested square frame block.
      return (
        <>
          <path
            d="M3.5 3.5h17v17h-17v-17Zm3.3 3.3v10.4h10.4V6.8H6.8Z"
            fill={color}
            fillRule="evenodd"
          />
          <rect x="9.3" y="9.3" width="5.4" height="5.4" rx="1" fill={color} fillOpacity="0.52" />
        </>
      );

    case "copilot":
      // Aviator visor helmet with twin luminous optical lenses.
      return (
        <>
          <path
            d="M12 3.2c-4.5 0-7.7 2.85-8 7.05-.9.35-1.5 1.2-1.5 2.25v2.2c0 1.25.95 2.25 2.15 2.35.95 1.8 3.65 3.15 7.35 3.15s6.4-1.35 7.35-3.15c1.2-.1 2.15-1.1 2.15-2.35v-2.2c0-1.05-.6-1.9-1.5-2.25-.3-4.2-3.5-7.05-8-7.05Z"
            fill={color}
            fillOpacity="0.18"
            stroke={color}
            strokeWidth="1.75"
            strokeLinejoin="round"
          />
          <rect x="6.4" y="9.4" width="11.2" height="6.8" rx="3.4" fill={color} />
          <rect x="8.65" y="10.95" width="2.2" height="3.7" rx="1.1" fill="var(--paper, #ffffff)" />
          <rect x="13.15" y="10.95" width="2.2" height="3.7" rx="1.1" fill="var(--paper, #ffffff)" />
        </>
      );

    case "qwen":
      // Interlocking 6-point geometric star crystal.
      return (
        <>
          <path
            d="M12 2.2 14.7 6.8l5.3.2-2.8 4.6 2.8 4.6-5.3.2L12 21.8l-2.7-5.4-5.3-.2 2.8-4.6L4 7l5.3-.2L12 2.2Z"
            fill={color}
            fillOpacity="0.22"
            stroke={color}
            strokeWidth="1.65"
            strokeLinejoin="round"
          />
          <polygon
            points="12,5.6 15.8,9.8 14.3,15.6 9.7,15.6 8.2,9.8"
            fill={color}
            fillOpacity="0.45"
          />
          <circle cx="12" cy="12" r="2.6" fill={color} />
        </>
      );

    case "amp":
      // High-voltage lightning bolt framed in an energy arc.
      return (
        <>
          <circle cx="12" cy="12" r="9.3" fill={color} fillOpacity="0.14" stroke={color} strokeWidth="1.5" strokeDasharray="44 14" strokeLinecap="round" />
          <path d="M13.5 3.6 6.8 13h4.55l-.85 7.4 6.7-9.4h-4.55l.85-7.4Z" fill={color} />
        </>
      );

    case "cline":
      // Mechanical assistant head with antenna and expressive vertical visor eyes.
      return (
        <>
          <path d="M12 3.8v3.1" stroke={color} strokeWidth="1.9" strokeLinecap="round" />
          <circle cx="12" cy="3.1" r="1.6" fill={color} />
          <rect x="2.1" y="11.2" width="2.1" height="4.8" rx="1" fill={color} fillOpacity="0.65" />
          <rect x="19.8" y="11.2" width="2.1" height="4.8" rx="1" fill={color} fillOpacity="0.65" />
          <rect x="4.5" y="6.8" width="15" height="13.4" rx="4.2" fill={color} fillOpacity="0.18" stroke={color} strokeWidth="1.8" />
          <rect x="8.3" y="10.8" width="2.4" height="5.2" rx="1.2" fill={color} />
          <rect x="13.3" y="10.8" width="2.4" height="5.2" rx="1.2" fill={color} />
        </>
      );

    case "cursor":
      // Isometric 3D prism cube with directional pointer facet.
      return (
        <>
          <polygon points="12,2.3 20.6,7.2 12,12.1 3.4,7.2" fill={color} fillOpacity="0.48" />
          <polygon points="3.4,7.2 12,12.1 12,21.7 3.4,16.8" fill={color} fillOpacity="0.78" />
          <polygon points="20.6,7.2 12,12.1 12,21.7 20.6,16.8" fill={color} />
          <polygon points="12,5.3 17.6,8.5 12,11.7 13.7,8.5" fill="var(--paper, #ffffff)" fillOpacity="0.92" />
        </>
      );

    case "vibe":
      // Mistral-style stepped geometric pixel bars forming an M crest.
      return (
        <>
          <rect x="3" y="4" width="3.4" height="3.1" rx="0.5" fill={color} fillOpacity="0.55" />
          <rect x="17.6" y="4" width="3.4" height="3.1" rx="0.5" fill={color} fillOpacity="0.55" />
          <rect x="3" y="7.8" width="6.8" height="3.1" rx="0.5" fill={color} fillOpacity="0.72" />
          <rect x="14.2" y="7.8" width="6.8" height="3.1" rx="0.5" fill={color} fillOpacity="0.72" />
          <rect x="3" y="11.6" width="18" height="3.1" rx="0.5" fill={color} fillOpacity="0.88" />
          <rect x="3" y="15.4" width="3.8" height="4.2" rx="0.6" fill={color} />
          <rect x="10.1" y="15.4" width="3.8" height="4.2" rx="0.6" fill={color} />
          <rect x="17.2" y="15.4" width="3.8" height="4.2" rx="0.6" fill={color} />
        </>
      );

    case "kimi":
      // Moonshot crescent moon with an orbital 4-point star sparkle.
      return (
        <>
          <path
            d="M14.35 3.6a8.6 8.6 0 1 0 6.15 14.35 6.85 6.85 0 0 1-6.15-14.35Z"
            fill={color}
          />
          <path
            d="M18.1 3.4 18.85 5.6 21 6.35l-2.15.75-.75 2.2-.75-2.2-2.15-.75 2.15-.75.75-2.2Z"
            fill={color}
            fillOpacity="0.85"
          />
        </>
      );

    case "aider":
      // Git pair-programming merge branch with three commit nodes.
      return (
        <>
          <path d="M7.5 7.8v8.4" stroke={color} strokeWidth="2.1" strokeLinecap="round" />
          <path d="M16.5 9.6c0 3.7-2.6 5.2-9 5.2" fill="none" stroke={color} strokeWidth="2.1" strokeLinecap="round" />
          <circle cx="7.5" cy="5.8" r="2.4" fill={color} />
          <circle cx="7.5" cy="18.2" r="2.4" fill={color} />
          <circle cx="16.5" cy="7.4" r="2.6" fill={color} fillOpacity="0.28" stroke={color} strokeWidth="1.9" />
          <path d="M17.8 16.2l.55 1.45 1.45.55-1.45.55-.55 1.45-.55-1.45-1.45-.55 1.45-.55.55-1.45Z" fill={color} />
        </>
      );

    case "droid":
      // Industrial faceted hexagonal chassis with robotic precision aperture.
      return (
        <>
          <polygon
            points="12,2.3 20.4,7.15 20.4,16.85 12,21.7 3.6,16.85 3.6,7.15"
            fill={color}
            fillOpacity="0.16"
            stroke={color}
            strokeWidth="1.8"
            strokeLinejoin="round"
          />
          <circle cx="12" cy="12" r="4.6" fill="none" stroke={color} strokeWidth="1.85" />
          <circle cx="12" cy="12" r="2.1" fill={color} />
        </>
      );

    case "goose":
      // Minimalist geometric origami goose silhouette.
      return (
        <>
          <path
            d="M15.4 3.6c-1.85 0-3.25 1.45-3.25 3.45 0 1.45-.35 2.7-1.45 3.75L5.2 14.7c-.85.62-.72 1.95.24 2.4 1.85.88 4.45 1.4 7.16 1.4 4.15 0 7.2-2.25 7.4-5.75.08-1.42-.9-2.2-2.15-2.05-1.35.15-2.45.75-3.5 1.35.8-1.45 1.35-3.05 1.35-4.95 0-.85.5-1.35 1.35-1.35h1.95l2.05-1.15-2.25-1h-3.4Z"
            fill={color}
          />
          <path
            d="M9.6 18.5v2.3M13.8 18.5v2.3"
            stroke={color}
            strokeWidth="1.8"
            strokeLinecap="round"
          />
        </>
      );

    case "pi":
      // Pure geometric vector pi symbol (paths, not text).
      return (
        <path
          d="M4.1 8.8c.95-2.25 2.7-3.2 5.2-3.2h10.6v2.7h-3.05v7.55c0 1.35.58 1.95 1.62 1.95.52 0 1.02-.12 1.43-.34v2.45c-.62.32-1.45.49-2.38.49-2.28 0-3.57-1.35-3.57-4V8.3h-3.55v11.95H7.5V8.3h-.72c-1.12 0-1.82.42-2.22 1.25L4.1 8.8Z"
          fill={color}
        />
      );

    case "grok":
      // Orbital black-hole ring pierced by a diagonal trajectory slash.
      return (
        <>
          <path
            d="M17.9 6.7a7.8 7.8 0 1 0 1.6 7.5"
            fill="none"
            stroke={color}
            strokeWidth="2.35"
            strokeLinecap="round"
          />
          <path
            d="M5.2 19.2 19.6 4.4"
            stroke={color}
            strokeWidth="2.45"
            strokeLinecap="round"
          />
        </>
      );

    case "t3code":
      // Multi-pane workbench terminal window icon.
      return (
        <>
          <rect
            x="2.4"
            y="3.6"
            width="19.2"
            height="16.8"
            rx="3.4"
            fill={color}
            fillOpacity="0.1"
            stroke={color}
            strokeWidth="1.8"
          />
          <path d="M2.6 8.2h18.8M8.5 8.2v12" stroke={color} strokeWidth="1.55" />
          <path
            d="m11.4 12.1 2.4 2.1-2.4 2.1M15.2 16.3h3.2"
            fill="none"
            stroke={color}
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </>
      );

    default:
      // Generic custom Agent CLI vector icon: terminal prompt + AI sparkle.
      return (
        <>
          <rect
            x="2.6"
            y="4.2"
            width="18.8"
            height="15.6"
            rx="3.6"
            fill={color}
            fillOpacity="0.14"
            stroke={color}
            strokeWidth="1.75"
          />
          <path
            d="m6.8 9.8 2.9 2.4-2.9 2.4M11.1 14.6h4.1"
            fill="none"
            stroke={color}
            strokeWidth="1.9"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          <path
            d="M17.7 7.1l.55 1.45 1.45.55-1.45.55-.55 1.45-.55-1.45-1.45-.55 1.45-.55.55-1.45Z"
            fill={color}
          />
        </>
      );
  }
}

// Every mainstream and custom Agent CLI renders a dedicated vector icon (never
// a two-letter initial monogram). Optional accountNumber adds a crisp numeric
// index pill at the bottom-right corner without obscuring the brand mark.
export function AgentIcon({ accountNumber, agentId = "", accentColor = "", size = 20 }) {
  const id = String(agentId || "").trim().toLowerCase();
  const number = Number.isInteger(accountNumber) && accountNumber > 0 ? String(accountNumber) : "";

  if (!id) {
    return (
      <svg
        className="agent-icon"
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
        role="img"
        aria-label={number ? `Agent · 账号 ${number}` : "Agent"}
      >
        <path d="M12 3v3M2 11v5m20-5v5" />
        <rect x="4" y="6" width="16" height="15" rx="4" />
        {number ? (
          <text
            x="12"
            y="14"
            dominantBaseline="central"
            textAnchor="middle"
            fill="currentColor"
            stroke="none"
            fontFamily="var(--ui-font-family, system-ui, sans-serif)"
            fontSize={number.length === 1 ? 12 : number.length === 2 ? 10 : 8}
            fontWeight="700"
            {...(number.length > 2 ? { textLength: 13, lengthAdjust: "spacingAndGlyphs" } : {})}
          >
            {number}
          </text>
        ) : (
          <path d="M8.5 11v2m7-2v2M9 17h6" />
        )}
      </svg>
    );
  }

  const accent = accentColor || BUILTIN_ACCENT[id] || "var(--accent, #cd745b)";
  const displayName = BUILTIN_LABEL[id] || agentId;
  const label = id === "t3code"
    ? "T3 Code"
    : number ? `${displayName} · 账号 ${number}` : displayName;

  return (
    <svg
      className={`agent-icon agent-icon--brand agent-icon--${id}`}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      role="img"
      aria-label={label}
    >
      <g transform={number ? "translate(-1.2 -1.2) scale(0.92)" : undefined}>
        {renderBrandMark(id, accent)}
      </g>
      {number && (
        <g>
          <circle
            cx="18.2"
            cy="18.2"
            r="5.2"
            fill={accent}
            stroke="var(--paper, #ffffff)"
            strokeWidth="1.4"
          />
          <text
            x="18.2"
            y="18.4"
            dominantBaseline="central"
            textAnchor="middle"
            fill="var(--paper, #ffffff)"
            fontFamily="var(--ui-font-family, system-ui, sans-serif)"
            fontSize={number.length > 1 ? 6.5 : 7.8}
            fontWeight="750"
          >
            {number}
          </text>
        </g>
      )}
    </svg>
  );
}
