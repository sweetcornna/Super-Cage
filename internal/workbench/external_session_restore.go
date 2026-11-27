package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	externalSessionFile  = "external-session-restore.json"
	externalSessionLimit = 512
)

var errExternalSessionClosed = errors.New("此原应用会话已标记关闭")

type externalSessionManager struct {
	mu            sync.Mutex
	recovery      sync.Mutex
	watching      bool
	exits         map[string]bool
	runGuestClaim func(context.Context, string, string, string) (bool, error)
}

type externalSessionRecord struct {
	ID            string                `json:"id"`
	App           string                `json:"application"`
	Guest         bool                  `json:"guest,omitempty"`
	CageInstance  string                `json:"cage_instance"`
	Connection    string                `json:"connection"`
	RuntimeID     string                `json:"runtime_id,omitempty"`
	Workspace     string                `json:"workspace"`
	WorktreeID    string                `json:"worktree_id,omitempty"`
	Continuation  string                `json:"continuation_id"`
	HostID        string                `json:"host_id,omitempty"`
	PaneKey       string                `json:"pane_key,omitempty"`
	Incarnation   string                `json:"incarnation_id,omitempty"`
	Handle        string                `json:"last_handle,omitempty"`
	Provider      string                `json:"provider,omitempty"`
	Kind          string                `json:"kind,omitempty"`
	Account       string                `json:"account,omitempty"`
	Profile       string                `json:"profile,omitempty"`
	UpdatedAt     int64                 `json:"updated_at"`
	Native        *externalNativeOrigin `json:"native_origin,omitempty"`
	ResumeBoot    string                `json:"resume_boot,omitempty"`
	ResumedHandle string                `json:"resumed_handle,omitempty"`
	Closed        bool                  `json:"closed,omitempty"`
}

type externalSessionWatch struct {
	Guest        bool   `json:"guest,omitempty"`
	CageInstance string `json:"cage_instance"`
	Connection   string `json:"connection"`
	RuntimeID    string `json:"runtime_id,omitempty"`
	Workspace    string `json:"workspace"`
}

type externalSessionJournal struct {
	Records []externalSessionRecord `json:"records"`
	Orca    []externalSessionWatch  `json:"orca"`
}

var (
	externalScopeID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	externalProfileID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	externalOrcaID    = regexp.MustCompile(`^orca_session_id:[0-9a-fA-F-]{36}$`)
)

func externalSessionText(s string, limit int) bool {
	return len(s) > 0 && len(s) <= limit && !strings.ContainsFunc(s, unicode.IsControl)
}

func externalSessionWorkspace(value string) bool {
	// The path belongs to the original client. Do not resolve remote symlinks
	// against Cage's host filesystem.
	return externalSessionText(value, 1024) && (strings.HasPrefix(value, "/") || filepath.IsAbs(value))
}

func externalSessionKey(r externalSessionRecord) string {
	paneIncarnation := ""
	if r.App == "orca" && !externalOrcaID.MatchString(r.Continuation) {
		paneIncarnation = r.Incarnation
		if paneIncarnation == "" {
			paneIncarnation = r.Handle
		}
	}
	origin := r.App
	if r.Guest {
		origin += ":guest"
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		origin, r.CageInstance, r.Connection, r.RuntimeID, r.Workspace, r.WorktreeID, r.HostID, r.Continuation, paneIncarnation,
	}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func (s *Server) loadExternalSessionJournal() (externalSessionJournal, error) {
	var data externalSessionJournal
	f, err := os.Open(filepath.Join(s.profile, externalSessionFile))
	if os.IsNotExist(err) {
		return data, nil
	}
	if err != nil {
		return data, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 2<<20))
	if err != nil || len(body) >= 2<<20 || json.Unmarshal(body, &data) != nil {
		return externalSessionJournal{}, errors.New("原应用会话记录无法读取")
	}
	return data, nil
}

func (s *Server) saveExternalSessionJournal(data externalSessionJournal) error {
	if err := writePrivate(filepath.Join(s.profile, externalSessionFile), data); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(s.profile)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func updateExternalSessionRecord(data *externalSessionJournal, r externalSessionRecord, now int64) (bool, error) {
	r.ID = externalSessionKey(r)
	r.UpdatedAt = now
	for i, existing := range data.Records {
		if existing.ID != r.ID {
			continue
		}
		if existing.Closed {
			return false, errExternalSessionClosed
		}
		if existing.Account != r.Account || existing.Provider != r.Provider || existing.Profile != r.Profile || existing.Kind != r.Kind {
			return false, errors.New("原会话的客户端或账号已变化，已保留旧记录")
		}
		if existing.UpdatedAt >= now-30 {
			return false, nil
		}
		r.Native, r.ResumeBoot, r.ResumedHandle = existing.Native, existing.ResumeBoot, existing.ResumedHandle
		data.Records[i] = r
		return true, nil
	}
	if len(data.Records) >= externalSessionLimit {
		return false, errors.New("原应用会话记录已达上限")
	}
	data.Records = append(data.Records, r)
	return true, nil
}

// An Orca terminal can get a new pane or handle when another client starts
// the same provider thread. Merge only positive, account-bound native evidence.
func (s *Server) recordExternalOrcaOrigin(observed externalSessionRecord, origin externalNativeOrigin) error {
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(data.Records, func(r externalSessionRecord) bool { return r.ID == observed.ID })
	if index < 0 || data.Records[index].Closed {
		return errExternalSessionClosed
	}
	if data.Records[index].Native != nil {
		return nil
	}
	for i := range data.Records {
		r := &data.Records[i]
		if i == index || r.Native == nil || r.App != observed.App || r.Guest != observed.Guest ||
			r.CageInstance != observed.CageInstance || r.Connection != observed.Connection || r.RuntimeID != observed.RuntimeID ||
			r.Workspace != observed.Workspace || r.WorktreeID != observed.WorktreeID || r.HostID != observed.HostID ||
			!origin.sameIdentity(*r.Native) {
			continue
		}
		if !r.Closed {
			r.Native, r.ResumeBoot, r.ResumedHandle = &origin, "", observed.Handle
			r.UpdatedAt = time.Now().Unix()
		}
		data.Records[index].Closed = true
		return s.saveExternalSessionJournal(data)
	}
	data.Records[index].Native = &origin
	return s.saveExternalSessionJournal(data)
}

// Persist only changes to an existing, still-open record. Native proof and launch
// receipts survive later catalog refreshes and never revive a closed record.
func (s *Server) updateExternalOrca(id string, change func(*externalSessionRecord) bool) error {
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return err
	}
	for i := range data.Records {
		row := &data.Records[i]
		if row.ID != id || row.App != "orca" {
			continue
		}
		if row.Closed {
			return errExternalSessionClosed
		}
		if change(row) {
			return s.saveExternalSessionJournal(data)
		}
		return nil
	}
	return errors.New("原 Orca 会话记录已移除")
}

func (s *Server) closeExternalSessionRecord(instance, id string) error {
	m := &s.externalSessions
	m.recovery.Lock()
	defer m.recovery.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return err
	}
	for i := range data.Records {
		r := &data.Records[i]
		if r.ID == id && r.CageInstance == instance {
			if r.Closed {
				return nil
			}
			r.Closed = true
			return s.saveExternalSessionJournal(data)
		}
	}
	return errors.New("未找到此连接中的原应用会话")
}

func (s *Server) externalSessionRecords(instance string) ([]externalSessionRecord, error) {
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	records := make([]externalSessionRecord, 0)
	for _, r := range data.Records {
		if !r.Closed && !r.Guest && r.CageInstance == instance {
			records = append(records, r)
		}
	}
	slices.SortFunc(records, func(a, b externalSessionRecord) int {
		if a.UpdatedAt != b.UpdatedAt {
			if a.UpdatedAt > b.UpdatedAt {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return records, err
}

// restoreExternalSessions reconciles the workspace scopes saved by this
// desktop. Without an authorized scope, it never queries Orca.
func (s *Server) restoreExternalSessions(ctx context.Context) error {
	selected, _ := s.externalSessionInstance()
	if selected == "" || !s.hasExternalOrcaWatches(selected) {
		return nil
	}
	instance, err := s.externalSessionLocalScope(ctx)
	if err != nil || instance != selected {
		return nil // the desktop opens the existing records after unlocking
	}
	return s.captureExternalOrcaWatches(ctx, instance)
}

func (s *Server) hasExternalOrcaWatches(instance string) bool {
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(data.Orca, func(w externalSessionWatch) bool { return !w.Guest && w.CageInstance == instance })
}

func (s *Server) captureExternalOrcaWatches(ctx context.Context, instance string) error {
	s.externalSessions.mu.Lock()
	data, err := s.loadExternalSessionJournal()
	s.externalSessions.mu.Unlock()
	if err != nil {
		return err
	}
	var result error
	for _, watch := range data.Orca {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		if watch.Guest || watch.CageInstance != instance {
			continue
		}
		client, _, err := s.externalOrcaConnection(ctx, watch.Connection, watch.RuntimeID)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		_, err = s.captureExternalOrcaWithClient(ctx, watch, false, client)
		result = errors.Join(result, err)
		if s.externalSessions.recovery.TryLock() {
			resumeErr := s.reconcileExternalOrcaWithClient(ctx, watch, client)
			s.externalSessions.recovery.Unlock()
			result = errors.Join(result, resumeErr)
		}
	}
	return result
}

func (s *Server) watchExternalSessions() {
	m := &s.externalSessions
	m.mu.Lock()
	if m.watching {
		m.mu.Unlock()
		return
	}
	ctx, release, err := s.beginActivity(context.Background())
	if err != nil {
		m.mu.Unlock()
		return
	}
	m.watching = true
	m.mu.Unlock()
	go func() {
		defer release()
		defer func() {
			m.mu.Lock()
			m.watching = false
			m.mu.Unlock()
		}()
		_ = s.restoreExternalSessions(ctx)
		ticker := time.NewTicker(12 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.restoreExternalSessions(ctx)
				_ = s.restoreGuestOrcaSessions(ctx)
			}
		}
	}()
}
