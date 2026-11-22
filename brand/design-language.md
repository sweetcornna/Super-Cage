# Super Cage design language (desktop workbench + server portal)

The phone app (ui/src/mobile-cli, see super.css and mobile.css) already has this look. Desktop and portal must feel like the same product: **ivory paper, dark warm ink, one clay accent, serif display type, a few hand-drawn drawings**. The reference in spirit is Claude's own apps: calm, warm, generous, typographic, never flashy. "Premium" here means restraint and craft: precise spacing, consistent radii, excellent type, quiet colour, and one or two human touches per screen. It never means gradients, glass, glow, heavy shadows or decoration everywhere.

## 1. Colour tokens

| token | light | dark | use |
|---|---|---|---|
| --paper | #FAF9F5 | #1F1E1D | app background, main canvas |
| --chrome | #F3F1EA | #1A1918 | sidebar, status bar, window chrome (one step from paper) |
| --surface | #FFFFFF | #2A2926 | cards, inputs, menus, dialogs |
| --soft | #F0EEE6 | #34322E | hover, quiet fills, code chips |
| --selected | #ECE8DD | #3A3733 | selected row / active nav |
| --line | #E6E3D8 | #3D3A35 | hairlines, borders |
| --line-strong | #D6D2C4 | #4A4640 | input borders, dividers that must read |
| --ink | #1F1E1D | #F3EFE5 | text, primary buttons |
| --muted | #6B655B | #ABA597 | secondary text |
| --faint | #9A9488 | #7E786C | placeholders, disabled |
| --clay | #D97757 | #D97757 | the accent: primary call to action, live dots, drawings |
| --clay-ink | #A4482A | #E8936F | accent text, links, focus ring |
| --clay-soft | #F6E5DC | #4A3329 | accent tint backgrounds |
| --green | #2E7D4F | #7BC79A | success / running |
| --amber | #9A6700 | #E3B341 | warning |
| --error | #B3261E | #F2B8B5 | errors, destructive |

Rules: text contrast at least 4.5:1 (muted on paper passes; faint is only for non-essential text). Clay is rare: at most one clay-filled control per view; everything else is ink or neutral. No pure black, no pure grey: every neutral is warm. Shadows: `0 1px 2px #1f1e1d0a, 0 8px 28px #1f1e1d10` for floating layers only (menus, dialogs, toasts); cards sit flat with a hairline.

Measured exceptions, kept on purpose: --green and --amber on --chrome are 4.46 and 4.31:1, so on chrome they colour icons and dots only, never text; white on --clay is 3.1:1, accepted for the single bold call to action. Selection (the current theme card, a chosen option) is outlined in ink, never in the accent.

Terminal palette (part of the theme): every ANSI colour and its bright variant at least 4.5:1 on the terminal background in both variants; bold may draw either, so both must pass. Cursor at least 3:1 (light #C96442; #D97757 is 2.96:1 on paper). Dark ANSI black is #8F897D (4.79:1). Dim text is drawn by xterm at half opacity and is not held to 4.5. ui/src/appearance-state.test.mjs checks all of these, and internal/workbench/appearance.go mirrors the values.

These are the **Super Cage theme** (light and dark) and become the default preset of the existing appearance system. Other presets (Codex, Solarized, VS Code…) stay selectable and must still produce a coherent UI through the same tokens.

## 2. Type

- Sans (UI): `-apple-system, BlinkMacSystemFont, "Segoe UI Variable Text", "Segoe UI", "PingFang SC", "Microsoft YaHei UI", "Noto Sans CJK SC", sans-serif`.
- Serif (display only): `"New York", ui-serif, "Iowan Old Style", "Songti SC", "Noto Serif CJK SC", "Source Han Serif SC", Georgia, "Microsoft YaHei UI", "Noto Sans CJK SC", serif`. No web fonts, no CDN. On Windows CJK display text must fall to YaHei, never to SimSun: check the stack order produces that.
- Mono: `ui-monospace, "SF Mono", Menlo, "Cascadia Mono", Consolas, monospace`.
- Serif is for: the brand wordmark, page titles, the home greeting, dialog titles, empty-state headings, big numbers. Weight 500, slight positive tracking for CJK (.01–.02em), tight for Latin. Everything functional is sans.
- Scale (px): 12 caption, 13 secondary, 14 body (default UI size, follows the user's UI font size setting), 16 lead, 20 section title, 26 page title (serif), 34 hero greeting (serif). Line height 1.5 body, 1.25 titles.
- Numbers in tables and status: tabular-nums.

## 3. Shape, space, motion

- Spacing scale 4 / 8 / 12 / 16 / 24 / 32 / 48. Page content max-width 760 for forms and reading, 1080 for lists.
- Radii: 8 small controls and chips, 10 inputs and buttons, 14 cards and menus, 18 dialogs and hero cards, 999 pills. One screen uses at most three radii.
- Borders 1px hairline in --line; inputs use --line-strong; focus ring 2px --clay-ink with 2px offset.
- Controls: height 32 (compact, toolbars), 36 (default), 44 (hero call to action). Primary = ink fill with paper text; the single most important action on a page may be clay. Secondary = surface + hairline. Ghost = text only, soft hover. Destructive = --error text on a faint --error tint with an --error hairline (`button.danger`; `.danger-fill` is the same style): one style everywhere, never a solid red fill.
- Density: sidebar rows 32–34 high (34 on desktop), 13.5–14 px text; desktop is denser than phone but never cramped.
- The frame scales with the UI font size: type scale, control heights, sidebar width, header and status bar heights are rem off the UI font size (14 px default), not fixed px.
- Motion: 120–180 ms ease-out for hover/press/reveal; dialogs fade + 6 px rise. Respect reduced motion. Nothing bounces or loops, except a live "working" dot.

## 4. Hand-drawn layer (the human touch)

Drawings are single-weight ink line art with round caps and slightly imperfect geometry, with clay as the only fill (see ui/src/mobile-cli/doodles.jsx: laptop, server, terminal, folder, sun, moon, chat, squiggle, arrow). Use them where a person pauses, never where they work:
- the home greeting: the text first, the time-of-day sun/moon after the name, the squiggle under the text, so the greeting starts on the same left edge as the card below it,
- empty states (no projects, no sessions, no accounts, no downloads…): one drawing ~120 px + serif heading + one line + one action,
- first-run / start-environment / sign-in heroes,
- success and error full-page moments,
- a dashed clay "nudge" card for the one thing the user should do next.
Functional icons stay Lucide (1.75 stroke, 16–18 px, ink or muted). New drawings must match the existing set's stroke weight and wobble; draw them as inline SVG using currentColor and var(--clay).

On the desktop the drawings are in the mark's brush hand: ui/scripts/brush-art.mjs turns stroked centre lines (the phone doodles plus desktop-only key, plug, tray and cup) into filled thick–thin outlines and writes ui/src/drawings-art.mjs; ui/src/drawings.jsx renders them (Drawing, EmptyState). A new drawing is added there as an ordinary stroked SVG and the file is regenerated, never hand-edited. The phone keeps its own line doodles.

## 5. Brand

Logo "一笔笼": a birdcage drawn with an ink brush (thin upstroke, thick downstroke), with a knot loop on top, a pointed crown, four curved ribs, a hoop and a base, and a clay light with a soft glow behind the ribs. The masters live in this directory (`brand/`) and are generated by `brand/gen.mjs` + `brand/final.mjs`; every platform icon is generated from them by `brand/generate.mjs` (see `brand/README.md`).

- `brand/mark-themed.svg`: full mark for UI at about 40 px and larger. Ink follows `currentColor`, the light is `var(--clay)`, the glow `var(--clay-soft)`.
- `brand/mark-small-themed.svg`: simplified cut for 16–32 px.
- `brand/mark.svg`, `mark-on-dark.svg`, `mark-mono.svg`, `mark-small.svg`, `mark-small-on-dark.svg`: fixed-colour versions.
- `brand/tile-ink.svg` (64 px and larger) and `brand/tile-ink-small.svg` (48 px and smaller): the app icon, cream line on an ink tile.

The mark is made of filled paths, not strokes; include its path data once per page and reuse it. The desktop adds the two themed masters to the document once as <symbol>s (ui/src/brand.jsx; BrandMark uses the small cut at 32 px and below). Wordmark: "Super Cage" in the serif stack, weight 500. New drawings use the same hand: a brush line with thick–thin modulation, slightly imperfect, clay as the only fill. The old pixel-art logo and the phone's sparkle cage are retired.

## 6. Layout principles for desktop

- Shell: left sidebar on --chrome (brand at top, navigation, projects with their sessions, environment status + account at the bottom); main area on --paper; a slim status bar. Hairlines, not shadows, separate regions.
- Sidebar: the brand row is mark + word mark + collapse button only; the guest system is a small icon (its name in the tooltip) in the status line at the bottom, after 环境运行正常. Navigation is grouped with inset hairlines, no headings: work surfaces (工作台, 浏览器, Claude Desktop) · connections (Tailscale, 虚拟机, SSH) · accounts (Claude 账号管理, Codex 账号管理). A new entry joins a group; it does not add a level. A secondary shortcut on a row (the claude.ai icon on 浏览器) is grey until hovered or focused.
- Session rows (sidebar and home) start with a 16 px provider slot: the Claude mark or the Codex mark in muted ink, plain terminals the terminal icon; the account number is a small pill on the slot's corner. Live state is the clay dot at the row's end.
- Home is a welcome, not a wizard: serif greeting with the user's name, a calm environment card (state, one primary action), quick actions, recent sessions and projects. When the environment is stopped the same page explains the one next step. The card's heading is the environment state; stopped, it becomes the dashed clay nudge card holding the page's one clay button.
- Messages: toasts float above the status bar; the status bar carries persistent state only. Destructive confirms (stop environment, remove account, clear browser data) use that same destructive button for the confirm action.
- Settings: left section list on --chrome, content column max 760, section titles serif 20–26, rows separated by hairlines rather than boxed cards, save bar only when there are changes (network and region keep theirs: their proxy apply flow needs it).
- Management pages (accounts, Tailscale, later modules): a header bar with a serif 16 px title, then a content column on paper.
- Lists (accounts, hosts, downloads, sessions): rows with hairlines, a clear title line, muted meta line, right-aligned state chip and quiet actions.
- Dialogs: surface, radius 18, serif title, body 14, actions right-aligned, primary last.
- Terminal and browser panes keep maximum room; their headers are quiet and the terminal palette is warm (ivory paper / warm ink in light, #1F1E1D in dark) and consistent with the theme.

## 7. Quality bar (check before showing anything)

1. Squint test: one clear focal point per screen; nothing shouts.
2. Alignment: everything sits on the 4 px grid; left edges line up across sections.
3. No leftover of the old look: no cool greys, blue accents, default system buttons, mixed radii, or raw browser controls.
4. Both themes, at 1440×900 and 1000×700; populated data, long names, error and empty states.
5. Chinese and Latin text both look intentional (line heights, punctuation, mixed-script spacing).
6. Keyboard focus is always visible; hit targets at least 28 px on desktop.

## 8. Implementation notes (desktop workbench)

- Tokens: ui/src/tokens.css holds the values above as the paint before the appearance system starts; at runtime the active preset writes them (ui/src/appearance-state.mjs). --surface means card/input/menu; the window background is --paper. Older names (--accent, --focus, --strong, --hover, --input, --button, --border, --success, --danger, --warning, --sidebar, --toolbar…) are aliases kept for styles not yet converted; new code uses the names in §1.
- Cascade: base.css → components.css → shell.css are cascade layers (declared in tokens.css). Feature stylesheets are unlayered, so a module's own rules win without !important. desktop-overrides.css restyles the phone-shared tooltip and inline select for the desktop only.
- Default theme: profiles without a saved appearance get Super Cage. A saved appearance carries the preset it was saved with and has no version marker, so a saved "codex" cannot be told from a deliberate choice; saved files are left as they are.
