// Package localterm knows the owner's own terminal applications: which are
// installed on this computer, how to open a window in each that runs
// "cage attach", and the per-computer choices of the "本机终端" feature.
// Nothing here launches anything by itself: launching goes through a Runner
// the caller supplies, so tests never open a window.
package localterm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// SettingsFile holds this computer's choices, next to the desktop's local
// client configuration. It never travels to an environment or the portal.
const SettingsFile = "local-terminal.json"

// Settings are the per-computer choices.
type Settings struct {
	// App is the application "在本机终端中打开" uses; empty means Super
	// Cage's built-in terminal, where the menu item is hidden.
	App string `json:"app"`
	// ImageUpload uploads image files dropped or pasted into a local window
	// as Cage's web terminal does, and pastes the environment's paths
	// instead of this computer's.
	ImageUpload bool `json:"image_upload"`
	// ClipboardWrite lets programs in the environment set this computer's
	// clipboard (OSC 52), as Cage's web terminal does. Reads are never allowed.
	ClipboardWrite bool `json:"clipboard_write"`
}

// DefaultSettings follow Cage's web terminal: dropped images upload and
// clipboard writes pass.
func DefaultSettings() Settings { return Settings{ImageUpload: true, ClipboardWrite: true} }

// LoadSettings reads the choices kept in dir; a missing file gives the defaults.
func LoadSettings(dir string) (Settings, error) {
	settings := DefaultSettings()
	data, err := os.ReadFile(filepath.Join(dir, SettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &settings) != nil {
		return DefaultSettings(), errors.New("本机终端设置无法读取，已使用默认设置")
	}
	if settings.App != "" && Lookup(settings.App) == nil {
		settings.App = ""
	}
	return settings, nil
}

// Validate refuses an application this build does not know.
func (s Settings) Validate() error {
	if s.App != "" && Lookup(s.App) == nil {
		return errors.New("不支持这个本机终端应用")
	}
	return nil
}
