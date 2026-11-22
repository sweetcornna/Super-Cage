import test from "node:test";
import assert from "node:assert/strict";
import {
  defaultAppearance,
  normalizeAppearance,
  themeVariables,
  terminalTheme,
  themeDocument,
  parseThemeDocument,
  sameTheme,
  colorGroups,
  terminalColorGroups,
} from "./appearance-state.mjs";

test("element overrides stay independent, reset returns to derived colors, and terminal follows live values", () => {
  const appearance = normalizeAppearance(structuredClone(defaultAppearance));
  const theme = appearance.lightTheme;
  const before = themeVariables(theme, "light");
  theme.colors = {
    chrome: "#123456",
    button: "#654321",
    buttonInk: "#abcdef",
    selection: "#aabbcc",
  };
  theme.terminal = { background: "#112233", red: "#fedcba", cursor: "#445566" };
  theme.surface = "#fafafa";
  const vars = themeVariables(theme, "light");
  assert.equal(vars["--chrome"], "#123456");
  assert.equal(vars["--button-ink"], "#abcdef");
  assert.equal(vars["--paper"], "#fafafa");
  assert.equal(vars["--border"], vars["--line"]);
  assert.notEqual(vars["--line"], before["--line"]);
  const terminal = terminalTheme(theme, "light");
  assert.equal(terminal.background, "#112233");
  assert.equal(terminal.red, "#fedcba");
  assert.equal(terminal.selectionBackground, "#aabbcc");
  delete theme.colors.chrome;
  assert.notEqual(themeVariables(theme, "light")["--chrome"], "#123456");
});

test("theme document round trips both variants and all editable colors, without device preferences or library", () => {
  const appearance = normalizeAppearance(structuredClone(defaultAppearance));
  appearance.lightTheme.colors = Object.fromEntries(
    colorGroups.flatMap((group) =>
      group.entries.map(([key]) => [key, "#123456"]),
    ),
  );
  appearance.darkTheme.terminal = Object.fromEntries(
    terminalColorGroups.flatMap((group) =>
      group.entries.map(([key]) => [key, "#abcdef"]),
    ),
  );
  appearance.darkTheme.fonts.code = "Menlo";
  const document = themeDocument("  自定义主题  ", appearance);
  const imported = parseThemeDocument(JSON.stringify(document));
  assert.equal(imported.name, "自定义主题");
  assert.ok(sameTheme(appearance, imported));
  assert.deepEqual(Object.keys(imported).sort(), [
    "darkTheme",
    "format",
    "lightTheme",
    "name",
    "version",
  ]);
  const copy = structuredClone(imported);
  copy.lightTheme.colors = Object.fromEntries(
    Object.entries(copy.lightTheme.colors).reverse(),
  );
  assert.ok(sameTheme(copy, imported));
  copy.darkTheme.terminal.red = "#000000";
  assert.equal(sameTheme(copy, imported), false);
});

test("invalid theme files are rejected before any mutation", () => {
  const valid = themeDocument("主题", defaultAppearance);
  const reject = (mutate) => {
    const data = structuredClone(valid);
    mutate(data);
    assert.throws(() => parseThemeDocument(JSON.stringify(data)));
  };
  for (const input of [
    "not json",
    "null",
    "[]",
    "{}",
    " ".repeat(256 * 1024 + 1),
  ])
    assert.throws(() => parseThemeDocument(input));
  reject((data) => {
    data.version = 2;
  });
  reject((data) => {
    data.name = "  ";
  });
  reject((data) => {
    delete data.darkTheme;
  });
  reject((data) => {
    data.lightTheme.accent = "#abc";
  });
  reject((data) => {
    data.lightTheme.colors = { chrome: "red" };
  });
  reject((data) => {
    data.lightTheme.colors = { unknown: "#abcdef" };
  });
  reject((data) => {
    data.lightTheme.terminal = { foreground: "url(https://example.com)" };
  });
  reject((data) => {
    data.lightTheme.fonts.code = "Menlo; color:red";
  });
  reject((data) => {
    data.lightTheme.contrast = 10.5;
  });
  reject((data) => {
    data.mode = "dark";
  });
  assert.equal(defaultAppearance.lightTheme.accent, "#d97757");
});

test("all built-in palettes produce valid, independently editable theme documents", async () => {
  const { themeCatalog, presets, presetNames, presetGroups, applyThemePreset } =
    await import("./appearance-presets.mjs");
  assert.equal(
    new Set(themeCatalog.map((item) => item.id)).size,
    themeCatalog.length,
  );
  assert.ok(Object.keys(presets.light).length >= 10);
  assert.ok(Object.keys(presets.dark).length >= 12);
  for (const item of themeCatalog) {
    assert.ok(presetGroups.includes(item.group));
    assert.equal(presetNames[item.id], item.name);
    for (const variant of ["light", "dark"]) {
      if (!item[variant]) continue;
      const current = structuredClone(defaultAppearance[variant + "Theme"]);
      current.colors = { danger: "#123456", toolbar: "#123456" };
      current.terminal = { foreground: "#123456", brightRed: "#123456" };
      current.fonts.code = "Menlo";
      const applied = applyThemePreset(current, variant, item.id);
      const appearance = normalizeAppearance({ [variant + "Theme"]: applied });
      const imported = parseThemeDocument(
        JSON.stringify(themeDocument(item.name, appearance)),
      );
      assert.ok(sameTheme(appearance, imported), item.id);
      assert.equal(applied.fonts.code, "Menlo");
      assert.notEqual(applied.colors.danger, "#123456");
      assert.notEqual(applied.terminal.foreground, "#123456");
      assert.ok(
        Object.values(themeVariables(applied, variant)).every(
          (value) => !String(value).includes("NaN"),
        ),
      );
      applied.colors.line = "#123456";
      applied.terminal.red = "#123456";
      assert.notEqual(presets[variant][item.id].colors.line, "#123456");
      assert.notEqual(presets[variant][item.id].terminal.red, "#123456");
    }
  }
});

function contrastRatio(left, right) {
  const luminance = (color) => {
    const [r, g, b] = [1, 3, 5]
      .map((index) => parseInt(color.slice(index, index + 2), 16) / 255)
      .map((value) =>
        value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4,
      );
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  const a = luminance(left),
    b = luminance(right);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

test("accessible presets keep text, state colors, focus, borders and all ANSI text legible", async () => {
  const { applyThemePreset } = await import("./appearance-presets.mjs");
  for (const id of ["color-vision", "high-contrast"])
    for (const variant of ["light", "dark"]) {
      const theme = applyThemePreset(
        defaultAppearance[variant + "Theme"],
        variant,
        id,
      );
      const vars = themeVariables(theme, variant),
        terminal = terminalTheme(theme, variant);
      const threshold = id === "high-contrast" ? 7 : 4.5;
      const check = (foreground, background, minimum, name) =>
        assert.ok(
          contrastRatio(foreground, background) >= minimum,
          `${id}/${variant}/${name}: ${contrastRatio(foreground, background).toFixed(3)} < ${minimum}`,
        );
      for (const key of [
        "--ink",
        "--strong",
        "--muted",
        "--link",
        "--success",
        "--danger",
        "--warning",
        "--inactive",
      ]) {
        for (const background of [
          "--paper",
          "--chrome",
          "--chrome-dark",
          "--hover",
          "--selected",
          "--input",
        ]) {
          // State messages and links are shown on paper/panel surfaces, not selection rows.
          if (
            ["--hover", "--selected"].includes(background) &&
            !["--ink", "--strong"].includes(key)
          )
            continue;
          check(
            vars[key],
            vars[background],
            threshold,
            `${key} on ${background}`,
          );
        }
      }
      check(vars["--sidebar-ink"], vars["--sidebar"], threshold, "sidebar");
      check(vars["--toolbar-ink"], vars["--toolbar"], threshold, "toolbar");
      check(vars["--button-ink"], vars["--button"], threshold, "button");
      for (const background of ["--paper", "--chrome", "--input"]) {
        check(vars["--line"], vars[background], 3, "border");
        check(vars["--focus"], vars[background], 3, "focus");
      }
      for (const group of terminalColorGroups)
        for (const [key] of group.entries) {
          if (
            ["background", "selectionBackground", "cursorAccent"].includes(key)
          )
            continue;
          check(
            terminal[key],
            terminal.background,
            threshold,
            `terminal ${key}`,
          );
        }
      assert.equal(theme.opaqueWindows, true);
      assert.notEqual(vars["--success"], vars["--danger"]);
      check(
        terminal.foreground,
        terminal.selectionBackground,
        threshold,
        "terminal selection",
      );
      check(terminal.cursorAccent, terminal.cursor, threshold, "cursor text");
    }
});


// brand/design-language.md §1: the Super Cage theme, token by token.
const designTokens = {
  light: {
    "--paper": "#faf9f5", "--chrome": "#f3f1ea", "--surface": "#ffffff", "--soft": "#f0eee6",
    "--selected": "#ece8dd", "--line": "#e6e3d8", "--line-strong": "#d6d2c4", "--ink": "#1f1e1d",
    "--muted": "#6b655b", "--faint": "#9a9488", "--clay": "#d97757", "--clay-ink": "#a4482a",
    "--clay-soft": "#f6e5dc", "--green": "#2e7d4f", "--amber": "#9a6700", "--error": "#b3261e",
  },
  dark: {
    "--paper": "#1f1e1d", "--chrome": "#1a1918", "--surface": "#2a2926", "--soft": "#34322e",
    "--selected": "#3a3733", "--line": "#3d3a35", "--line-strong": "#4a4640", "--ink": "#f3efe5",
    "--muted": "#aba597", "--faint": "#7e786c", "--clay": "#d97757", "--clay-ink": "#e8936f",
    "--clay-soft": "#4a3329", "--green": "#7bc79a", "--amber": "#e3b341", "--error": "#f2b8b5",
  },
};

test("Super Cage is the default and produces exactly the design tokens", async () => {
  const { readFile } = await import("node:fs/promises");
  const css = await readFile(new URL("./tokens.css", import.meta.url), "utf8");
  const block = (selector) => {
    const start = css.indexOf(selector + " {");
    assert.ok(start >= 0, selector);
    const body = css.slice(start, css.indexOf("}", start));
    return Object.fromEntries([...body.matchAll(/(--[a-z-]+):\s*(#[0-9a-f]{6});/g)].map((m) => [m[1], m[2]]));
  };
  const statics = { light: block(":root"), dark: block(':root[data-theme="dark"]') };
  for (const variant of ["light", "dark"]) {
    const theme = defaultAppearance[variant + "Theme"];
    assert.equal(theme.preset, "super-cage");
    assert.equal(theme.opaqueWindows, true);
    const vars = themeVariables(theme, variant);
    for (const [token, value] of Object.entries(designTokens[variant])) {
      assert.equal(vars[token], value, `${variant} ${token} from the preset`);
      assert.equal(statics[variant][token], value, `${variant} ${token} in tokens.css`);
    }
    const terminal = terminalTheme(theme, variant);
    assert.equal(terminal.background, vars["--paper"]);
    assert.equal(terminal.foreground, vars["--ink"]);
  }
});

test("every preset fills the whole design token set", async () => {
  const { themeCatalog, applyThemePreset } = await import("./appearance-presets.mjs");
  for (const item of themeCatalog)
    for (const variant of ["light", "dark"]) {
      if (!item[variant]) continue;
      const theme = applyThemePreset(defaultAppearance[variant + "Theme"], variant, item.id);
      const vars = themeVariables(theme, variant);
      for (const token of Object.keys(designTokens[variant]))
        assert.match(vars[token], /^#[0-9a-f]{6}$/, `${item.id}/${variant} ${token}`);
    }
});

test("Super Cage text, focus and terminal colours stay legible", () => {
  for (const variant of ["light", "dark"]) {
    const theme = defaultAppearance[variant + "Theme"];
    const vars = themeVariables(theme, variant);
    const terminal = terminalTheme(theme, variant);
    const check = (fg, bg, minimum, name) =>
      assert.ok(contrastRatio(fg, bg) >= minimum, `${variant}/${name}: ${contrastRatio(fg, bg).toFixed(2)} < ${minimum}`);
    for (const key of ["--ink", "--muted", "--clay-ink", "--green", "--amber", "--error"])
      for (const background of ["--paper", "--chrome", "--surface"])
        // State colours are text on paper and cards; on chrome they only
        // colour icons and dots (brand/design-language.md §1).
        check(vars[key], vars[background], background === "--chrome" && ["--green", "--amber"].includes(key) ? 3 : 4.5, `${key} on ${background}`);
    check(vars["--ink"], vars["--selected"], 4.5, "ink on selected");
    check(vars["--button-ink"], vars["--button"], 4.5, "primary button");
    for (const background of ["--paper", "--surface"])
      check(vars["--focus"], vars[background], 3, "focus ring");
    for (const group of terminalColorGroups)
      for (const [key] of group.entries) {
        if (["background", "selectionBackground", "cursorAccent", "cursor"].includes(key)) continue;
        check(terminal[key], terminal.background, 4.5, "terminal " + key);
      }
    check(terminal.foreground, terminal.selectionBackground, 4.5, "terminal selection");
    check(terminal.cursor, terminal.background, 3, "terminal cursor");
  }
});

test("a saved theme keeps its own colour overrides", () => {
  const saved = normalizeAppearance({
    lightTheme: { preset: "codex", accent: "#339cff", surface: "#ffffff", ink: "#1a1c1f", contrast: 45, opaqueWindows: false, fonts: { ui: "", code: "" } },
  });
  assert.deepEqual(saved.lightTheme.colors, {});
  assert.deepEqual(saved.lightTheme.terminal, {});
  assert.equal(saved.darkTheme.preset, "super-cage");
  assert.deepEqual(saved.darkTheme.colors, defaultAppearance.darkTheme.colors);
});

test("the Go default mirrors the Super Cage preset", async () => {
  const { readFile } = await import("node:fs/promises");
  const go = await readFile(new URL("../../internal/workbench/appearance.go", import.meta.url), "utf8");
  const body = go.slice(go.indexOf("func superCageTheme"), go.indexOf("func codexTheme"));
  const [darkPart, lightPart] = body.split("\n\treturn AppearanceTheme{");
  const parse = (part) => {
    const map = (name) => Object.fromEntries([...part.slice(part.search(new RegExp(name + ":\\s+map"))).split("}")[0]
      .matchAll(/"([A-Za-z]+)": "(#[0-9a-f]{6})"/g)].map((m) => [m[1], m[2]]));
    const field = (name) => part.match(new RegExp(name + ': "(#[0-9a-f]{6})"'))[1];
    return { accent: field("Accent"), surface: field("Surface"), ink: field("Ink"), colors: map("Colors"), terminal: map("Terminal") };
  };
  for (const [variant, part] of [["dark", darkPart], ["light", lightPart]]) {
    const theme = defaultAppearance[variant + "Theme"];
    const go = parse(part);
    assert.deepEqual(go, { accent: theme.accent, surface: theme.surface, ink: theme.ink, colors: theme.colors, terminal: theme.terminal }, variant);
  }
});
