# Super Cage brand

One mark, drawn as ink-brush art ("一笔笼": a birdcage with a knot loop on top, ribs and a clay light inside).
Everything in this repo that shows the mark is generated from the masters in this directory.

## Masters (single source)

Plain filled `<path>` SVGs on a 256 x 256 canvas. Do not edit them by hand: `final.mjs` (centre lines, brush widths, colours)
and `gen.mjs` (the brush) draw them, and `generate.test.mjs` checks that running `final.mjs` reproduces every file byte for byte.

| File | Use |
|---|---|
| mark.svg | full mark, ink #1F1E1D on light, pale glow #F6E5DC behind the ribs; 40 px and up |
| mark-on-dark.svg | cream #F3EFE5 line, glow #4A3329 |
| mark-mono.svg | one colour, no glow (Android themed icon) |
| mark-themed.svg | `currentColor` + CSS variables `--clay`, `--clay-soft` for UI |
| mark-small.svg, mark-small-on-dark.svg, mark-small-themed.svg | simplified cut for 16-32 px (heavier line, bigger light, no glow) |
| tile-ink.svg, tile-ink-small.svg | app icon: ink tile (rx 57/256), full mark at scale .76 / small cut at scale .7 |

Colours: ink #1F1E1D, cream #F3EFE5, clay #D97757. Pick the cut by pixel size: up to 48 px the small cut, above that the full mark.

## Generator

```
CAGE_PLAYWRIGHT_MODULE=<playwright module> CAGE_TEST_BROWSER_PATH=<Chrome> node brand/generate.mjs
node brand/generate.mjs --check     # fail when a checked-in output is stale
node brand/generate.mjs --check-text # same for SVG/XML/JSON outputs only, no browser (mobile/build.sh runs it)
node --test brand/generate.test.mjs # browser-free checks
node brand/final.mjs                # redraw the masters (rarely needed)
```

Needs Node 22+ and a Chrome or Chromium reachable through Playwright (set `CAGE_PLAYWRIGHT_MODULE` and `CAGE_TEST_BROWSER_PATH`
as the repo's other browser scripts do). Without them the script stops with a message saying what is missing.
Each PNG is rendered natively at its own size (no downscaling from 1024), then re-encoded by the script itself
(RGBA, or RGB for the App Store icon) so the bytes are stable: running it twice changes nothing.
The PNG bytes depend on the Chrome build; on another machine `--check` can differ in anti-aliasing only.
`iconutil` (macOS) validates the iconset at the end; elsewhere that step is skipped.

The composition of the mark inside the tile (scale and vertical offset) is read from `tile-ink.svg` and `tile-ink-small.svg`;
the macOS tile (824 of 1024) and Android (72 of 108 dp) scale that same composition.

## Outputs

| Output | Notes |
|---|---|
| packaging/macos/Cage.iconset/*.png | 10 files, 824/1024 rounded tile with margin; small cut at 16 and 32 px. scripts/build-app.sh and build-client.sh run `iconutil` on it; no .icns is committed |
| packaging/windows/Cage.ico | PNG frames 16, 20, 24, 32, 40, 48 (small cut), 64, 128, 256; full-bleed rounded square |
| packaging/linux/icons/hicolor/ | PNG 16, 22, 24, 32, 48, 64, 128, 256, 512 and scalable SVG, installed by scripts/package-linux.sh |
| mobile/ios/.../AppIcon.appiconset/AppIcon.png | 1024 px opaque RGB square, no rounding |
| mobile/ios/.../Logo.imageset | in-app logo, light and dark appearance (flat SVG paths) |
| mobile/android/res/drawable/cage_icon_foreground.xml, cage_icon_monochrome.xml | adaptive icon layers inside the 66 dp safe zone; background is `cage_icon_background` (ink) in values/themes.xml |
| mobile/android/res/drawable/cage_logo.xml | in-app logo; ink and glow come from the theme (`?attr/cageInk`, `?attr/cageGlow`) |
| mobile/assets/brand/logo.svg | copy of mark.svg |
| ui/public | PNG logo (512, rounded), SVG favicon (small cut), favicon.ico (16/32/48), apple-touch-icon.png (180, opaque); ui/src/logo.png is the same 512 PNG |

The doodles in mobile/assets/brand/doodles are not part of this generator; they keep using `node mobile/scripts/brand-assets.mjs`.
