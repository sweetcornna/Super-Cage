import React, { useId } from "react";

// Claude mark: Simple Icons (CC0-1.0), matches provider-mark.jsx / claude-logo.svg.
const CLAUDE_PATH =
  "m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z";

// Codex mark: matches provider-mark.jsx braces-around-core silhouette.
const CODEX_PATH =
  "M10.1 2.6L9.2 2.6L8.3 2.7L7.5 3.1L6.7 3.6L6.2 4.4L5.8 5.3L5.6 6.2L5.6 7.1L5.6 7.9L5.6 8.6L5.5 9.3L5.3 10L4.7 10.7L3.5 12L4.7 13.3L5.3 14L5.5 14.7L5.6 15.4L5.6 16.1L5.6 16.9L5.6 17.8L5.8 18.7L6.2 19.6L6.7 20.4L7.5 20.9L8.3 21.3L9.2 21.4L10.1 21.4A0.68 0.68 0 0 1 10.3 20L10.3 20L9.5 19.9L9 19.6L8.7 19.3L8.6 19L8.5 18.6L8.5 18.2L8.6 17.6L8.6 16.7L8.4 15.7L8.1 14.7L7.5 13.7L6.7 12.8L5.7 12L4.9 12L5.7 12L6.7 11.2L7.5 10.3L8.1 9.3L8.4 8.3L8.6 7.3L8.6 6.4L8.5 5.8L8.5 5.4L8.6 5L8.7 4.7L9 4.4L9.5 4.1L10.3 4A0.68 0.68 0 0 1 10.1 2.6ZM13.7 4L14.5 4.1L15 4.4L15.3 4.7L15.4 5L15.5 5.4L15.5 5.8L15.4 6.4L15.4 7.3L15.6 8.3L15.9 9.3L16.5 10.3L17.3 11.2L18.3 12L19.1 12L18.3 12L17.3 12.8L16.5 13.7L15.9 14.7L15.6 15.7L15.4 16.7L15.4 17.6L15.5 18.2L15.5 18.6L15.4 19L15.3 19.3L15 19.6L14.5 19.9L13.7 20A0.68 0.68 0 0 1 13.9 21.4L13.9 21.4L14.8 21.4L15.7 21.3L16.5 20.9L17.3 20.4L17.8 19.6L18.2 18.7L18.4 17.8L18.4 16.9L18.4 16.1L18.4 15.4L18.5 14.7L18.7 14L19.3 13.3L20.5 12L19.3 10.7L18.7 10L18.5 9.3L18.4 8.6L18.4 7.9L18.4 7.1L18.4 6.2L18.2 5.3L17.8 4.4L17.3 3.6L16.5 3.1L15.7 2.7L14.8 2.6L13.9 2.6A0.68 0.68 0 0 1 13.7 4ZM12 10.2a1.8 1.8 0 1 1 0 3.6a1.8 1.8 0 1 1 0-3.6Z";

// Official Magpie vector mark from yetone/magpie/site/public/img/magpie-small.svg (44x44 viewBox).
const MAGPIE_PATH =
  "M1.5 5.5C1.3 5.7 1.2 6.5 1.2 6.9C1.2 6.9 1.2 7 1.2 7.1C1.1 7.4 1.3 8 1.4 8.3C1.4 8.4 1.5 8.5 1.5 8.7C1.5 8.9 1.7 9 1.8 9.3C2.2 9.9 2.2 10 2.3 10.1C2.4 10.2 2.5 10.3 2.6 10.5C2.8 10.8 3 11.1 3.2 11.3C3.3 11.3 3.4 11.6 3.6 11.8C3.8 12 4.1 12.4 4.2 12.5C4.7 13 5.2 13.7 5.4 13.9C6 14.5 6.5 15.1 6.7 15.3C6.7 15.4 6.9 15.5 6.9 15.7C7.3 15.9 7.7 16.5 7.9 16.8C7.9 16.8 8 16.9 8 16.9C8.1 16.9 8.2 17.2 8.2 17.3C8.2 17.3 8.1 17.2 8.1 17.1C7.9 16.9 7.9 16.9 7.6 16.6C7.5 16.5 7.4 16.5 7.4 16.3C7.2 16.2 6.7 15.7 6.3 15.2C6.1 15.1 6 14.9 6 14.9C5.9 14.8 5.7 14.7 5.7 14.6C5.6 14.5 5.4 14.3 5.3 14.2C5.1 14.1 4.9 13.8 4.7 13.5C4.5 13.3 4.2 13.1 4.1 12.9C4.1 12.9 3.8 12.7 3.8 12.5C3.5 12.3 3 11.8 2.8 11.4C1.8 10.5 0.9 9.6 0.7 9.6C0.6 9.7 0.6 9.8 0.5 10C0.5 10.1 0.6 10.2 0.5 10.3C0.5 10.4 0.6 10.5 0.6 10.6C0.5 10.7 0.5 10.9 0.6 11C0.5 11.1 0.6 11.4 0.6 11.5C0.9 12.9 1.3 13.8 2.8 14.9C2.9 15.1 3.3 15.3 3.4 15.4C3.4 15.5 3.6 15.6 3.7 15.7C4.2 16 6.4 17.8 7 18.4C7.4 18.8 7.4 18.8 7.5 18.9C7.6 19 7.7 19 7.7 19.1C7.7 19.2 7.8 19.2 7.9 19.2C7.9 19.3 8.1 19.4 8.1 19.5C8.2 19.6 8.2 19.7 8.3 19.7C8.3 19.7 8.3 19.7 8.2 19.7C8.2 19.6 8.1 19.6 8 19.6C7.9 19.5 7.9 19.4 7.8 19.4C7.7 19.3 7.6 19.2 7.6 19.2C7.6 19.1 7.5 19 7.3 18.9C7.2 18.9 6.9 18.6 6.7 18.4C6.1 17.8 4.5 16.6 3.5 16C3.4 15.8 3 15.6 2.9 15.5C2.6 15.3 1.5 14.6 0.9 14.3C0.7 14.1 0.6 14.1 0.6 14.2C0.5 14.5 0.8 15.9 0.9 16.1C0.9 16.2 1 16.2 0.9 16.3C1.2 16.8 1.9 17.6 2.7 18.3C2.8 18.5 4 19.2 4.7 19.5C6.2 20.5 6.6 20.7 7.4 21.5C8.2 22.2 8.7 22.7 9.3 23.6C9.5 24.1 9.9 24.4 10.3 25.3C10.4 25.5 10.5 25.8 10.5 25.8C10.7 25.8 10.8 25.7 11 25.4C11.6 25 11.9 24.6 12.3 24.3C12.8 24 13 23.8 13 23.7C13.1 23.7 13.1 23.8 13 24C12 24.8 11 26 10.2 26.9C10.1 27.1 9.9 27.5 9.7 27.7C9.5 27.8 9.4 28 9.4 28C9.4 28 9.3 28.3 9.1 28.5C8.6 29.2 7.7 30.8 7.7 30.9C7.7 31 7.8 31 8.3 30.8C9 30.4 9.5 30.2 10.4 30C11.7 29.6 12.5 29.6 11.6 30C10.5 30.2 9.7 30.6 9.2 31.2C8.8 31.7 8.8 31.7 9.3 31.7C10.1 31.8 10.9 31.9 11.6 32.2C12.8 32.7 13.5 33.1 15.5 34.8C16.4 35.5 17.3 36.2 17.8 36.4C17.9 36.5 18.1 36.7 18.2 36.8C19.1 37.3 21.2 38.1 22.3 38.3C22.4 38.3 22.7 38.3 22.8 38.3C22.8 38.5 23 38.5 23 38.5C23.1 38.5 23.2 38.5 23.3 38.5C23.6 38.6 25.9 38.6 26.1 38.5C26.2 38.5 26.3 38.5 26.4 38.5C26.5 38.5 26.6 38.5 26.7 38.3C26.7 38.3 27 38.3 27.1 38.3C28.3 38.1 30.2 37.3 31.4 36.7C31.5 36.7 31.7 36.4 31.8 36.4C32.2 36.2 33.2 35.4 33.4 35.2C34.2 34.5 35.5 33 35.8 32.6C35.8 32.5 36 32.2 36.1 32C36.4 31.8 36.6 31.4 36.6 31.2C36.7 31.1 36.8 30.9 36.9 30.8C37.2 30.4 37.5 29.6 37.7 29.2C37.7 29.1 37.8 28.7 37.9 28.7C38.1 28.5 38.4 27.1 38.5 26.8C39 25.2 39.1 23.7 39 22.4C39 22.2 38.9 21.9 38.9 21.8C38.9 21.7 38.9 21.6 38.9 21.4C38.6 20.5 38.5 19.9 38.2 19C37.9 18.5 37.5 17.5 37.3 17.2C37 16.8 36.8 15.8 36.8 15.2C36.8 13.8 37.7 12.7 39.6 12.2C40.1 12.1 41.3 11.8 42.1 11.7C43.5 11.5 43.5 11.5 43.4 11.3C42.9 10.7 40.9 9.9 39.2 9.7C37.9 9.5 38.1 9.5 37.6 9C37 8.6 36.4 8 36 7.9C36 7.8 35.8 7.8 35.7 7.7C35.1 7.3 34.2 7 33.1 6.9C32.6 6.7 31.6 6.9 31 7C29.6 7.3 28.3 8 27.3 9.2C26.7 9.8 26.2 10.8 25.7 11.7C25.5 12.1 24.8 13.4 24.7 13.7C24.6 13.9 24.5 14.1 24.1 14.7C23.5 15.7 22.1 16.9 20.6 17.8C20.5 18 20.3 18.2 20.1 18.3C19.7 18.5 19.4 18.8 19.1 19C18.9 19.1 18.7 19.3 18.5 19.4C18 19.8 17.2 20.3 17 20.5C16.9 20.5 16.8 20.6 16.7 20.7C16.3 20.8 16.1 20.9 16 20.9C15.5 20.8 14.9 20.5 14.2 20.2C13.7 19.8 12.7 18.9 12.4 18.6C12.3 18.5 12.2 18.4 12.1 18.2C11.9 18 11.2 17.1 11 16.6C10.9 16.5 10.8 16.4 10.7 16.3C10.7 16.2 10.6 16.1 10.6 15.9C10.5 15.9 10.4 15.7 10.4 15.6C10.3 15.5 10.2 15.2 10.1 15C10 14.8 9.8 14.4 9.7 14.2C9.6 14 9.5 13.7 9.4 13.6C9.4 13.4 9.3 13.2 9.2 13C9.2 12.8 9 12.6 9 12.5C8.9 12.3 8.7 12.1 8.7 12C8.7 12 8.5 11.7 8.5 11.7C8.3 11.2 8.2 10.9 7.5 10C6.4 8.3 4.2 6.4 2.7 5.8C2 5.5 1.6 5.4 1.5 5.5ZM28.4 16.7C29.2 16.9 29.7 17.2 29.9 17.7C30.1 18.2 30 18.9 29.6 19.4C29.2 19.9 28.2 20.9 27.6 21.2C26.8 21.7 25.3 22.4 24.2 22.7C23.9 22.8 23.6 22.9 23.5 22.9C22.8 23.2 20.7 23.7 19.6 24C19 24.2 18.4 24.3 18.1 24.4C17.9 24.4 16.1 24.9 15.3 25.1C14.6 25.2 14.7 25.1 15.7 23.6C16.4 22.7 18.5 20.6 19.4 19.9C20.8 18.8 21.4 18.4 23.2 17.6C23.8 17.4 25.6 16.8 25.9 16.8C25.9 16.8 26.3 16.8 26.6 16.7C26.8 16.7 28.2 16.7 28.4 16.7ZM30.4 20.2C30.5 20.3 30.6 20.6 30.6 20.7C31.7 23.1 33.4 24.6 35.6 25.3C36.1 25.5 36.7 25.7 37.2 25.8C37.7 25.8 37.8 25.9 37.8 26.1C37.7 26.2 37.7 26.3 37.7 26.5C37.7 26.6 37.7 26.8 37.6 27C37.4 28.5 36.8 29.9 36.1 31C36.1 31.2 35.9 31.4 35.9 31.6C35.7 31.9 35.2 32.6 34.7 33.3C32.6 35.6 29.7 37.3 26.6 37.8C26.5 37.8 26.3 37.9 26.2 37.9C25.8 37.9 23.7 37.9 23.3 37.9C23.2 37.9 23 37.8 22.9 37.8C21.1 37.6 19.3 36.8 18 35.7C17.7 35.4 17 34.7 16.8 34.5C16.3 33.8 16.1 33 16.3 32.5C16.5 31.8 16.8 31.6 17.9 31.1C19.1 30.5 19.4 30.4 20.2 30C21.4 29.4 23.2 28.4 23.9 27.9C23.9 27.8 24.2 27.7 24.5 27.6C26.3 26.3 28 24.6 28.9 22.9C29.5 22.2 29.7 21.6 29.9 20.7C30.1 20 30.2 19.9 30.4 20.2Z";

// Official Herdr sheep vector mark from herdrdev/herdr/assets/logo.svg (5120x5120 inverted-Y space).
const HERDR_PATH =
  "M2794 3710 c-129 -33 -299 -135 -359 -214 -21 -28 -26 -42 -21 -63 9 -38 154 -178 199 -192 32 -11 41 -9 104 23 171 86 354 70 475 -43 150 -138 150 -379 0 -511 -107 -95 -278 -94 -386 2 l-46 40 -11 -29 c-16 -40 -14 -122 4 -164 60 -144 264 -222 452 -174 360 92 559 494 430 868 -36 103 -81 173 -175 267 -71 72 -100 93 -180 132 -52 26 -127 54 -167 62 -96 21 -230 20 -319 -4z M2183 3695 c-116 -32 -221 -108 -273 -199 -17 -28 -30 -54 -30 -58 0 -4 20 1 45 12 66 28 220 68 294 76 64 7 65 7 137 83 40 41 71 77 69 79 -2 2 -21 8 -42 13 -55 12 -140 10 -200 -6z M2212 3388 c-159 -22 -390 -122 -559 -241 -299 -210 -585 -600 -609 -828 -12 -118 40 -251 125 -318 96 -76 178 -98 426 -116 110 -8 224 -21 254 -29 125 -34 230 -115 272 -211 11 -24 24 -81 30 -127 20 -170 65 -271 166 -374 34 -35 63 -65 63 -67 0 -1 -10 -27 -22 -57 -29 -76 -37 -259 -14 -350 42 -170 158 -318 311 -397 44 -23 98 -46 120 -52 22 -6 45 -14 51 -18 5 -5 15 -48 22 -96 6 -48 14 -92 17 -97 4 -6 415 -10 1131 -10 l1124 0 0 1584 0 1585 -55 -19 c-84 -29 -143 -68 -232 -154 l-83 -78 -54 49 c-111 102 -233 151 -391 160 -113 6 -199 -10 -298 -54 l-60 -27 -26 34 c-37 51 -120 134 -127 128 -3 -4 0 -34 7 -67 17 -89 7 -268 -21 -356 -103 -328 -377 -545 -688 -545 -161 0 -273 41 -373 137 -37 35 -66 75 -85 116 -79 173 -8 407 124 407 44 0 68 -14 117 -66 74 -78 167 -82 238 -9 60 62 72 147 33 231 -42 90 -120 130 -238 122 -50 -4 -85 -14 -131 -37 -89 -45 -122 -52 -176 -40 -92 20 -262 163 -302 253 -11 25 -21 45 -22 45 -1 -1 -30 -6 -65 -11z m-256 -505 c115 -88 129 -102 132 -131 2 -18 -2 -40 -9 -49 -7 -8 -69 -55 -138 -105 -103 -73 -130 -88 -151 -83 -35 8 -51 34 -48 74 3 31 12 42 83 92 44 31 80 61 82 66 1 4 -30 31 -69 58 -39 28 -77 56 -85 63 -18 19 -16 72 4 94 32 35 63 23 199 -79z m528 -88 c23 -24 28 -52 14 -82 l-13 -28 -141 -3 c-130 -2 -142 -1 -158 17 -23 26 -24 66 -1 91 16 18 32 20 151 20 105 0 136 -3 148 -15z";

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
  magpie: "var(--strong, #1f1e1d)",
  herdr: "var(--strong, #1f1e1d)",
  orca: "var(--strong, #1f1e1d)",
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
  magpie: "Magpie",
  herdr: "Herdr",
  orca: "Orca",
};

function renderBrandMark(id, color, clipId) {
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

    case "magpie":
      // Official Magpie bird silhouette scaled from 44x44 into 24x24.
      return (
        <g transform="scale(0.5454545)">
          <path d={MAGPIE_PATH} fill={color} fillRule="evenodd" />
        </g>
      );

    case "herdr":
      // Official Herdr sheep silhouette scaled and clipped inside a rounded square.
      return (
        <>
          <defs>
            <clipPath id={clipId}>
              <rect x="1.5" y="1.5" width="21" height="21" rx="4.5" />
            </clipPath>
          </defs>
          <rect
            x="1.5"
            y="1.5"
            width="21"
            height="21"
            rx="4.5"
            fill={color}
            fillOpacity="0.12"
          />
          <g clipPath={`url(#${clipId})`}>
            <g transform="translate(1.5 22.5) scale(0.0041015625 -0.0041015625)">
              <path d={HERDR_PATH} fill={color} />
            </g>
          </g>
        </>
      );

    case "orca":
      // Sleek geometric Orca whale silhouette with signature dorsal fin and eye patch.
      return (
        <>
          <path
            d="M3.2 15.8c.6-4.2 3.8-7.4 8.2-8.2.5-1.8 1.5-3.4 2.7-4.5.3-.3.8-.1.7.4-.3 1.4-.2 2.8.4 3.9 2.9.6 5.1 2.4 5.8 5.2.4 1.5-.3 3.1-1.7 3.7-1.7.7-3.8.5-5.6-.2-1.8-.7-3.7-1-5.5-.4-1.3.4-2.5 1.3-3.4 2.3-.4.4-1.1.1-1.1-.5-.1-.6-.6-1-.5-1.7Z"
            fill={color}
          />
          <ellipse cx="16.2" cy="11.4" rx="1.65" ry="1.05" transform="rotate(-16 16.2 11.4)" fill="var(--paper, #ffffff)" />
          <path
            d="M11.6 14.3c1.6.3 3.1.8 4.4 1.6-1.4.4-3 .3-4.6-.3-.6-.2-.5-1.4.2-1.3Z"
            fill="var(--paper, #ffffff)"
            fillOpacity="0.88"
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
  const clipId = useId();
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
  const label = id === "t3code" || id === "magpie" || id === "herdr" || id === "orca"
    ? displayName
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
        {renderBrandMark(id, accent, clipId)}
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
