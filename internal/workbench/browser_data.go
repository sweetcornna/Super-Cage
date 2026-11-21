package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Browsing data management (docs/guide/browser.md). POST /api/browser/data
// carries the guest operations of the Linux browser controller; their
// settings (kept sites, dialog defaults, automatic cleaning) live on the host
// in <profile>/browser-data.json, served by /api/browser/data-settings.
// Guest calls follow the locking model: outside s.op, only while the
// environment is ready and no job runs. A reset restarts the browser and so
// runs as a job. Native guests and older guest controllers report the
// capability as unavailable and keep the earlier screens.

const browserDataLimit = browserResponseLimit

var (
	browserDataModes  = []string{"capabilities", "summary", "clear", "clear-sites", "job", "sites", "site", "cookie", "history", "history-delete", "reset"}
	browserDataTypes  = []string{"history", "site", "cache", "downloads"}
	browserDataRanges = map[string]time.Duration{"hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 28 * 24 * time.Hour, "all": 0}
	browserDataHost   = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$|^\[[0-9a-f:.]+\]$`)
	browserDataJobID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	browserDataKey    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errBrowserDataOld = errors.New("browser data unsupported by the guest")
	browserDataMu     sync.Mutex
)

type browserDataInput struct {
	Mode     string   `json:"mode"`
	Session  string   `json:"session,omitempty"`
	Range    string   `json:"range,omitempty"`
	Since    int64    `json:"since,omitempty"`
	Types    []string `json:"types,omitempty"`
	Protect  string   `json:"protect,omitempty"`
	Keep     []string `json:"keep,omitempty"`
	Sites    []string `json:"sites,omitempty"`
	Site     string   `json:"site,omitempty"`
	Key      string   `json:"key,omitempty"`
	Action   string   `json:"action,omitempty"`
	Query    string   `json:"query,omitempty"`
	Sort     string   `json:"sort,omitempty"`
	Page     int      `json:"page,omitempty"`
	Before   int64    `json:"before,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	URLs     []string `json:"urls,omitempty"`
	Host     string   `json:"host,omitempty"`
	Start    int64    `json:"start,omitempty"`
	End      int64    `json:"end,omitempty"`
	ID       string   `json:"id,omitempty"`
	Remember bool     `json:"remember,omitempty"`
}

type browserKeptSite struct {
	Domain string    `json:"domain"`
	Added  time.Time `json:"added"`
}

type browserDataDefaults struct {
	Range string   `json:"range"`
	Types []string `json:"types"`
}

type browserDataAuto struct {
	Enabled bool      `json:"enabled"`
	Trigger string    `json:"trigger"`
	Days    int       `json:"days"`
	Types   []string  `json:"types"`
	Last    time.Time `json:"last,omitzero"`
	Result  string    `json:"result,omitempty"`
}

type browserDataSettings struct {
	Defaults browserDataDefaults `json:"defaults"`
	Keep     []browserKeptSite   `json:"keep"`
	Auto     browserDataAuto     `json:"auto"`
}

func defaultBrowserDataSettings() browserDataSettings {
	return browserDataSettings{
		Defaults: browserDataDefaults{Range: "day", Types: []string{"history", "site", "cache"}},
		Keep:     []browserKeptSite{},
		Auto:     browserDataAuto{Trigger: "stop", Days: 7, Types: []string{"history", "site", "cache"}},
	}
}

func (s *Server) browserDataSettingsPath() string {
	return filepath.Join(s.profile, "browser-data.json")
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("file too large")
	}
	return data, err
}

func (s *Server) loadBrowserDataSettings() browserDataSettings {
	settings := defaultBrowserDataSettings()
	data, err := readLimitedFile(s.browserDataSettingsPath(), 256<<10)
	if err != nil || json.Unmarshal(data, &settings) != nil {
		return defaultBrowserDataSettings()
	}
	settings.normalize()
	return settings
}

func (b *browserDataSettings) normalize() {
	fallback := defaultBrowserDataSettings()
	if _, ok := browserDataRanges[b.Defaults.Range]; !ok {
		b.Defaults.Range = fallback.Defaults.Range
	}
	b.Defaults.Types = validBrowserDataTypes(b.Defaults.Types)
	b.Auto.Types = validBrowserDataTypes(b.Auto.Types)
	if b.Auto.Trigger != "stop" && b.Auto.Trigger != "days" {
		b.Auto.Trigger = "stop"
	}
	if !slices.Contains([]int{1, 7, 30}, b.Auto.Days) {
		b.Auto.Days = 7
	}
	kept := []browserKeptSite{}
	for _, entry := range b.Keep {
		if domain, err := browserKeptDomain(entry.Domain); err == nil && !slices.ContainsFunc(kept, func(k browserKeptSite) bool { return k.Domain == domain }) {
			kept = append(kept, browserKeptSite{Domain: domain, Added: entry.Added})
		}
	}
	b.Keep = kept
}

func validBrowserDataTypes(types []string) []string {
	result := []string{}
	for _, kind := range browserDataTypes {
		if slices.Contains(types, kind) {
			result = append(result, kind)
		}
	}
	return result
}

// A kept site is a domain: what someone types or copies (a URL, a host with a
// port, a leading "*." or "www.") becomes the host. A domain also keeps its
// subdomains, so "www." is dropped.
func browserKeptDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", errors.New("网址无效")
		}
		value = parsed.Hostname()
	}
	value = strings.TrimPrefix(strings.TrimPrefix(value, "*."), ".")
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "www."), ".")
	if value == "" || len(value) > 253 || !browserDataHost.MatchString(value) || (!strings.Contains(value, ".") && value != "localhost") {
		return "", errors.New("请输入网站域名，例如 github.com")
	}
	return value, nil
}

func browserDataSite(value string) bool {
	return value != "" && len(value) <= 253 && browserDataHost.MatchString(value)
}

func (in *browserDataInput) validate(now time.Time) error {
	invalid := browserInputError{errors.New("浏览数据操作无效")}
	if !slices.Contains(browserDataModes, in.Mode) || (in.Session != "" && in.Session != "normal" && in.Session != "private") {
		return invalid
	}
	if len(in.Query) > 200 || in.Page < 0 || in.Page > 10000 || in.Limit < 0 || in.Limit > 100 || in.Since < 0 || in.Before < 0 || in.Start < 0 || in.End < 0 {
		return invalid
	}
	if in.Sort != "" && in.Sort != "size" && in.Sort != "recent" && in.Sort != "name" {
		return invalid
	}
	// "none" clears the kept sites and the built-in pages too.
	if in.Protect != "" && (in.Mode != "clear" || (in.Protect != "kept" && in.Protect != "none")) {
		return invalid
	}
	if in.Range != "" {
		window, ok := browserDataRanges[in.Range]
		if !ok {
			return invalid
		}
		in.Since = 0
		if window > 0 {
			in.Since = now.Add(-window).UnixMilli()
		}
	}
	if in.Since > now.Add(24*time.Hour).UnixMilli() {
		return invalid
	}
	switch in.Mode {
	case "clear":
		if len(in.Types) == 0 || len(in.Types) > len(browserDataTypes) || len(validBrowserDataTypes(in.Types)) != len(in.Types) {
			return browserInputError{errors.New("请选择要清除的内容")}
		}
	case "clear-sites":
		if len(in.Sites) == 0 || len(in.Sites) > 200 {
			return browserInputError{errors.New("请选择 1 到 200 个网站")}
		}
		for _, site := range in.Sites {
			if !browserDataSite(site) {
				return invalid
			}
		}
	case "site":
		if !browserDataSite(in.Site) {
			return invalid
		}
	case "cookie":
		if !browserDataSite(in.Site) || !browserDataKey.MatchString(in.Key) || (in.Action != "value" && in.Action != "delete") {
			return invalid
		}
	case "job":
		if !browserDataJobID.MatchString(in.ID) {
			return invalid
		}
	case "history-delete":
		switch {
		case len(in.URLs) > 0:
			if len(in.URLs) > 100 || in.Host != "" || in.End != 0 {
				return invalid
			}
			for _, raw := range in.URLs {
				parsed, err := url.Parse(raw)
				if err != nil || len(raw) > 8192 || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
					return invalid
				}
			}
		case in.Host != "":
			if !browserDataSite(in.Host) || in.End != 0 {
				return invalid
			}
		default:
			if in.End <= in.Start {
				return invalid
			}
		}
	}
	return nil
}

// The body the guest receives: what it needs and nothing a client could use
// to widen an operation. Kept sites come from the host settings only.
func (in *browserDataInput) guestBody(settings browserDataSettings) map[string]any {
	body := map[string]any{"mode": in.Mode}
	if in.Session != "" {
		body["session"] = in.Session
	}
	switch in.Mode {
	case "summary":
		body["since"] = in.Since
	case "clear":
		keep := []string{}
		for _, entry := range settings.Keep {
			keep = append(keep, entry.Domain)
		}
		body["since"], body["types"], body["keep"] = in.Since, in.Types, keep
		body["keepEnabled"] = in.Protect != "none"
	case "clear-sites":
		body["sites"] = in.Sites
	case "job":
		body["id"] = in.ID
	case "sites":
		body["query"], body["sort"], body["page"] = in.Query, in.Sort, in.Page
		if in.Sort == "" {
			body["sort"] = "size"
		}
	case "site":
		body["site"], body["page"], body["query"] = in.Site, in.Page, in.Query
	case "cookie":
		body["site"], body["key"], body["action"] = in.Site, in.Key, in.Action
	case "history":
		body["query"], body["before"], body["limit"] = in.Query, in.Before, in.Limit
		if in.Limit == 0 {
			body["limit"] = 100
		}
	case "history-delete":
		if len(in.URLs) > 0 {
			body["urls"] = in.URLs
		} else if in.Host != "" {
			body["host"] = in.Host
		} else {
			body["start"], body["end"] = in.Start, in.End
		}
	}
	return body
}

type browserDataGuestError struct{ message string }

func (e browserDataGuestError) Error() string { return e.message }

// One guest call with its own bound. Guest errors are the module's own
// sentences (never a URL or a cookie) and are shown as they are.
func (s *Server) browserDataGuest(ctx context.Context, token string, body any, timeout time.Duration) ([]byte, error) {
	payload, _ := json.Marshal(body)
	response, err := s.browserClient().open(ctx, token, http.MethodPost, "/data", "application/json", bytes.NewReader(payload), int64(len(payload)), timeout)
	if err != nil {
		diagnostics.Log("warn", "browser-data", fmt.Sprintf("transport=%v", err))
		return nil, errors.New("浏览器没有响应，请确认环境仍在运行后重试")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, browserDataLimit+1))
	if err != nil || len(data) > browserDataLimit {
		return nil, errors.New("浏览数据结果不完整，请重试")
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		diagnostics.Log("warn", "browser-data", fmt.Sprintf("http=%d", response.StatusCode))
		if failure.Error == "Unknown browser action" {
			return nil, errBrowserDataOld
		}
		if failure.Error == "" || len(failure.Error) > 300 {
			failure.Error = "浏览数据操作未完成，请重试"
		}
		return nil, browserDataGuestError{failure.Error}
	}
	return data, nil
}

func (s *Server) browserData(w http.ResponseWriter, r *http.Request) {
	var input browserDataInput
	if !readJSON(w, r, &input) {
		return
	}
	if err := input.validate(time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	native := s.macosGuest != nil || s.windowsGuest != nil
	if input.Mode == "capabilities" && native {
		jsonReply(w, map[string]any{"available": false, "reason": "platform"})
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		http.Error(w, errBrowserNotReady.Error(), http.StatusConflict)
		return
	}
	if native {
		http.Error(w, "此环境的浏览器使用自己的设置页管理浏览数据", http.StatusConflict)
		return
	}
	if s.busy() {
		http.Error(w, "环境正在执行其他操作，请稍后再试", http.StatusConflict)
		return
	}
	if input.Mode == "reset" {
		s.resetBrowser(w)
		return
	}
	settings := s.loadBrowserDataSettings()
	timeout := 15 * time.Second
	if input.Mode == "history-delete" || input.Mode == "summary" || input.Mode == "sites" {
		timeout = 45 * time.Second
	}
	data, err := s.browserDataGuest(r.Context(), token, input.guestBody(settings), timeout)
	if errors.Is(err, errBrowserDataOld) {
		if input.Mode == "capabilities" {
			jsonReply(w, map[string]any{"available": false, "reason": "update"})
			return
		}
		http.Error(w, "浏览器组件不支持管理浏览数据，请更新环境组件", http.StatusConflict)
		return
	}
	if err != nil {
		status := http.StatusBadGateway
		var guest browserDataGuestError
		if errors.As(err, &guest) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if input.Mode == "clear" && input.Remember && (input.Session == "" || input.Session == "normal") {
		_ = s.updateBrowserDataSettings(func(settings *browserDataSettings) error {
			for name, window := range browserDataRanges {
				if (window == 0 && input.Since == 0) || (window > 0 && input.Range == name) {
					settings.Defaults.Range = name
				}
			}
			settings.Defaults.Types = validBrowserDataTypes(input.Types)
			return nil
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *Server) updateBrowserDataSettings(change func(*browserDataSettings) error) error {
	browserDataMu.Lock()
	defer browserDataMu.Unlock()
	settings := s.loadBrowserDataSettings()
	if err := change(&settings); err != nil {
		return err
	}
	settings.normalize()
	return writePrivate(s.browserDataSettingsPath(), settings)
}

// Host data only: it works while the environment is stopped, like favourites.
func (s *Server) browserDataSettingsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonReply(w, s.loadBrowserDataSettings())
		return
	}
	var input struct {
		Action  string   `json:"action"`
		Domain  string   `json:"domain"`
		Range   string   `json:"range"`
		Types   []string `json:"types"`
		Enabled bool     `json:"enabled"`
		Trigger string   `json:"trigger"`
		Days    int      `json:"days"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	err := s.updateBrowserDataSettings(func(settings *browserDataSettings) error {
		switch input.Action {
		case "keep-add":
			domain, err := browserKeptDomain(input.Domain)
			if err != nil {
				return browserInputError{err}
			}
			if slices.ContainsFunc(settings.Keep, func(entry browserKeptSite) bool { return entry.Domain == domain }) {
				return nil
			}
			if len(settings.Keep) >= 200 {
				return browserInputError{errors.New("最多保留 200 个网站")}
			}
			settings.Keep = append(settings.Keep, browserKeptSite{Domain: domain, Added: time.Now().UTC()})
		case "keep-remove":
			settings.Keep = slices.DeleteFunc(settings.Keep, func(entry browserKeptSite) bool { return entry.Domain == strings.ToLower(input.Domain) })
		case "defaults":
			if _, ok := browserDataRanges[input.Range]; !ok || len(validBrowserDataTypes(input.Types)) != len(input.Types) {
				return browserInputError{errors.New("默认清除范围无效")}
			}
			settings.Defaults = browserDataDefaults{Range: input.Range, Types: input.Types}
		case "auto":
			if (input.Trigger != "stop" && input.Trigger != "days") || !slices.Contains([]int{1, 7, 30}, input.Days) || len(validBrowserDataTypes(input.Types)) != len(input.Types) || (input.Enabled && len(input.Types) == 0) {
				return browserInputError{errors.New("自动清理设置无效")}
			}
			settings.Auto.Enabled, settings.Auto.Trigger, settings.Auto.Days, settings.Auto.Types = input.Enabled, input.Trigger, input.Days, input.Types
		default:
			return browserInputError{errors.New("浏览数据设置无效")}
		}
		return nil
	})
	var inputError browserInputError
	if errors.As(err, &inputError) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "保存浏览数据设置失败，请重试", http.StatusInternalServerError)
		return
	}
	jsonReply(w, s.loadBrowserDataSettings())
}

// A reset restarts the browser controller on a fresh profile. It is a job:
// the environment monitor does not take the restart for a failure, and other
// operations wait. Open sign-in pages would be lost, so they block it.
func (s *Server) resetBrowser(w http.ResponseWriter) {
	job, err := s.submitJob("browser-reset", func() error {
		ctx := s.operationContext()
		s.mu.Lock()
		token := s.credentials.Token
		s.mu.Unlock()
		s.setMessage("正在重置浏览器…")
		state, err := s.browserClient().request(ctx, token, http.MethodGet, "/tabs", nil)
		if err != nil {
			return err
		}
		var tabs struct {
			Tabs []struct {
				Login bool `json:"login"`
			} `json:"tabs"`
		}
		if json.Unmarshal(state, &tabs) != nil {
			return errors.New("浏览器状态无效，请重试")
		}
		if slices.ContainsFunc(tabs.Tabs, func(tab struct {
			Login bool `json:"login"`
		}) bool {
			return tab.Login
		}) {
			return errors.New("请先完成或关闭账号授权页面，再重置浏览器")
		}
		if _, err = s.browserDataGuest(ctx, token, map[string]any{"mode": "reset"}, 10*time.Second); err != nil {
			return err
		}
		diagnostics.Log("info", "browser-data", "reset requested")
		s.setMessage("正在以全新资料重新启动浏览器…")
		return s.waitBrowserRestart(ctx, token, 90*time.Second)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, job)
}

// The controller closes Chromium and exits; systemd starts it again after
// RestartSec. Wait for the old one to stop answering, then for the new one.
func (s *Server) waitBrowserRestart(ctx context.Context, token string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	down := false
	for time.Now().Before(deadline) {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := s.browserClient().request(probe, token, http.MethodGet, "/health", nil)
		cancel()
		if err != nil {
			down = true
		} else if down {
			diagnostics.Log("info", "browser-data", "reset finished")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return errors.New("浏览器重新启动用时过长。请稍后重新打开浏览器；仍无法使用时重启环境")
}

// Automatic cleaning, off by default. Called inside the stop job (Linux,
// before the guest stops) and after a start; "days" runs at the first of
// either once the interval has passed. Best effort and bounded: a failure is
// recorded and never fails the stop or the start.
func (s *Server) browserDataAutoClean(trigger string) {
	settings := s.loadBrowserDataSettings()
	auto := settings.Auto
	if !auto.Enabled || len(auto.Types) == 0 || s.macosGuest != nil || s.windowsGuest != nil {
		return
	}
	if auto.Trigger == "stop" && trigger != "stop" {
		return
	}
	if auto.Trigger == "days" && !auto.Last.IsZero() && time.Since(auto.Last) < time.Duration(auto.Days)*24*time.Hour {
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		return
	}
	ctx, cancel := context.WithTimeout(s.operationContext(), 20*time.Second)
	defer cancel()
	input := browserDataInput{Mode: "clear", Types: auto.Types}
	result := "failed"
	data, err := s.browserDataGuest(ctx, token, input.guestBody(settings), 10*time.Second)
	var started struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err == nil && json.Unmarshal(data, &started) == nil && started.Job.ID != "" {
		for ctx.Err() == nil {
			data, err = s.browserDataGuest(ctx, token, map[string]any{"mode": "job", "id": started.Job.ID}, 5*time.Second)
			var status struct {
				Job struct {
					State  string `json:"state"`
					Result struct {
						FreedBytes int64 `json:"freedBytes"`
					} `json:"result"`
				} `json:"job"`
			}
			if err != nil || json.Unmarshal(data, &status) != nil {
				break
			}
			if status.Job.State != "running" {
				result = status.Job.State
				diagnostics.Log("info", "browser-data", fmt.Sprintf("auto-clean trigger=%s types=%s state=%s freed=%d", trigger, strings.Join(auto.Types, ","), result, status.Job.Result.FreedBytes))
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if result != "done" {
		diagnostics.Log("warn", "browser-data", fmt.Sprintf("auto-clean trigger=%s state=%s", trigger, result))
	}
	_ = s.updateBrowserDataSettings(func(settings *browserDataSettings) error {
		settings.Auto.Result = result
		if result == "done" {
			settings.Auto.Last = time.Now().UTC()
		}
		return nil
	})
}
