package controlclient

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Companion identifies the cage-attach program a controller found beside
// its own executable: where it is, the file it was, and its SHA-256.
//
// It answers one threat: a stale or foreign cage-attach (left from an
// earlier version, or from another installation or a development build)
// talking to this controller. cage agent proves that by hashing its own
// executable; cage-attach is a different program, so instead it must carry
// the controller's build stamp and be this very file. It is not a defence
// against other code of the same user, which can read the private control
// record anyway.
type Companion struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime_ns"`
	Inode   uint64 `json:"inode,omitempty"`
	SHA256  string `json:"sha256"`
}

// AttachProgram is the file name of the slim cage-attach program.
func AttachProgram(goos string) string {
	if goos == "windows" {
		return "cage-attach.exe"
	}
	return "cage-attach"
}

var companions struct {
	sync.Mutex
	byPath map[string]Companion
}

// CompanionOf describes the program at path, or returns nil when there is
// no regular file. The hash is computed once per file: it is kept while the
// path, size, modification time and inode stay the same.
func CompanionOf(path string) *Companion {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	key := Companion{Path: path, Size: info.Size(), ModTime: info.ModTime().UnixNano(), Inode: fileInode(info)}
	companions.Lock()
	defer companions.Unlock()
	if known, ok := companions.byPath[path]; ok && sameFile(known, key) {
		return &known
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil
	}
	if opened, err := f.Stat(); err != nil || opened.Size() != key.Size || opened.ModTime().UnixNano() != key.ModTime || fileInode(opened) != key.Inode {
		return nil // replaced while it was read
	}
	key.SHA256 = hex.EncodeToString(h.Sum(nil))
	if companions.byPath == nil {
		companions.byPath = map[string]Companion{}
	}
	companions.byPath[path] = key
	return &key
}

func sameFile(a, b Companion) bool {
	return a.Path == b.Path && a.Size == b.Size && a.ModTime == b.ModTime && a.Inode == b.Inode
}

// executable is os.Executable; tests replace it.
var executable = os.Executable

// SameCompanion accepts a controller of the same build stamp that verified
// this very program beside itself. It is as strict as SameBuild: the same
// build, and a byte-identical file the controller hashed, without hashing
// anything here when the file is unchanged.
func SameCompanion(stamp string) Check {
	return func(id ControllerIdentity) bool {
		if stamp == "" || id.Stamp != stamp || id.Build == "" || id.Protocol != Protocol || id.Attach == nil || id.Attach.SHA256 == "" {
			return false
		}
		self, err := executable()
		if err != nil {
			return false
		}
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		if filepath.Clean(self) != filepath.Clean(id.Attach.Path) {
			return false
		}
		info, err := os.Stat(self)
		if err != nil {
			return false
		}
		mine := Companion{Path: id.Attach.Path, Size: info.Size(), ModTime: info.ModTime().UnixNano(), Inode: fileInode(info)}
		if sameFile(mine, *id.Attach) {
			return true
		}
		// Changed since the controller looked: compare the content.
		now := CompanionOf(self)
		return now != nil && now.SHA256 == id.Attach.SHA256
	}
}
