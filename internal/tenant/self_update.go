package tenant

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/sylvinus/agent-vm/internal/appupdate/catalog"
	"github.com/sylvinus/agent-vm/internal/version"
)

const (
	defaultGuestCagePath    = "/usr/lib/claude-cage/cage"
	defaultGuestServiceName = "cage-serve.service"
	maxReleaseMetadataBytes = 4 << 20
	maxChecksumFileBytes    = 64 << 10
	maxBundleArchiveBytes   = 256 << 20
	maxExtractedFileBytes   = 256 << 20
	maxUpdateLogLines       = 120
	postUpdateGraceFile     = "post-update-grace.json"
	postUpdateGraceDuration = 5 * time.Minute
	selfUpdateRestartDelay  = 1500 * time.Millisecond
)

// SelfUpdateConfig controls optional administrator-triggered updates from the
// portal UI. It is off by default and refuses to start without at least one
// explicit admin username.
type SelfUpdateConfig struct {
	Enabled          bool
	Admins           []string
	GatewayPath      string
	UIDir            string
	GuestCagePath    string
	GuestServiceName string
	ReleasesURL      string
	TargetOS         string
	TargetArch       string
	CurrentVersion   string
	HTTPClient       *http.Client
	AllowInsecureURL bool
	RestartDelay     time.Duration
	OnRestart        func(gatewayPath string)
	WorkspaceUpdater WorkspaceUpdater
	VerifyStaged     func(ctx context.Context, stageDir, targetVersion string) error
}

// WorkspaceUpdateResult records what happened to one managed workspace during
// an update run.
type WorkspaceUpdateResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "updated" | "skipped" | "failed"
	Detail string `json:"detail,omitempty"`
}

// WorkspaceUpdater updates the Super Cage binary inside one workspace instance.
type WorkspaceUpdater interface {
	UpdateWorkspace(ctx context.Context, id, stagedCagePath, targetVersion string, logf func(string)) (skipped bool, detail string, err error)
}

// SelfUpdateJob describes the active or most recent update run.
type SelfUpdateJob struct {
	Running           bool                    `json:"running"`
	Phase             string                  `json:"phase"`
	TargetVersion     string                  `json:"target_version,omitempty"`
	Message           string                  `json:"message,omitempty"`
	Progress          int                     `json:"progress"`
	WorkspacesTotal   int                     `json:"workspaces_total"`
	WorkspacesUpdated int                     `json:"workspaces_updated"`
	WorkspacesSkipped int                     `json:"workspaces_skipped"`
	Workspaces        []WorkspaceUpdateResult `json:"workspaces,omitempty"`
	Logs              []string                `json:"logs,omitempty"`
	Error             string                  `json:"error,omitempty"`
	StartedAt         string                  `json:"started_at,omitempty"`
	FinishedAt        string                  `json:"finished_at,omitempty"`
}

// SelfUpdateStatus is returned by GET /api/tenant/update and POST /api/tenant/update/*.
type SelfUpdateStatus struct {
	Enabled         bool          `json:"enabled"`
	CurrentVersion  string        `json:"current_version"`
	CurrentCommit   string        `json:"current_commit,omitempty"`
	Arch            string        `json:"arch"`
	InstanceType    string        `json:"instance_type,omitempty"`
	LatestVersion   string        `json:"latest_version,omitempty"`
	UpdateAvailable bool          `json:"update_available"`
	PublishedAt     string        `json:"published_at,omitempty"`
	ReleaseURL      string        `json:"release_url,omitempty"`
	ReleaseNotes    string        `json:"release_notes,omitempty"`
	CheckedAt       string        `json:"checked_at,omitempty"`
	CheckError      string        `json:"check_error,omitempty"`
	Job             SelfUpdateJob `json:"job"`
}

type serverReleaseCandidate struct {
	Version      string
	Tag          string
	PublishedAt  string
	ReleaseURL   string
	ReleaseNotes string
	Bundle       catalog.Asset
	Checksums    catalog.Asset
}

type selfUpdater struct {
	cfg     SelfUpdateConfig
	admins  map[string]bool
	updater WorkspaceUpdater
	commit  string

	mu            sync.Mutex
	checkedAt     time.Time
	checkErr      string
	latest        *serverReleaseCandidate
	releases      []serverRelease
	job           SelfUpdateJob
	instanceType  string
}

type serverRelease struct {
	Tag         string          `json:"tag_name"`
	Name        string          `json:"name"`
	Body        string          `json:"body"`
	HTMLURL     string          `json:"html_url"`
	Draft       bool            `json:"draft"`
	Prerelease  bool            `json:"prerelease"`
	PublishedAt string          `json:"published_at"`
	Assets      []catalog.Asset `json:"assets"`
}

func newSelfUpdater(cfg SelfUpdateConfig, rt Runtime) (*selfUpdater, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	admins := make(map[string]bool, len(cfg.Admins))
	for _, raw := range cfg.Admins {
		name := canonicalUsername(raw)
		if name == "" {
			return nil, fmt.Errorf("invalid self-update admin username %q", raw)
		}
		admins[name] = true
	}
	if len(admins) == 0 {
		return nil, errors.New("self-update requires at least one -self-update-admin username")
	}
	if cfg.GatewayPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve gateway executable path: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		cfg.GatewayPath = exe
	}
	if !filepath.IsAbs(cfg.GatewayPath) {
		return nil, errors.New("self-update gateway path must be absolute")
	}
	if cfg.UIDir == "" || !filepath.IsAbs(cfg.UIDir) {
		return nil, errors.New("self-update requires an absolute portal UI directory")
	}
	if cfg.GuestCagePath == "" {
		cfg.GuestCagePath = defaultGuestCagePath
	}
	if !path.IsAbs(cfg.GuestCagePath) || path.Clean(cfg.GuestCagePath) != cfg.GuestCagePath || strings.ContainsAny(cfg.GuestCagePath, "\x00\r\n'\" ") {
		return nil, errors.New("self-update guest cage path must be a clean absolute path")
	}
	if cfg.GuestServiceName == "" {
		cfg.GuestServiceName = defaultGuestServiceName
	}
	if strings.ContainsAny(cfg.GuestServiceName, "\x00\r\n/ '\"\\;") {
		return nil, errors.New("invalid self-update guest service name")
	}
	if cfg.ReleasesURL == "" {
		cfg.ReleasesURL = catalog.ReleasesURL
	}
	if cfg.TargetOS == "" {
		cfg.TargetOS = runtime.GOOS
	}
	if cfg.TargetArch == "" {
		cfg.TargetArch = runtime.GOARCH
	}
	if cfg.CurrentVersion == "" {
		cfg.CurrentVersion = strings.TrimSpace(version.Version)
	}
	if cfg.RestartDelay == 0 {
		cfg.RestartDelay = selfUpdateRestartDelay
	}
	u := &selfUpdater{
		cfg:     cfg,
		admins:  admins,
		updater: cfg.WorkspaceUpdater,
		commit:  currentBuildCommit(),
		job:     SelfUpdateJob{Phase: "idle"},
	}
	if incusRt, ok := rt.(*IncusRuntime); ok {
		u.instanceType = incusRt.instanceType()
		if u.updater == nil {
			u.updater = &incusWorkspaceUpdater{
				runtime:     incusRt,
				cagePath:    cfg.GuestCagePath,
				serviceName: cfg.GuestServiceName,
			}
		}
	}
	return u, nil
}

func currentBuildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

func (s *Server) canSelfUpdate(username string) bool {
	return s.selfUpdate != nil && s.selfUpdate.admins[canonicalUsername(username)]
}

func (s *Server) requireSelfUpdateAdmin(w http.ResponseWriter, r *http.Request) *loginSession {
	if s.selfUpdate == nil {
		notFound(w)
		return nil
	}
	s.mu.Lock()
	session := s.expectedSessionLocked(w, r)
	s.mu.Unlock()
	if session == nil {
		return nil
	}
	if !s.canSelfUpdate(session.user.Username) {
		problem(w, http.StatusForbidden, "forbidden", "Administrator access is required.")
		return nil
	}
	return session
}

func (s *Server) handleGetSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if s.requireSelfUpdateAdmin(w, r) == nil {
		return
	}
	jsonReply(w, http.StatusOK, s.selfUpdate.statusSnapshot())
}

func (s *Server) handleCheckSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	session := s.requireSelfUpdateAdmin(w, r)
	if session == nil {
		return
	}
	s.mu.Lock()
	allowed := s.allowRate("self-update-check", session.user.ID, 30, s.now())
	s.mu.Unlock()
	if !allowed {
		rateLimited(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, err := s.selfUpdate.check(ctx, s.now())
	status := s.selfUpdate.statusSnapshot()
	if err != nil {
		jsonReply(w, http.StatusOK, status)
		return
	}
	jsonReply(w, http.StatusOK, status)
}

func (s *Server) handleApplySelfUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version string `json:"version"`
		Force   bool   `json:"force"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	session := s.requireSelfUpdateAdmin(w, r)
	if session == nil {
		return
	}
	s.mu.Lock()
	allowed := s.allowRate("self-update-apply", session.user.ID, 10, s.now())
	s.mu.Unlock()
	if !allowed {
		rateLimited(w)
		return
	}
	targetVersion := strings.TrimPrefix(strings.TrimSpace(input.Version), "v")
	if targetVersion != "" {
		tag := "v" + targetVersion
		if !semver.IsValid(tag) || semver.Canonical(tag) != tag {
			invalidInput(w)
			return
		}
	}
	u := s.selfUpdate
	u.mu.Lock()
	if u.job.Running {
		u.mu.Unlock()
		problem(w, http.StatusConflict, "update_in_progress", "已有更新任务正在执行，请等待完成。")
		return
	}
	now := s.now().UTC()
	u.job = SelfUpdateJob{
		Running:       true,
		Phase:         "checking",
		TargetVersion: targetVersion,
		Message:       "正在核对 Super Cage 发行版本…",
		Progress:      5,
		Logs:          []string{formatUpdateLog(now, "开始执行 Super Cage 更新任务")},
		StartedAt:     now.Format(time.RFC3339),
	}
	u.mu.Unlock()

	s.workers.Go(func() {
		s.runSelfUpdateJob(targetVersion, input.Force)
	})
	jsonReply(w, http.StatusAccepted, u.statusSnapshot())
}

func (u *selfUpdater) statusSnapshot() SelfUpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := SelfUpdateStatus{
		Enabled:        true,
		CurrentVersion: u.cfg.CurrentVersion,
		CurrentCommit:  u.commit,
		Arch:           u.cfg.TargetArch,
		InstanceType:   u.instanceType,
		CheckError:     u.checkErr,
		Job:            u.job,
	}
	st.Job.Logs = slices.Clone(u.job.Logs)
	st.Job.Workspaces = slices.Clone(u.job.Workspaces)
	if !u.checkedAt.IsZero() {
		st.CheckedAt = u.checkedAt.UTC().Format(time.RFC3339)
	}
	if u.latest != nil {
		st.LatestVersion = u.latest.Version
		st.PublishedAt = u.latest.PublishedAt
		st.ReleaseURL = u.latest.ReleaseURL
		st.ReleaseNotes = u.latest.ReleaseNotes
		st.UpdateAvailable = isNewerSemver(u.latest.Version, u.cfg.CurrentVersion)
	}
	return st
}

func (u *selfUpdater) httpClient() *http.Client {
	if u.cfg.HTTPClient != nil {
		return u.cfg.HTTPClient
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 6 {
				return errors.New("too many redirects")
			}
			if !u.allowedURL(req.URL, true) {
				return errors.New("更新下载重定向到不受信任的地址")
			}
			return nil
		},
	}
}

func (u *selfUpdater) allowedURL(parsed *url.URL, redirect bool) bool {
	if parsed == nil {
		return false
	}
	if u.cfg.AllowInsecureURL {
		return parsed.Scheme == "http" || parsed.Scheme == "https"
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	switch parsed.Hostname() {
	case "api.github.com":
		return parsed.Path == "/repos/"+catalog.Repository+"/releases"
	case "github.com":
		return strings.HasPrefix(parsed.Path, "/"+catalog.Repository+"/releases/download/") && !strings.Contains(parsed.Path, "/../")
	case "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return redirect
	}
	return false
}

func (u *selfUpdater) check(ctx context.Context, now time.Time) (*serverReleaseCandidate, error) {
	releases, err := u.fetchReleases(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checkedAt = now
	if err != nil {
		u.checkErr = err.Error()
		return nil, err
	}
	u.checkErr = ""
	u.releases = releases
	cand, selErr := selectServerRelease(releases, u.cfg.CurrentVersion, u.cfg.TargetArch, "", true, u.cfg.AllowInsecureURL)
	if selErr != nil {
		u.checkErr = selErr.Error()
		return nil, selErr
	}
	u.latest = cand
	return cand, nil
}

func (u *selfUpdater) fetchReleases(ctx context.Context) ([]serverRelease, error) {
	parsed, err := url.Parse(u.cfg.ReleasesURL)
	if err != nil || !u.allowedURL(parsed, false) {
		return nil, errors.New("更新源地址不属于 Super Cage 受信发布源")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.cfg.ReleasesURL, nil)
	if err != nil {
		return nil, errors.New("无法创建更新检查请求")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Super-Cage-Tenant/"+u.cfg.CurrentVersion)
	if parsed.Hostname() == "api.github.com" {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接发布源失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("发布源返回 HTTP %d，请稍后重试", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseMetadataBytes+1))
	if err != nil || int64(len(body)) > maxReleaseMetadataBytes {
		return nil, errors.New("发布源响应中断或超过大小限制")
	}
	var releases []serverRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, errors.New("发布源返回了无效的版本列表")
	}
	return releases, nil
}

func isNewerSemver(candidateVer, currentVer string) bool {
	cTag, curTag := "v"+candidateVer, "v"+currentVer
	if !semver.IsValid(cTag) || !semver.IsValid(curTag) {
		return false
	}
	return semver.Compare(cTag, curTag) > 0
}

// selectServerRelease finds the target (or newest) published Super-Cage-NoVirt
// Linux bundle for arch. When includeEqualOrOlder is true and targetVersion is
// empty, it returns the newest stable release on the channel even when already
// current so the UI can display "已是最新版本 vX.Y.Z".
func selectServerRelease(releases []serverRelease, currentVer, arch, targetVersion string, includeEqualOrOlder, allowInsecureURL bool) (*serverReleaseCandidate, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("暂不支持 %s 架构的在线自更新", arch)
	}
	curTag := "v" + currentVer
	if !semver.IsValid(curTag) || semver.Canonical(curTag) != curTag {
		return nil, fmt.Errorf("当前版本号 %q 无效，无法安全比较更新", currentVer)
	}
	currentPre := semver.Prerelease(curTag) != ""
	var best *serverReleaseCandidate
	for _, r := range releases {
		if r.Draft || !strings.HasPrefix(r.Tag, "v") || !semver.IsValid(r.Tag) || semver.Canonical(r.Tag) != r.Tag {
			continue
		}
		v := strings.TrimPrefix(r.Tag, "v")
		if targetVersion != "" && v != targetVersion {
			continue
		}
		pre := r.Prerelease || semver.Prerelease(r.Tag) != ""
		if targetVersion == "" && !currentPre && pre {
			continue
		}
		if targetVersion == "" && !includeEqualOrOlder && semver.Compare(r.Tag, curTag) <= 0 {
			continue
		}
		if best != nil && semver.Compare(r.Tag, best.Tag) <= 0 {
			continue
		}
		bundleName := fmt.Sprintf("Super-Cage-NoVirt-%s-linux-%s.tar.gz", v, arch)
		var bundle, checksums catalog.Asset
		for _, a := range r.Assets {
			if a.State != "" && a.State != "uploaded" {
				continue
			}
			switch a.Name {
			case bundleName:
				if a.Size > 0 && a.Size <= maxBundleArchiveBytes {
					bundle = a
				}
			case "SHA256SUMS":
				if a.Size > 0 && a.Size <= maxChecksumFileBytes {
					checksums = a
				}
			}
		}
		if bundle.URL == "" || (checksums.URL == "" && bundle.Digest == "") {
			continue
		}
		if !allowInsecureURL {
			base := "https://github.com/" + catalog.Repository + "/releases/download/" + r.Tag + "/"
			if bundle.URL != base+bundle.Name || (checksums.URL != "" && checksums.URL != base+"SHA256SUMS") {
				continue
			}
		}
		notes := strings.TrimSpace(r.Body)
		if len(notes) > 4000 {
			notes = strings.ToValidUTF8(notes[:4000], "") + "\n…"
		}
		releaseURL := r.HTMLURL
		if releaseURL == "" {
			releaseURL = "https://github.com/" + catalog.Repository + "/releases/tag/" + r.Tag
		}
		best = &serverReleaseCandidate{
			Version:      v,
			Tag:          r.Tag,
			PublishedAt:  r.PublishedAt,
			ReleaseURL:   releaseURL,
			ReleaseNotes: notes,
			Bundle:       bundle,
			Checksums:    checksums,
		}
	}
	return best, nil
}

func (s *Server) runSelfUpdateJob(requestedVersion string, force bool) {
	u := s.selfUpdate
	ctx, cancel := context.WithTimeout(s.context, 20*time.Minute)
	defer cancel()

	appendLog := func(line string) {
		entry := formatUpdateLog(s.now().UTC(), line)
		log.Printf("cage-tenant: self-update: %s", line)
		u.mu.Lock()
		u.job.Logs = append(u.job.Logs, entry)
		if len(u.job.Logs) > maxUpdateLogLines {
			u.job.Logs = u.job.Logs[len(u.job.Logs)-maxUpdateLogLines:]
		}
		u.mu.Unlock()
	}
	setStage := func(phase, message string, progress int) {
		u.mu.Lock()
		u.job.Phase = phase
		u.job.Message = message
		u.job.Progress = progress
		u.mu.Unlock()
	}
	failJob := func(err error) {
		msg := err.Error()
		appendLog("更新失败：" + msg)
		u.mu.Lock()
		u.job.Running = false
		u.job.Phase = "error"
		u.job.Error = msg
		u.job.Message = "更新未完成：" + msg
		u.job.FinishedAt = s.now().UTC().Format(time.RFC3339)
		u.mu.Unlock()
	}

	// 1. Fetch releases and select verified target bundle.
	releases, err := u.fetchReleases(ctx)
	if err != nil {
		failJob(err)
		return
	}
	cand, err := selectServerRelease(releases, u.cfg.CurrentVersion, u.cfg.TargetArch, requestedVersion, true, u.cfg.AllowInsecureURL)
	if err != nil {
		failJob(err)
		return
	}
	if cand == nil {
		failJob(errors.New("发布源中未找到匹配当前架构的服务器更新包"))
		return
	}
	u.mu.Lock()
	u.checkedAt = s.now()
	u.checkErr = ""
	u.releases = releases
	if requestedVersion == "" {
		u.latest = cand
	}
	u.job.TargetVersion = cand.Version
	u.mu.Unlock()

	if !force && !isNewerSemver(cand.Version, u.cfg.CurrentVersion) {
		appendLog(fmt.Sprintf("当前版本 v%s 已是最新（目标 v%s），无需更新", u.cfg.CurrentVersion, cand.Version))
		u.mu.Lock()
		u.job.Running = false
		u.job.Phase = "done"
		u.job.Progress = 100
		u.job.Message = fmt.Sprintf("当前已是最新版本 v%s，无需更新。", u.cfg.CurrentVersion)
		u.job.FinishedAt = s.now().UTC().Format(time.RFC3339)
		u.mu.Unlock()
		return
	}
	if !force && requestedVersion != "" && semver.Compare("v"+cand.Version, "v"+u.cfg.CurrentVersion) < 0 {
		failJob(fmt.Errorf("拒绝降级：目标版本 v%s 低于当前版本 v%s", cand.Version, u.cfg.CurrentVersion))
		return
	}
	appendLog(fmt.Sprintf("已选择发行版 v%s（%s）", cand.Version, cand.Bundle.Name))

	// 2. Download SHA256SUMS + bundle archive to private staging directory.
	stageDir, err := os.MkdirTemp("", "cage-tenant-update-*")
	if err != nil {
		failJob(fmt.Errorf("创建临时目录失败：%w", err))
		return
	}
	defer os.RemoveAll(stageDir)
	_ = os.Chmod(stageDir, 0o700)

	setStage("downloading", fmt.Sprintf("正在下载 v%s 校验清单与安装包…", cand.Version), 10)
	expectedSHA, err := u.resolveExpectedDigest(ctx, *cand)
	if err != nil {
		failJob(err)
		return
	}
	appendLog("SHA-256 摘要已获取：" + expectedSHA[:16] + "…")

	archivePath := filepath.Join(stageDir, cand.Bundle.Name)
	if err := u.downloadVerifiedArchive(ctx, cand.Bundle, expectedSHA, archivePath, func(got, total int64) {
		if total > 0 {
			pct := 10 + int(got*35/total)
			if pct > 45 {
				pct = 45
			}
			setStage("downloading", fmt.Sprintf("正在下载 %s（%.1f / %.1f MB）…", cand.Bundle.Name, float64(got)/(1<<20), float64(total)/(1<<20)), pct)
		}
	}); err != nil {
		failJob(err)
		return
	}
	appendLog("安装包下载并完成 SHA-256 校验")

	// 3. Extract and verify binaries & assets before touching any live file.
	setStage("verifying", "正在解压并校验二进制架构与内置资源…", 48)
	extractDir := filepath.Join(stageDir, "extracted")
	staged, err := extractServerBundle(archivePath, extractDir)
	if err != nil {
		failJob(err)
		return
	}
	if err := u.verifyStagedBundle(ctx, staged, cand.Version); err != nil {
		failJob(err)
		return
	}
	appendLog(fmt.Sprintf("校验通过：cage v%s（含 Web 与 Lima 客机资源）及 cage-tenant（linux/%s）", cand.Version, u.cfg.TargetArch))

	// 4. Backup tenant state.json before any live mutation.
	stamp := s.now().UTC().Format("20060102-150405")
	if err := backupFileIfExists(filepath.Join(s.cfg.StateDir, "state.json"), filepath.Join(s.cfg.StateDir, "state.json.bak-"+stamp), 0o600); err != nil {
		failJob(fmt.Errorf("备份状态文件失败：%w", err))
		return
	}
	appendLog("已备份 state.json")

	// 5. Update managed workspace instances while holding elastic memory shrink.
	s.holdMemoryShrink("", 10*time.Minute)
	s.mu.Lock()
	var vmList []vmRecord
	for _, rec := range s.store.VMs {
		vmList = append(vmList, rec)
	}
	s.mu.Unlock()
	sortVMRecords(vmList)

	u.mu.Lock()
	u.job.WorkspacesTotal = len(vmList)
	u.job.Workspaces = make([]WorkspaceUpdateResult, 0, len(vmList))
	u.mu.Unlock()

	if len(vmList) > 0 && u.updater != nil {
		setStage("updating_workspaces", fmt.Sprintf("正在更新 %d 个工作区组件…", len(vmList)), 52)
		for idx, rec := range vmList {
			pct := 52 + (idx * 28 / len(vmList))
			setStage("updating_workspaces", fmt.Sprintf("正在更新工作区「%s」（%d/%d）…", rec.Name, idx+1, len(vmList)), pct)
			s.holdMemoryShrink(rec.ID, 10*time.Minute)
			skipped, detail, werr := u.updater.UpdateWorkspace(ctx, rec.ID, staged.cagePath, cand.Version, func(line string) {
				appendLog(fmt.Sprintf("[%s] %s", rec.Name, line))
			})
			res := WorkspaceUpdateResult{ID: rec.ID, Name: rec.Name, Detail: detail}
			switch {
			case werr != nil:
				res.Status = "failed"
				res.Detail = werr.Error()
				u.mu.Lock()
				u.job.Workspaces = append(u.job.Workspaces, res)
				u.mu.Unlock()
				failJob(fmt.Errorf("工作区「%s」更新失败（已保留或回滚原版本）：%w", rec.Name, werr))
				return
			case skipped:
				res.Status = "skipped"
				u.mu.Lock()
				u.job.WorkspacesSkipped++
				u.job.Workspaces = append(u.job.Workspaces, res)
				u.mu.Unlock()
				appendLog(fmt.Sprintf("已跳过未运行的工作区「%s」：%s", rec.Name, detail))
			default:
				res.Status = "updated"
				s.dropWorkspaceConnection(rec.ID)
				u.mu.Lock()
				u.job.WorkspacesUpdated++
				u.job.Workspaces = append(u.job.Workspaces, res)
				u.mu.Unlock()
				appendLog(fmt.Sprintf("工作区「%s」已更新至 v%s", rec.Name, cand.Version))
			}
		}
	} else {
		appendLog("当前无需更新工作区实例")
	}

	// 6. Persist post-update memory grace, backup & swap UI files, then swap gateway binary.
	setStage("updating_gateway", "正在更新门户界面与网关服务…", 85)
	if err := writePostUpdateGrace(s.cfg.StateDir, s.now().Add(postUpdateGraceDuration)); err != nil {
		appendLog("警告：写入更新后内存保护标记失败：" + err.Error())
	}
	uiBackupDir, err := swapPortalUI(u.cfg.UIDir, staged.uiDir, stamp)
	if err != nil {
		failJob(fmt.Errorf("更新门户静态资源失败：%w", err))
		return
	}
	appendLog("门户静态资源已更新（备份：" + filepath.Base(uiBackupDir) + "）")

	gwBackup := u.cfg.GatewayPath + ".prev-" + stamp
	if err := atomicReplaceExecutable(u.cfg.GatewayPath, staged.gatewayPath, gwBackup); err != nil {
		_ = restorePortalUI(u.cfg.UIDir, uiBackupDir)
		failJob(fmt.Errorf("替换网关二进制失败（已回滚门户资源）：%w", err))
		return
	}
	appendLog("网关二进制已替换（备份：" + filepath.Base(gwBackup) + "）")

	// 7. Signal restarting so the frontend starts reconnecting, then trigger exec restart.
	setStage("restarting", fmt.Sprintf("已更新至 v%s，门户服务正在平滑重启…", cand.Version), 100)
	appendLog(fmt.Sprintf("准备重启网关服务进入 v%s", cand.Version))

	if u.cfg.OnRestart != nil {
		timer := time.NewTimer(u.cfg.RestartDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		u.cfg.OnRestart(u.cfg.GatewayPath)
		return
	}
	u.mu.Lock()
	u.job.Running = false
	u.job.Phase = "done"
	u.job.Message = fmt.Sprintf("已更新至 v%s。", cand.Version)
	u.job.FinishedAt = s.now().UTC().Format(time.RFC3339)
	u.mu.Unlock()
}

func (s *Server) dropWorkspaceConnection(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replacements[id]++
	// Retain a retired workspace so a request with a live owner grant can
	// trigger re-preparation. Its cookie also identifies the old generation.
	ws := s.workspaces[id]
	if ws == nil {
		s.deleteWorkspaceTickets(id)
		return
	}
	s.markWorkspaceUnready(id, ws)
	ws.nextProbe = time.Time{}
	ws.close()
}

func sortVMRecords(list []vmRecord) {
	slices.SortFunc(list, func(a, b vmRecord) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func formatUpdateLog(now time.Time, msg string) string {
	return now.Format("15:04:05") + " " + msg
}

func (u *selfUpdater) resolveExpectedDigest(ctx context.Context, cand serverReleaseCandidate) (string, error) {
	var apiDigest string
	if cand.Bundle.Digest != "" {
		d, err := catalog.DigestValue(cand.Bundle.Digest)
		if err != nil {
			return "", err
		}
		apiDigest = d
	}
	if cand.Checksums.URL == "" {
		if apiDigest == "" {
			return "", errors.New("发行版缺少 SHA-256 校验信息")
		}
		return apiDigest, nil
	}
	parsed, err := url.Parse(cand.Checksums.URL)
	if err != nil || !u.allowedURL(parsed, false) {
		return "", errors.New("校验清单地址不受信任")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cand.Checksums.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Super-Cage-Tenant/"+u.cfg.CurrentVersion)
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 SHA256SUMS 失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 SHA256SUMS 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumFileBytes+1))
	if err != nil || int64(len(body)) > maxChecksumFileBytes {
		return "", errors.New("SHA256SUMS 响应无效或过大")
	}
	found := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != cand.Bundle.Name {
			continue
		}
		h, err := catalog.DigestValue("sha256:" + fields[0])
		if err != nil || found != "" {
			return "", errors.New("SHA256SUMS 格式无效或存在重复项")
		}
		found = h
	}
	if found == "" {
		return "", fmt.Errorf("SHA256SUMS 中未包含 %s", cand.Bundle.Name)
	}
	if apiDigest != "" && apiDigest != found {
		return "", errors.New("发行包元数据摘要与 SHA256SUMS 不一致")
	}
	return found, nil
}

func (u *selfUpdater) downloadVerifiedArchive(ctx context.Context, asset catalog.Asset, wantSHA, dst string, progress func(got, total int64)) error {
	parsed, err := url.Parse(asset.URL)
	if err != nil || !u.allowedURL(parsed, false) {
		return errors.New("安装包下载地址不受信任")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Super-Cage-Tenant/"+u.cfg.CurrentVersion)
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("下载安装包失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载安装包返回 HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	hasher := sha256.New()
	var written int64
	lastReport := time.Time{}
	buf := make([]byte, 64<<10)
	reader := io.LimitReader(resp.Body, maxBundleArchiveBytes+1)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBundleArchiveBytes {
				return errors.New("安装包超过允许大小上限")
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			_, _ = hasher.Write(buf[:n])
			if progress != nil && time.Since(lastReport) >= 250*time.Millisecond {
				lastReport = time.Now()
				progress(written, asset.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("安装包传输中断：%w", rerr)
		}
	}
	if asset.Size > 0 && written != asset.Size {
		return fmt.Errorf("安装包大小不匹配（实际 %d 字节，预期 %d 字节）", written, asset.Size)
	}
	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if gotSHA != wantSHA {
		return fmt.Errorf("安装包 SHA-256 校验失败（预期 %s，实际 %s）", wantSHA[:16], gotSHA[:16])
	}
	if progress != nil {
		progress(written, asset.Size)
	}
	return f.Sync()
}

type stagedBundle struct {
	root        string
	gatewayPath string
	cagePath    string
	uiDir       string
}

func extractServerBundle(archivePath, dstDir string) (*stagedBundle, error) {
	if err := os.MkdirAll(filepath.Join(dstDir, "ui", "tenant"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("无法解压安装包：%w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	wanted := map[string]struct {
		rel  string
		mode os.FileMode
		seen bool
	}{
		"cage-tenant":          {rel: "cage-tenant", mode: 0o755},
		"cage":                 {rel: "cage", mode: 0o755},
		"ui/tenant/index.html": {rel: filepath.Join("ui", "tenant", "index.html"), mode: 0o644},
		"ui/tenant/app.mjs":    {rel: filepath.Join("ui", "tenant", "app.mjs"), mode: 0o644},
		"ui/tenant/styles.css": {rel: filepath.Join("ui", "tenant", "styles.css"), mode: 0o644},
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档条目失败：%w", err)
		}
		name := strings.TrimPrefix(path.Clean(strings.ReplaceAll(hdr.Name, "\\", "/")), "./")
		if name == "." || name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			return nil, fmt.Errorf("归档包含非法路径：%q", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return nil, fmt.Errorf("归档包含不允许的符号链接：%q", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Strip single top-level directory (Super-Cage-NoVirt-<ver>-linux-<arch>/...).
		_, sub, ok := strings.Cut(name, "/")
		if !ok {
			continue
		}
		spec, needed := wanted[sub]
		if !needed {
			continue
		}
		if spec.seen {
			return nil, fmt.Errorf("归档包含重复文件：%s", sub)
		}
		if hdr.Size <= 0 || hdr.Size > maxExtractedFileBytes {
			return nil, fmt.Errorf("归档文件 %s 大小异常", sub)
		}
		target := filepath.Join(dstDir, spec.rel)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, spec.mode)
		if err != nil {
			return nil, err
		}
		n, copyErr := io.Copy(out, io.LimitReader(tr, hdr.Size+1))
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n != hdr.Size {
			return nil, fmt.Errorf("解压 %s 失败", sub)
		}
		spec.seen = true
		wanted[sub] = spec
	}
	for name, spec := range wanted {
		if !spec.seen {
			return nil, fmt.Errorf("安装包缺少必需文件：%s", name)
		}
	}
	return &stagedBundle{
		root:        dstDir,
		gatewayPath: filepath.Join(dstDir, "cage-tenant"),
		cagePath:    filepath.Join(dstDir, "cage"),
		uiDir:       filepath.Join(dstDir, "ui", "tenant"),
	}, nil
}

func (u *selfUpdater) verifyStagedBundle(ctx context.Context, staged *stagedBundle, targetVersion string) error {
	if u.cfg.VerifyStaged != nil {
		return u.cfg.VerifyStaged(ctx, staged.root, targetVersion)
	}
	if err := verifyLinuxELFArch(staged.gatewayPath, u.cfg.TargetArch); err != nil {
		return fmt.Errorf("cage-tenant 架构校验失败：%w", err)
	}
	if err := verifyLinuxELFArch(staged.cagePath, u.cfg.TargetArch); err != nil {
		return fmt.Errorf("cage 架构校验失败：%w", err)
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == u.cfg.TargetArch {
		verCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(verCtx, staged.cagePath, "version").CombinedOutput()
		if err != nil {
			return fmt.Errorf("新版本 cage 无法执行：%v", err)
		}
		got := strings.TrimSpace(string(out))
		if !strings.Contains(got, targetVersion) {
			return fmt.Errorf("新版本 cage 版本号不匹配（实际 %q，预期 %q）", got, targetVersion)
		}
		if out, err := exec.CommandContext(verCtx, staged.cagePath, "verify-web-assets").CombinedOutput(); err != nil {
			return fmt.Errorf("新版本 cage 缺少 Web 资源：%s", strings.TrimSpace(string(out)))
		}
		if out, err := exec.CommandContext(verCtx, staged.cagePath, "verify-vm-assets").CombinedOutput(); err != nil {
			return fmt.Errorf("新版本 cage 缺少匹配架构的 Lima 客机组件：%s", strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func verifyLinuxELFArch(binaryPath, targetArch string) error {
	f, err := os.Open(binaryPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return errors.New("文件过短，不是有效的 ELF 可执行文件")
	}
	if !bytes.Equal(hdr[:4], []byte{0x7f, 'E', 'L', 'F'}) || hdr[4] != 2 || hdr[5] != 1 {
		return errors.New("不是有效的 64 位 Linux ELF 可执行文件")
	}
	machine := binary.LittleEndian.Uint16(hdr[18:20])
	var want uint16
	switch targetArch {
	case "amd64":
		want = 62 // EM_X86_64
	case "arm64":
		want = 183 // EM_AARCH64
	default:
		return fmt.Errorf("不支持的目标架构 %s", targetArch)
	}
	if machine != want {
		return fmt.Errorf("ELF 架构不匹配（e_machine=%d，预期 %d）", machine, want)
	}
	return nil
}

func backupFileIfExists(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data, mode)
}

func writeFileAtomic(dst string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func atomicReplaceExecutable(targetPath, stagedPath, backupPath string) error {
	if err := backupFileIfExists(targetPath, backupPath, 0o755); err != nil {
		return fmt.Errorf("备份当前程序失败：%w", err)
	}
	data, err := os.ReadFile(stagedPath)
	if err != nil {
		return err
	}
	return writeFileAtomic(targetPath, data, 0o755)
}

func swapPortalUI(liveDir, stagedDir, stamp string) (string, error) {
	backupDir := liveDir + ".bak-" + stamp
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	files := []string{"index.html", "app.mjs", "styles.css"}
	for _, name := range files {
		if err := backupFileIfExists(filepath.Join(liveDir, name), filepath.Join(backupDir, name), 0o644); err != nil {
			return "", err
		}
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(stagedDir, name))
		if err != nil {
			_ = restorePortalUI(liveDir, backupDir)
			return "", err
		}
		if err := writeFileAtomic(filepath.Join(liveDir, name), data, 0o644); err != nil {
			_ = restorePortalUI(liveDir, backupDir)
			return "", err
		}
	}
	return backupDir, nil
}

func restorePortalUI(liveDir, backupDir string) error {
	var firstErr error
	for _, name := range []string{"index.html", "app.mjs", "styles.css"} {
		data, err := os.ReadFile(filepath.Join(backupDir, name))
		if err != nil {
			continue
		}
		if err := writeFileAtomic(filepath.Join(liveDir, name), data, 0o644); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type postUpdateGraceRecord struct {
	Until time.Time `json:"until"`
}

func writePostUpdateGrace(stateDir string, until time.Time) error {
	if stateDir == "" {
		return nil
	}
	data, err := json.Marshal(postUpdateGraceRecord{Until: until.UTC()})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(stateDir, postUpdateGraceFile), data, 0o600)
}

func readPostUpdateGrace(stateDir string, now time.Time) time.Time {
	if stateDir == "" {
		return time.Time{}
	}
	p := filepath.Join(stateDir, postUpdateGraceFile)
	data, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}
	}
	var rec postUpdateGraceRecord
	if json.Unmarshal(data, &rec) != nil || !now.Before(rec.Until) {
		_ = os.Remove(p)
		return time.Time{}
	}
	// Cap the grace window to prevent a stale or tampered file from holding indefinitely.
	maxUntil := now.Add(15 * time.Minute)
	if rec.Until.After(maxUntil) {
		return maxUntil
	}
	return rec.Until
}

func clearPostUpdateGrace(stateDir string) {
	if stateDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(stateDir, postUpdateGraceFile))
}

// incusWorkspaceUpdater updates the cage binary inside a running Incus VM or
// container, verifying assets before swapping and rolling back automatically
// if cage-serve fails to come back up.
type incusWorkspaceUpdater struct {
	runtime     *IncusRuntime
	cagePath    string
	serviceName string
}

func (u *incusWorkspaceUpdater) UpdateWorkspace(ctx context.Context, id, stagedCagePath, targetVersion string, logf func(string)) (bool, string, error) {
	if !vmIDPattern.MatchString(id) {
		return false, "", errors.New("无效的工作区 ID")
	}
	unlock := u.runtime.lockInstance(id)
	defer unlock()

	body, err := captureIncus(u.runtime.localCommand(ctx, "query", "local:/1.0/instances/cage-"+id+"?project="+u.runtime.cfg.Project), 1<<20)
	if err != nil {
		return false, "", errors.New("无法查询工作区实例状态")
	}
	var rec instance
	if json.Unmarshal(body, &rec) != nil || rec.Name != "cage-"+id || rec.Config[instanceLabel] != id {
		return false, "", errors.New("工作区实例归属校验失败")
	}
	if rec.Status != "Running" {
		return true, fmt.Sprintf("实例当前为 %s 状态，启动后可再次执行更新", rec.Status), nil
	}

	// Restore shrunk VM memory toward the profile ceiling before restarting so
	// the user's next environment start never stalls or OOMs on ballooned memory.
	if u.runtime.instanceType() == InstanceVM {
		u.restoreVMMemoryBeforeUpdate(ctx, id, rec, logf)
	}

	// Gracefully stop any running nested environment BEFORE replacing cage and
	// restarting cage-serve (avoids systemd KillMode=mixed SIGKILLing QEMU).
	if u.runtime.instanceType() == InstanceVM {
		logf("正在安全停止嵌套环境（如正在运行）…")
		stopCtx, stopCancel := context.WithTimeout(ctx, 90*time.Second)
		stopCmd := u.runtime.command(stopCtx, "exec", "local:cage-"+id, "--mode=non-interactive", "--",
			"/bin/sh", "-c", `if [ -x "$1" ] && id cage >/dev/null 2>&1; then su -s /bin/sh cage -c "CLAUDE_CAGE_CONFIG=\"$2\" \"$1\" stop" || true; fi`,
			"sh", u.cagePath, path.Join(path.Dir(u.runtime.cfg.ControlPath), "config.json"))
		stopOut, _ := stopCmd.CombinedOutput()
		stopCancel()
		if trimmed := strings.TrimSpace(string(stopOut)); trimmed != "" {
			lines := strings.Split(trimmed, "\n")
			logf("停止环境输出：" + lines[len(lines)-1])
		}
	}

	// Push verified binary to <cagePath>.new inside the workspace.
	logf("正在推送新版 cage 二进制到工作区…")
	pushCtx, pushCancel := context.WithTimeout(ctx, 2*time.Minute)
	pushCmd := u.runtime.command(pushCtx, "file", "push", "--mode=0755", stagedCagePath, "local:cage-"+id+u.cagePath+".new")
	pushErrBuf := &firstBytes{limit: 4096}
	pushCmd.Stderr = pushErrBuf
	pushErr := pushCmd.Run()
	pushCancel()
	if pushErr != nil {
		return false, "", fmt.Errorf("推送新版 cage 失败：%s", incusReason(pushErrBuf.String(), pushErr))
	}

	// Verify inside guest, backup, atomic swap, restart service, and auto-rollback on failure.
	logf("正在工作区内校验资源并平滑重启服务…")
	verifyVM := "0"
	if u.runtime.instanceType() == InstanceVM {
		verifyVM = "1"
	}
	swapScript := `set -e
dst="$1"
ver="$2"
svc="$3"
ctrl="$4"
verify_vm="$5"
new="${dst}.new"
prev="${dst}.prev"
dir="$(dirname "$dst")"

if [ -e "$dst" ]; then
  chown --reference="$dst" "$new" 2>/dev/null || true
fi
chmod 0755 "$new"

got_ver="$("$new" version 2>&1 || true)"
case "$got_ver" in
  *"$ver"*) ;;
  *) rm -f "$new"; echo "version mismatch: $got_ver" >&2; exit 1 ;;
esac
if ! "$new" verify-web-assets >/dev/null 2>&1; then
  rm -f "$new"; echo "verify-web-assets failed" >&2; exit 1
fi
if [ "$verify_vm" = "1" ] && ! "$new" verify-vm-assets >/dev/null 2>&1; then
  rm -f "$new"; echo "verify-vm-assets failed" >&2; exit 1
fi

if [ -e "$dst" ]; then
  cp -p "$dst" "$prev"
fi
mv -f "$new" "$dst"
printf '%s\n' "$ver" > "$dir/VERSION"

rollback() {
  if [ -e "$prev" ]; then
    mv -f "$prev" "$dst"
    systemctl restart "$svc" >/dev/null 2>&1 || true
  fi
}

if ! systemctl restart "$svc"; then
  rollback
  echo "systemctl restart $svc failed (rolled back)" >&2
  exit 1
fi

i=0
while [ "$i" -lt 25 ]; do
  if systemctl is-active --quiet "$svc" && [ -s "$ctrl" ]; then
    exit 0
  fi
  sleep 1
  i=$((i + 1))
done
rollback
echo "$svc did not become ready in time (rolled back)" >&2
exit 1
`
	swapCtx, swapCancel := context.WithTimeout(ctx, 2*time.Minute)
	swapCmd := u.runtime.command(swapCtx, "exec", "local:cage-"+id, "--mode=non-interactive", "--",
		"/bin/sh", "-c", swapScript, "sh", u.cagePath, targetVersion, u.serviceName, u.runtime.cfg.ControlPath, verifyVM)
	swapErrBuf := &firstBytes{limit: 4096}
	swapCmd.Stderr = swapErrBuf
	swapErr := swapCmd.Run()
	swapCancel()
	if swapErr != nil {
		return false, "", fmt.Errorf("切换并重启工作区服务失败：%s", incusReason(swapErrBuf.String(), swapErr))
	}
	return false, "已更新至 v" + targetVersion, nil
}

func (u *incusWorkspaceUpdater) restoreVMMemoryBeforeUpdate(ctx context.Context, id string, rec instance, logf func(string)) {
	profile, err := u.runtime.loadProfile(ctx)
	if err != nil {
		return
	}
	ceiling, ok := incusSizeBytes(profile.Config["limits.memory"])
	if !ok || ceiling <= 0 {
		return
	}
	current, ok := incusSizeBytes(rec.ExpandedConfig["limits.memory"])
	if !ok || current >= ceiling {
		return
	}
	data, err := os.ReadFile(hostMeminfo)
	if err != nil {
		return
	}
	hostTotal, hostAvail, err := parseMeminfo(data)
	if err != nil {
		return
	}
	reserve := memoryReserve(0, hostTotal)
	// Only restore within currently free host memory above reserve, rounded down to memoryUnit.
	room := hostAvail - reserve
	if room <= memoryUnit {
		return
	}
	target := min(ceiling, roundDownMemory(current+room))
	if target <= current {
		return
	}
	memCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := u.runtime.command(memCtx, "config", "set", "local:cage-"+id, fmt.Sprintf("limits.memory=%dMiB", target>>20))
	if err := cmd.Run(); err == nil {
		logf(fmt.Sprintf("已恢复工作区内存上限：%s -> %s", mib(current), mib(target)))
	}
}
