// Package localterm opens a window of the owner's own terminal application
// that runs "cage attach": Ghostty when it is installed on this computer,
// the system Terminal otherwise, with no choice to make. It also keeps the
// per-computer switches of the "本机终端" feature. Nothing here launches
// anything by itself: launching goes through a Runner the caller supplies,
// so tests never open a window.
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

// Settings are the per-computer choices. A file written by an earlier build
// may still hold the terminal application that was chosen then: it is
// ignored and is not written again.
type Settings struct {
	// ImageUpload uploads image files dropped or pasted into a local window
	// as Cage's web terminal does, and pastes the environment's paths
	// instead of this computer's.
	ImageUpload bool `json:"image_upload"`
	// ClipboardWrite lets programs in the environment set this computer's
	// clipboard (OSC 52), as Cage's web terminal does. Reads are never allowed.
	ClipboardWrite bool `json:"clipboard_write"`
}

// UnreadableSettings are what a window uses when the settings cannot be
// read: the defaults with clipboard writes off. A choice that cannot be read
// is not taken as permission.
func UnreadableSettings() Settings {
	settings := DefaultSettings()
	settings.ClipboardWrite = false
	return settings
}

// DefaultSettings follow Cage's web terminal: dropped images upload and
// clipboard writes pass.
func DefaultSettings() Settings { return Settings{ImageUpload: true, ClipboardWrite: true} }

// LoadSettings reads the choices kept in dir; a missing file gives the
// defaults, one that cannot be read or parsed UnreadableSettings and an
// error.
func LoadSettings(dir string) (Settings, error) {
	settings := DefaultSettings()
	data, err := os.ReadFile(filepath.Join(dir, SettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return UnreadableSettings(), err
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &settings) != nil {
		return UnreadableSettings(), errors.New("本机终端设置无法读取：已关闭剪贴板写入，其余使用默认设置")
	}
	return settings, nil
}
