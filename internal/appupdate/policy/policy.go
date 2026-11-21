// Package policy decides when Super Cage may install an application update.
//
// Nothing here downloads or installs anything. The package holds the user's
// update mode, the record of attempts, the idle registry, the decision made
// under the workbench's operation lock, and the post-update chain record that
// survives the application being replaced. Every unknown reads as "do not
// install unattended". See design.md in this directory.
package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

// Mode is how the user wants updates to happen.
type Mode string

const (
	// Manual: nothing is downloaded or installed without a click. A check that
	// only learns that a newer version exists is allowed.
	Manual Mode = "manual"
	// Silent: the whole chain may run unattended while the machine is idle.
	Silent Mode = "silent"
)

// Trigger is who asked for an installation.
type Trigger string

const (
	Click        Trigger = "click"
	SilentUpdate Trigger = "silent"
)

const (
	// HoldPeriod is how long idle must hold, with an unchanged activity
	// fingerprint, before an unattended installation. It equals one tick of
	// the workbench's existing hourly ticker, so two idle ticks prove it.
	HoldPeriod = time.Hour
	// UserIdle is the minimum time without keyboard or mouse input on the host.
	UserIdle = 30 * time.Minute
	// ReleaseAge is the minimum age of a release installed unattended, so a
	// bad release can be withdrawn before idle machines take it.
	ReleaseAge = 24 * time.Hour
	// CheckEvery is how often silent mode asks for the newest release; it is
	// the same cadence as a visible window's check.
	CheckEvery = 6 * time.Hour
)

// Dir is the private update directory the native shells and "cage
// app-update" already share.
func Dir(profile string) string { return filepath.Join(profile, "updates") }

// Settings is the user's update preference. Its only writer is the
// workbench's policy route; profile restore and settings saves never touch it.
type Settings struct {
	Mode      Mode      `json:"mode"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == Manual || m == Silent }

// LoadSettings reads the preference. A missing file is the default, manual.
// An unreadable or invalid file is also manual, with the error returned so
// that a caller can report it; it never reads as silent.
func LoadSettings(profile string) (Settings, error) {
	data, err := readSmall(filepath.Join(Dir(profile), "policy.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Settings{Mode: Manual}, nil
	}
	if err != nil {
		return Settings{Mode: Manual}, err
	}
	var s Settings
	if json.Unmarshal(data, &s) != nil || !s.Mode.Valid() {
		return Settings{Mode: Manual}, errors.New("更新方式记录无效，已按手动更新处理")
	}
	return s, nil
}

// SaveSettings stores the preference privately and atomically.
func SaveSettings(profile string, s Settings) error {
	if !s.Mode.Valid() {
		return errors.New("更新方式无效")
	}
	return writePrivate(filepath.Join(Dir(profile), "policy.json"), s)
}

const maxRecord = 256 << 10

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxRecord {
		return nil, errors.New("更新记录不是有效的私有文件")
	}
	data := make([]byte, st.Size())
	if _, err := f.ReadAt(data, 0); err != nil && st.Size() > 0 {
		return nil, err
	}
	return data, nil
}

// writePrivate replaces path with v, readable only by the user, so that a
// crash leaves either the old or the new record and never a torn one.
func writePrivate(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxRecord {
		return errors.New("更新记录过大")
	}
	dir := filepath.Dir(path)
	if err = host.EnsurePrivateDir(dir); err != nil {
		return err
	}
	if st, e := os.Lstat(dir); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("更新目录必须是私有普通目录")
	}
	f, err := os.CreateTemp(dir, ".update-record-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = host.ProtectFile(f.Name()); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
