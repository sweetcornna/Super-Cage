package workbench

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

type AppearanceFonts struct {
	UI   string `json:"ui" toml:"ui"`
	Code string `json:"code" toml:"code"`
}
type AppearanceTheme struct {
	Preset        string            `json:"preset"`
	Accent        string            `json:"accent" toml:"accent"`
	Surface       string            `json:"surface" toml:"surface"`
	Ink           string            `json:"ink" toml:"ink"`
	Contrast      int               `json:"contrast" toml:"contrast"`
	OpaqueWindows bool              `json:"opaqueWindows" toml:"opaqueWindows"`
	Fonts         AppearanceFonts   `json:"fonts" toml:"fonts"`
	Colors        map[string]string `json:"colors,omitempty"`
	Terminal      map[string]string `json:"terminal,omitempty"`
}
type SavedAppearanceTheme struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Light AppearanceTheme `json:"lightTheme"`
	Dark  AppearanceTheme `json:"darkTheme"`
}
type Appearance struct {
	CustomThemes   []SavedAppearanceTheme `json:"customThemes,omitempty"`
	Mode           string                 `json:"mode"`
	Light          AppearanceTheme        `json:"lightTheme"`
	Dark           AppearanceTheme        `json:"darkTheme"`
	UIFontSize     int                    `json:"uiFontSize"`
	CodeFontSize   int                    `json:"codeFontSize"`
	PointerCursors bool                   `json:"pointerCursors"`
	FontSmoothing  bool                   `json:"fontSmoothing"`
	ReducedMotion  string                 `json:"reducedMotion"`
}

// superCageTheme mirrors the "super-cage" preset in ui/src/appearance-presets.mjs
// (the token table in brand/design-language.md). It is the default for
// profiles that have never saved an appearance.
func superCageTheme(dark bool) AppearanceTheme {
	if dark {
		return AppearanceTheme{Preset: "super-cage", Accent: "#d97757", Surface: "#1f1e1d", Ink: "#f3efe5", Contrast: 60, OpaqueWindows: true,
			Colors:   map[string]string{"chrome": "#1a1918", "strong": "#f3efe5", "muted": "#aba597", "faint": "#7e786c", "line": "#3d3a35", "lineStrong": "#4a4640", "input": "#2a2926", "hover": "#34322e", "selected": "#3a3733", "selection": "#5c3d30", "button": "#f3efe5", "buttonInk": "#1f1e1d", "accentInk": "#ffffff", "accentSoft": "#4a3329", "link": "#e8936f", "focus": "#e8936f", "success": "#7bc79a", "warning": "#e3b341", "danger": "#f2b8b5", "inactive": "#7e786c"},
			Terminal: map[string]string{"background": "#1f1e1d", "foreground": "#f3efe5", "cursor": "#d97757", "cursorAccent": "#1f1e1d", "selectionBackground": "#5c3d30", "black": "#8f897d", "red": "#f28b7d", "green": "#7bc79a", "yellow": "#e3b341", "blue": "#8fb3e0", "magenta": "#d4a2cf", "cyan": "#7cc6bf", "white": "#e8e2d4", "brightBlack": "#aba597", "brightRed": "#f7a99d", "brightGreen": "#9bd8b2", "brightYellow": "#f0c869", "brightBlue": "#aecaf0", "brightMagenta": "#e2bde0", "brightCyan": "#a0d9d3", "brightWhite": "#faf9f5"}}
	}
	return AppearanceTheme{Preset: "super-cage", Accent: "#d97757", Surface: "#faf9f5", Ink: "#1f1e1d", Contrast: 45, OpaqueWindows: true,
		Colors:   map[string]string{"chrome": "#f3f1ea", "strong": "#1f1e1d", "muted": "#6b655b", "faint": "#9a9488", "line": "#e6e3d8", "lineStrong": "#d6d2c4", "input": "#ffffff", "hover": "#f0eee6", "selected": "#ece8dd", "selection": "#f0d5c7", "button": "#1f1e1d", "buttonInk": "#faf9f5", "accentInk": "#ffffff", "accentSoft": "#f6e5dc", "link": "#a4482a", "focus": "#a4482a", "success": "#2e7d4f", "warning": "#9a6700", "danger": "#b3261e", "inactive": "#9a9488"},
		Terminal: map[string]string{"background": "#faf9f5", "foreground": "#1f1e1d", "cursor": "#c96442", "cursorAccent": "#faf9f5", "selectionBackground": "#f0d5c7", "black": "#1f1e1d", "red": "#b3261e", "green": "#2e7d4f", "yellow": "#8a5d00", "blue": "#365f8c", "magenta": "#86467f", "cyan": "#22706d", "white": "#6b655b", "brightBlack": "#6b655b", "brightRed": "#a4482a", "brightGreen": "#2a6e46", "brightYellow": "#7a5300", "brightBlue": "#2f5680", "brightMagenta": "#773d71", "brightCyan": "#1d6461", "brightWhite": "#3d3a35"}}
}

// codexTheme is the "codex" preset, the default before Super Cage. Importing
// from the local Codex app starts from it.
func codexTheme(dark bool) AppearanceTheme {
	if dark {
		return AppearanceTheme{Preset: "codex", Accent: "#339cff", Surface: "#181818", Ink: "#ffffff", Contrast: 60}
	}
	return AppearanceTheme{Preset: "codex", Accent: "#339cff", Surface: "#ffffff", Ink: "#1a1c1f", Contrast: 45}
}

func defaultAppearance() Appearance {
	return Appearance{Mode: "system", Light: superCageTheme(false), Dark: superCageTheme(true), UIFontSize: 14, CodeFontSize: 13, FontSmoothing: true, ReducedMotion: "system"}
}

func (a Appearance) validate() error {
	if a.Mode != "system" && a.Mode != "light" && a.Mode != "dark" {
		return errors.New("请选择跟随系统、浅色或深色外观")
	}
	if a.UIFontSize < 12 || a.UIFontSize > 24 || a.CodeFontSize < 10 || a.CodeFontSize > 24 {
		return errors.New("界面字号范围为 12–24，代码字号范围为 10–24")
	}
	if a.ReducedMotion != "system" && a.ReducedMotion != "on" && a.ReducedMotion != "off" {
		return errors.New("减少动态效果选项无效")
	}
	if err := validateAppearanceTheme(a.Light); err != nil {
		return err
	}
	if err := validateAppearanceTheme(a.Dark); err != nil {
		return err
	}
	if len(a.CustomThemes) > 50 {
		return errors.New("最多保存 50 个自定义主题，请先删除不需要的主题")
	}
	ids := make(map[string]bool)
	for _, saved := range a.CustomThemes {
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`).MatchString(saved.ID) || ids[saved.ID] {
			return errors.New("自定义主题标识无效或重复")
		}
		ids[saved.ID] = true
		if strings.TrimSpace(saved.Name) == "" || utf8.RuneCountInString(saved.Name) > 80 {
			return errors.New("请输入 1–80 个字符的主题名称")
		}
		if err := validateAppearanceTheme(saved.Light); err != nil {
			return err
		}
		if err := validateAppearanceTheme(saved.Dark); err != nil {
			return err
		}
	}
	return nil
}

var appearanceColorKeys = strings.Fields("sidebar sidebarInk toolbar toolbarInk chrome chromeDark strong muted faint line lineStrong input button buttonInk accentInk accentSoft link focus hover selected selection success warning danger inactive")
var terminalColorKeys = strings.Fields("background foreground cursor cursorAccent selectionBackground black red green yellow blue magenta cyan white brightBlack brightRed brightGreen brightYellow brightBlue brightMagenta brightCyan brightWhite")
var appearanceColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validateAppearanceTheme(theme AppearanceTheme) error {
	if !appearanceColor.MatchString(theme.Accent) || !appearanceColor.MatchString(theme.Surface) || !appearanceColor.MatchString(theme.Ink) {
		return errors.New("颜色须为六位十六进制值，例如 #fdf6e3")
	}
	if theme.Contrast < 0 || theme.Contrast > 100 {
		return errors.New("对比度范围为 0–100")
	}
	if utf8.RuneCountInString(theme.Preset) > 80 {
		return errors.New("主题名称过长")
	}
	for _, font := range []string{theme.Fonts.UI, theme.Fonts.Code} {
		if utf8.RuneCountInString(font) > 200 || strings.ContainsAny(font, ";{}<>\r\n") {
			return errors.New("字体名称无效")
		}
	}
	for field, colors := range map[string]map[string]string{"colors": theme.Colors, "terminal": theme.Terminal} {
		allowed := appearanceColorKeys
		if field == "terminal" {
			allowed = terminalColorKeys
		}
		for key, value := range colors {
			found := false
			for _, candidate := range allowed {
				if key == candidate {
					found = true
					break
				}
			}
			if !found || !appearanceColor.MatchString(value) {
				return errors.New("元素颜色无效或包含不支持的颜色项目")
			}
		}
	}
	return nil
}

func (s *Server) getAppearance(w http.ResponseWriter, r *http.Request) {
	a := defaultAppearance()
	b, err := os.ReadFile(filepath.Join(s.profile, "appearance.json"))
	if err == nil {
		// Saved files omit empty colour maps. Decoding one over the default's
		// Super Cage overrides would graft them onto a theme saved without any
		// (every file written before Super Cage became the default).
		a.Light.Colors, a.Light.Terminal, a.Dark.Colors, a.Dark.Terminal = nil, nil, nil, nil
		err = json.Unmarshal(b, &a)
	}
	if err != nil && !os.IsNotExist(err) {
		requestError(w, errors.New("无法读取已保存的外观设置"))
		return
	}
	if err = a.validate(); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, a)
}

func (s *Server) saveAppearance(w http.ResponseWriter, r *http.Request) {
	var a Appearance
	if !readJSON(w, r, &a, 512<<10) {
		return
	}
	if err := a.validate(); err != nil {
		requestError(w, err)
		return
	}
	// Appearance has no VM or network side effects and uses its own atomic file.
	if err := writePrivate(filepath.Join(s.profile, "appearance.json"), a); err != nil {
		requestError(w, errors.New("外观设置未能保存，请重试"))
		return
	}
	jsonReply(w, a)
}

func readCodexAppearance(configPath string) (Appearance, error) {
	a := defaultAppearance()
	a.Light, a.Dark = codexTheme(false), codexTheme(true)
	a.CodeFontSize = 12
	var config struct {
		Desktop struct {
			Mode           string           `toml:"appearanceTheme"`
			Light          *AppearanceTheme `toml:"appearanceLightChromeTheme"`
			Dark           *AppearanceTheme `toml:"appearanceDarkChromeTheme"`
			LightPreset    string           `toml:"appearanceLightCodeThemeId"`
			DarkPreset     string           `toml:"appearanceDarkCodeThemeId"`
			UIFontSize     int              `toml:"sansFontSize"`
			CodeFontSize   int              `toml:"codeFontSize"`
			PointerCursors bool             `toml:"usePointerCursors"`
			FontSmoothing  *bool            `toml:"useFontSmoothing"`
		} `toml:"desktop"`
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return a, errors.New("未找到本机 Codex 配置")
	}
	if err = toml.Unmarshal(b, &config); err != nil {
		return a, errors.New("无法解析本机 Codex 配置")
	}
	d := config.Desktop
	if d.Mode != "" {
		a.Mode = d.Mode
	}
	if d.Light != nil {
		a.Light = *d.Light
	}
	if d.Dark != nil {
		a.Dark = *d.Dark
	}
	a.Light.Preset = d.LightPreset
	if a.Light.Preset == "" {
		a.Light.Preset = "codex"
	}
	a.Dark.Preset = d.DarkPreset
	if a.Dark.Preset == "" {
		a.Dark.Preset = "codex"
	}
	if d.UIFontSize != 0 {
		a.UIFontSize = d.UIFontSize
	}
	if d.CodeFontSize != 0 {
		a.CodeFontSize = d.CodeFontSize
	}
	a.PointerCursors = d.PointerCursors
	if d.FontSmoothing != nil {
		a.FontSmoothing = *d.FontSmoothing
	}
	// This preference is stored separately by Codex; no other state is returned.
	var global struct {
		ReducedMotion string `json:"reduced-motion-preference"`
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), ".codex-global-state.json")); err == nil && json.Unmarshal(b, &global) == nil && global.ReducedMotion != "" {
		a.ReducedMotion = global.ReducedMotion
	}
	return a, a.validate()
}

func (s *Server) codexAppearance(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		requestError(w, errors.New("无法定位本机用户目录"))
		return
	}
	a, err := readCodexAppearance(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, a)
}
