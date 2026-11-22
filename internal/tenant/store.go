package tenant

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	stopped        = "stopped"
	starting       = "starting"
	ready          = "ready"
	failed         = "error"
	startupMessage = "The workspace is unavailable. Open it again to retry."
)

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type userRecord struct {
	user
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
}

type vm struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	SystemID    string    `json:"system_id"`
	SystemLabel string    `json:"system_label"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	Message     string    `json:"message,omitempty"`
}

type vmRecord struct {
	vm
	Owner string `json:"owner"`
	Image string `json:"image"`
}

type database struct {
	Version int                   `json:"version"`
	Users   map[string]userRecord `json:"users"`
	VMs     map[string]vmRecord   `json:"vms"`
	// Devices are the computers enrolled for the cloud entry (devices.go).
	// Absent until the first enrolment, so a dark build leaves state.json
	// as it was.
	Devices map[string]deviceRecord `json:"devices,omitempty"`
}

type store struct {
	database
	dir     *os.File
	lock    *os.File
	blocked bool
}

func openStore(path, legacyImage string) (*store, error) {
	if path == "" {
		return nil, errors.New("a private state directory is required")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err != nil || !info.IsDir() || !privateFile(info) {
		dir.Close()
		return nil, errors.New("state directory must be private (0700)")
	}
	lock, err := os.OpenFile(filepath.Join(path, "lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		dir.Close()
		return nil, err
	}
	state := &store{dir: dir, lock: lock}
	if err := lockStore(lock); err != nil {
		state.close()
		return nil, errors.New("cannot acquire exclusive state directory lock")
	}
	if err := state.load(legacyImage); err != nil {
		state.close()
		return nil, err
	}
	return state, nil
}

func (s *store) load(legacyImage string) error {
	file, err := os.Open(filepath.Join(s.dir.Name(), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		s.database = database{Version: 2, Users: make(map[string]userRecord), VMs: make(map[string]vmRecord)}
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !privateFile(info) || info.Size() > 32<<20 {
		return errors.New("invalid state file permissions or size")
	}
	decoder := json.NewDecoder(file)
	if decoder.Decode(&s.database) != nil || decoder.Decode(new(any)) != io.EOF || (s.Version != 1 && s.Version != 2) || s.Users == nil || s.VMs == nil {
		return errors.New("invalid tenant state file")
	}
	owners := make(map[string]bool)
	for name, record := range s.Users {
		if name != record.Username || canonicalUsername(name) != name || !vmIDPattern.MatchString(record.ID) || owners[record.ID] || len(record.Salt) != 16 || len(record.Hash) != 32 {
			return errors.New("invalid account record in tenant state")
		}
		owners[record.ID] = true
	}
	legacy := s.Version == 1
	changed := legacy
	s.Version = 2
	for id, record := range s.VMs {
		if id != record.ID || !vmIDPattern.MatchString(id) || !owners[record.Owner] || !validVMName(record.Name) || record.CreatedAt.IsZero() {
			return errors.New("invalid VM record in tenant state")
		}
		if legacy {
			if legacyImage == "" {
				return errors.New("v1 VM records need the verified original Debian fingerprint (-legacy-debian-image)")
			}
			system := DefaultSystem(legacyImage)
			record.SystemID, record.SystemLabel, record.Image = system.ID, system.Label, system.Image
		}
		if !systemIDPattern.MatchString(record.SystemID) || !validSystemLabel(record.SystemLabel) || (record.Image != "" && !secretPattern.MatchString(record.Image)) || (record.Image == "" && record.SystemID != defaultSystemID) {
			return errors.New("invalid VM system in tenant state")
		}
		if record.Image == "" && legacyImage != "" {
			record.Image = legacyImage
			changed = true
		}
		switch record.State {
		case starting, ready, failed, stopped:
			record.State, record.Message = stopped, ""
		default:
			return errors.New("invalid VM state in tenant state")
		}
		s.VMs[id] = record
	}
	for id, record := range s.Devices {
		if !validDeviceRecord(id, record, owners) {
			return errors.New("invalid device record in tenant state")
		}
	}
	if changed {
		return s.save(s.database)
	}
	return nil
}

func (s *store) save(next database) error {
	if s.blocked {
		return errors.New("tenant state storage is unavailable")
	}
	file, err := os.CreateTemp(s.dir.Name(), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(next)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(s.dir.Name(), "state.json")); err != nil {
		return err
	}
	s.database = next
	if err := syncDirectory(s.dir); err != nil {
		s.blocked = true
		return err
	}
	return nil
}

func (s *store) close() {
	s.lock.Close()
	s.dir.Close()
}
