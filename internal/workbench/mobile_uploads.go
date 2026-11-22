package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	mobileUploadLimit = 100 << 20
	mobileUploadChunk = 48 << 10
	mobileUploadSlots = 4
)

// An upload with no chunk for this long is discarded with its temporary file.
var mobileUploadIdle = 10 * time.Minute

var mobileUploadID = regexp.MustCompile("^[0-9a-f]{32}$")

var mobileSHA256 = regexp.MustCompile("^[0-9a-f]{64}$")

// mobileUploads holds phone uploads until they finish, fail or expire.
type mobileUploads struct {
	mu     sync.Mutex
	active map[string]*mobileUpload
	closed bool
}

// The temporary file is created exclusively next to its destination and is
// renamed only after its size and SHA-256 match the declared file.
type mobileUpload struct {
	mu              sync.Mutex
	dir             *os.Root
	file            *os.File
	created         fs.FileInfo
	temp, name, rel string
	size, received  int64
	overwrite       bool
	digest          hash.Hash
	lastOffset      int64
	lastSum         [32]byte
	lastLength      int
	timer           *time.Timer
	done            bool
}

// discard closes the upload and removes its temporary file. Caller holds mu.
func (u *mobileUpload) discard() {
	if u.done {
		return
	}
	u.done = true
	if u.timer != nil {
		u.timer.Stop()
	}
	if u.file != nil {
		u.file.Close()
		u.file = nil
	}
	if info, err := u.dir.Lstat(u.temp); err == nil && os.SameFile(info, u.created) {
		_ = u.dir.Remove(u.temp)
	}
	u.dir.Close()
}

func (m *mobileUploads) add(id string, upload *mobileUpload) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fileFailure(http.StatusServiceUnavailable, "后台正在退出，请重新打开 Super Cage。")
	}
	if len(m.active) >= mobileUploadSlots {
		return fileFailure(http.StatusConflict, "同时进行的上传过多，请稍后重试")
	}
	if m.active == nil {
		m.active = map[string]*mobileUpload{}
	}
	m.active[id] = upload
	return nil
}

// take locks and returns a pending upload; ok is false when it is gone.
func (m *mobileUploads) take(id string) (*mobileUpload, bool) {
	m.mu.Lock()
	upload := m.active[id]
	m.mu.Unlock()
	if upload == nil {
		return nil, false
	}
	upload.mu.Lock()
	if upload.done {
		upload.mu.Unlock()
		return nil, false
	}
	return upload, true
}

// end discards an upload and frees its slot. Caller holds the upload's mu.
func (m *mobileUploads) end(id string, upload *mobileUpload) {
	upload.discard()
	m.mu.Lock()
	if m.active[id] == upload {
		delete(m.active, id)
	}
	m.mu.Unlock()
}

func (m *mobileUploads) expire(id string) {
	if upload, ok := m.take(id); ok {
		m.end(id, upload)
		upload.mu.Unlock()
	}
}

func (m *mobileUploads) close() {
	m.mu.Lock()
	m.closed = true
	uploads := m.active
	m.active = nil
	m.mu.Unlock()
	for _, upload := range uploads {
		upload.mu.Lock()
		upload.discard()
		upload.mu.Unlock()
	}
}

// mobileUploadName is a single path segment the host can create.
func mobileUploadName(name string) error {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) ||
		name == "." || name == ".." || strings.Contains(name, "/") {
		return fileFailure(http.StatusBadRequest, "文件名无效")
	}
	if _, err := filepath.Localize(name); err != nil {
		return fileFailure(http.StatusBadRequest, "文件名无效")
	}
	return nil
}

var errUploadGone = fileFailure(http.StatusNotFound, "上传已过期或不存在，请重新上传")

func (s *Server) mobileUploadStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Project   string `json:"project"`
		Dir       string `json:"dir"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		Overwrite bool   `json:"overwrite"`
	}
	if !readJSON(w, r, &in, 16<<10) {
		return
	}
	if in.Size < 0 || in.Size > mobileUploadLimit {
		http.Error(w, "单个文件最大 100 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	parts, err := mobileRelative(in.Dir)
	if err == nil {
		err = mobileUploadName(in.Name)
	}
	rel := path.Join(in.Dir, in.Name)
	if err == nil {
		err = writableName(rel)
	}
	if err != nil {
		replyFileError(w, err)
		return
	}
	if !s.mobileAllowed(w, r) {
		return
	}
	id, err := s.beginUpload(in.Project, parts, in.Name, rel, in.Size, in.Overwrite)
	if err != nil {
		replyFileError(w, err)
		return
	}
	jsonReply(w, map[string]string{"id": id})
}

func (s *Server) beginUpload(project string, parts []string, name, rel string, size int64, overwrite bool) (string, error) {
	s.mu.Lock()
	sealed := s.appUpdateSealed
	s.mu.Unlock()
	if sealed {
		return "", errAppUpdateSealed
	}
	s.noteAppUpdateActivity()
	root, err := s.mobileProjectRoot(project)
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir, err := openDirectory(root, parts, true)
	if err != nil {
		return "", err
	}
	if err := uploadTarget(dir, name, overwrite); err != nil {
		dir.Close()
		return "", err
	}
	id := randomToken()[:32]
	upload := &mobileUpload{dir: dir, temp: ".super-cage-upload-" + randomToken()[:16] + ".part", name: name, rel: rel,
		size: size, overwrite: overwrite, digest: sha256.New(), lastOffset: -1}
	upload.file, err = dir.OpenFile(upload.temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		upload.created, err = upload.file.Stat()
	}
	if err != nil {
		upload.mu.Lock()
		upload.discard()
		upload.mu.Unlock()
		return "", fileFailure(http.StatusConflict, "无法在此文件夹创建文件，请检查权限和磁盘空间")
	}
	upload.mu.Lock()
	defer upload.mu.Unlock()
	if err := s.mobileUploads.add(id, upload); err != nil {
		upload.discard()
		return "", err
	}
	upload.timer = time.AfterFunc(mobileUploadIdle, func() { s.mobileUploads.expire(id) })
	return id, nil
}

// uploadTarget refuses to replace anything but a regular file, and replaces
// that only when the phone asked to.
func uploadTarget(dir *os.Root, name string, overwrite bool) error {
	info, err := dir.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fileFailure(http.StatusConflict, "无法读取目标文件夹")
	case !info.Mode().IsRegular():
		return fileFailure(http.StatusConflict, "已有同名的文件夹或链接")
	case !overwrite:
		return fileFailure(http.StatusConflict, "已有同名文件")
	}
	return nil
}

func (s *Server) mobileUploadChunk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
		Data   []byte `json:"data"`
	}
	if !readJSON(w, r, &in, 72<<10) {
		return
	}
	if !mobileUploadID.MatchString(in.ID) || in.Offset < 0 || len(in.Data) == 0 {
		http.Error(w, "上传请求无效", http.StatusBadRequest)
		return
	}
	if len(in.Data) > mobileUploadChunk {
		http.Error(w, "单次上传内容过大", http.StatusRequestEntityTooLarge)
		return
	}
	if !s.mobileAllowed(w, r) {
		return
	}
	upload, ok := s.mobileUploads.take(in.ID)
	if !ok {
		replyFileError(w, errUploadGone)
		return
	}
	defer upload.mu.Unlock()
	sum := sha256.Sum256(in.Data)
	// A repeated last chunk is a retry after a lost reply.
	if in.Offset == upload.lastOffset && len(in.Data) == upload.lastLength && sum == upload.lastSum {
		jsonReply(w, map[string]int64{"received": upload.received})
		return
	}
	if in.Offset != upload.received {
		http.Error(w, "上传进度不一致，请重新上传", http.StatusConflict)
		return
	}
	if upload.received+int64(len(in.Data)) > upload.size {
		http.Error(w, "上传内容超过文件大小", http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := upload.file.Write(in.Data); err != nil {
		s.mobileUploads.end(in.ID, upload)
		http.Error(w, "写入文件失败，请检查磁盘空间后重新上传", http.StatusConflict)
		return
	}
	upload.digest.Write(in.Data)
	upload.lastOffset, upload.lastLength, upload.lastSum = in.Offset, len(in.Data), sum
	upload.received += int64(len(in.Data))
	upload.timer.Reset(mobileUploadIdle)
	jsonReply(w, map[string]int64{"received": upload.received})
}

func (s *Server) mobileUploadFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	}
	if !readJSON(w, r, &in, 1<<10) {
		return
	}
	if !mobileUploadID.MatchString(in.ID) || !mobileSHA256.MatchString(in.SHA256) {
		http.Error(w, "上传请求无效", http.StatusBadRequest)
		return
	}
	if !s.mobileAllowed(w, r) {
		return
	}
	upload, ok := s.mobileUploads.take(in.ID)
	if !ok {
		replyFileError(w, errUploadGone)
		return
	}
	defer upload.mu.Unlock()
	if upload.received != upload.size {
		http.Error(w, "文件尚未上传完整", http.StatusConflict)
		return
	}
	if err := upload.save(in.SHA256); err != nil {
		s.mobileUploads.end(in.ID, upload)
		replyFileError(w, err)
		return
	}
	s.mobileUploads.end(in.ID, upload)
	jsonReply(w, map[string]any{"path": upload.rel, "name": upload.name, "size": upload.size})
}

// save verifies and renames the complete file. Caller holds mu.
func (u *mobileUpload) save(sum string) error {
	if hex.EncodeToString(u.digest.Sum(nil)) != sum {
		return fileFailure(http.StatusConflict, "文件校验失败，请重新上传")
	}
	err := u.file.Sync()
	if closeErr := u.file.Close(); err == nil {
		err = closeErr
	}
	u.file = nil
	if err != nil {
		return fileFailure(http.StatusConflict, "写入文件失败，请检查磁盘空间后重新上传")
	}
	if info, err := u.dir.Lstat(u.temp); err != nil || !os.SameFile(info, u.created) || info.Size() != u.size {
		return fileFailure(http.StatusConflict, "上传的临时文件已变化，请重新上传")
	}
	if err := uploadTarget(u.dir, u.name, u.overwrite); err != nil {
		return err
	}
	if err := u.dir.Rename(u.temp, u.name); err != nil {
		return fileFailure(http.StatusConflict, "无法保存文件，请检查权限后重试")
	}
	// The renamed file is the result; discard must not look for it.
	u.created = nil
	return nil
}

func (s *Server) mobileUploadAbort(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &in, 1<<10) {
		return
	}
	if !mobileUploadID.MatchString(in.ID) {
		http.Error(w, "上传请求无效", http.StatusBadRequest)
		return
	}
	if upload, ok := s.mobileUploads.take(in.ID); ok {
		s.mobileUploads.end(in.ID, upload)
		upload.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}
