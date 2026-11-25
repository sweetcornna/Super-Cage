package workbench

import (
	"bytes"
	"cmp"
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

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Browsing data management (docs/guide/browser.md). POST /api/browser/data
// carries the guest operations of the Linux browser controller; their
// settings (kept sites, dialog defaults, automatic cleaning) live on the host
// in <profile>/browser-data.json, served by /api/browser/data-settings.
// Guest calls follow the locking model: outside s.op, only while the
// environment is ready and no job runs. A reset restarts the browser and so
// runs as a job, and so does a clear (browser-clear): stop, reset, a network
// check and other windows wait for it, and every window follows the same one.
// Native guests and older guest controllers report the capability as
// unavailable and keep the earlier screens.

const browserDataLimit = browserResponseLimit

var (
	browserDataModes  = []string{"capabilities", "summary", "clear", "clear-sites", "job", "sites", "site", "cookie", "history", "history-url", "history-delete", "reset"}
	browserDataTypes  = []string{"history", "site", "cache", "downloads"}
	browserDataRanges = map[string]time.Duration{"hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 28 * 24 * time.Hour, "all": 0}
	browserDataHost   = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$|^\[[0-9a-f:.]+\]$`)
	browserDataJobID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	browserDataKey    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	browserHistoryID  = regexp.MustCompile(`^[0-9]{1,20}$`)
	errBrowserDataOld = errors.New("浏览器组件不支持管理浏览数据，请更新环境组件")
	// errBrowserDataNoReply: no reply came back, so the request may or may
	// not have reached the controller.
	errBrowserDataNoReply = errors.New("浏览器没有响应，请确认环境仍在运行后重试")
	// errBrowserDataSettingsUnreadable is a settings file that exists but
	// cannot be read or understood. It is left as it is: only a missing file
	// means the defaults, so the kept sites are never replaced by none.
	errBrowserDataSettingsUnreadable = errors.New("浏览数据设置暂时读不出来，请稍后重试；保持登录的网站列表没有改动")
	browserDataMu                    sync.Mutex
	// browserRestartLimit bounds the wait for a reset's restart and
	// browserClearLimit a clear's; tests shorten them.
	browserRestartLimit = 90 * time.Second
	// A controller that has not gone down this long after a reset request
	// whose reply was lost never took the request.
	browserResetUnanswered = 25 * time.Second
	browserClearLimit      = 150 * time.Second
)

type browserDataInput struct {
	Mode     string                `json:"mode"`
	Session  string                `json:"session,omitempty"`
	Range    string                `json:"range,omitempty"`
	Since    int64                 `json:"since,omitempty"`
	Types    []string              `json:"types,omitempty"`
	Protect  string                `json:"protect,omitempty"`
	Keep     []string              `json:"keep,omitempty"`
	Sites    []string              `json:"sites,omitempty"`
	Site     string                `json:"site,omitempty"`
	Key      string                `json:"key,omitempty"`
	Action   string                `json:"action,omitempty"`
	Query    string                `json:"query,omitempty"`
	Sort     string                `json:"sort,omitempty"`
	Page     int                   `json:"page,omitempty"`
	Before   int64                 `json:"before,omitempty"`
	Limit    int                   `json:"limit,omitempty"`
	URLs     []string              `json:"urls,omitempty"`
	Entries  []browserHistoryEntry `json:"entries,omitempty"`
	Time     float64               `json:"time,omitempty"`
	Host     string                `json:"host,omitempty"`
	Start    int64                 `json:"start,omitempty"`
	End      int64                 `json:"end,omitempty"`
	ID       string                `json:"id,omitempty"`
	Remember bool                  `json:"remember,omitempty"`
}

// A history entry by its id and last visit, for an address too long to send
// whole (the guest cuts addresses past 2048 characters in its replies).
type browserHistoryEntry struct {
	ID   string  `json:"id"`
	Time float64 `json:"time"`
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
	Enabled bool     `json:"enabled"`
	Trigger string   `json:"trigger"`
	Days    int      `json:"days"`
	Types   []string `json:"types"`
	// What the host stores about the last automatic run, all of it made by
	// the host: the time of the last success, the time and outcome of the last
	// attempt (done, failed, timeout or unsupported) and the bytes it freed.
	Last      time.Time `json:"last,omitzero"`
	Attempted time.Time `json:"attempted,omitzero"`
	Result    string    `json:"result,omitempty"`
	Freed     int64     `json:"freed,omitempty"`
}

var browserAutoResults = []string{"done", "failed", "timeout", "unsupported"}

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

// loadBrowserDataSettings reads the host settings. A missing file is the
// defaults; any other failure (permissions, size, format, a field of the
// wrong type) is errBrowserDataSettingsUnreadable, and the file stays as it
// is: callers do not write over it.
func (s *Server) loadBrowserDataSettings() (browserDataSettings, error) {
	data, err := readLimitedFile(s.browserDataSettingsPath(), 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return defaultBrowserDataSettings(), nil
	}
	settings := defaultBrowserDataSettings()
	if err == nil {
		err = json.Unmarshal(data, &settings)
	}
	if err != nil {
		fault := "format"
		switch {
		case errors.Is(err, os.ErrPermission):
			fault = "permission"
		case err.Error() == "file too large":
			fault = "size"
		case data == nil:
			fault = "read"
		}
		diagnostics.Log("warn", "browser-data", "settings unreadable: "+fault)
		return browserDataSettings{}, errBrowserDataSettingsUnreadable
	}
	settings.normalize()
	return settings, nil
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
	if !slices.Contains(browserAutoResults, b.Auto.Result) {
		b.Auto.Result = ""
	}
	b.Auto.Freed = min(max(b.Auto.Freed, 0), 1<<50)
	// Stored entries are checked for form only: the rules for adding one
	// (browserKeptDomain) may grow, and a kept site must not vanish because
	// of it.
	kept := []browserKeptSite{}
	for _, entry := range b.Keep {
		if domain, err := browserDomainForm(entry.Domain); err == nil && !slices.ContainsFunc(kept, func(k browserKeptSite) bool { return k.Domain == domain }) {
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
// subdomains, so "www." is dropped. A host Chromium cannot take as an origin
// (a last label of digits that is not a full IPv4 address) and a public
// suffix are refused.
func browserKeptDomain(value string) (string, error) {
	domain, err := browserDomainForm(value)
	if err != nil {
		return "", err
	}
	if !browserHostUsable(domain) {
		return "", errors.New("请输入网站域名或完整的 IP 地址，例如 github.com 或 192.168.1.10")
	}
	if browserPublicSuffix(domain) {
		return "", fmt.Errorf("%s 是公共后缀，下面是许多互不相干的网站，不能整个保留。请输入具体的网站，例如 example.%s", domain, domain)
	}
	return domain, nil
}

// Public suffixes refused as kept domains, since keeping one keeps every
// site under it: a generic second level under a two-letter country code
// (co.uk, com.cn, ac.jp, …) and common hosting suffixes, the same lists the
// guest groups sites by (browser-data.mjs siteOf). The full public suffix
// list has thousands more; those are not recognised (docs/guide/browser.md).
var (
	browserHostingSuffixes = []string{"github.io", "gitlab.io", "vercel.app", "netlify.app", "pages.dev", "workers.dev", "herokuapp.com",
		"appspot.com", "web.app", "firebaseapp.com", "blogspot.com", "azurewebsites.net", "cloudfront.net", "onrender.com", "fly.dev", "glitch.me"}
	browserSecondLevels = []string{"com", "net", "org", "gov", "edu", "ac", "co", "ne", "or", "go", "mil", "gen", "biz", "info", "nom", "ltd", "plc", "sch"}
)

func browserPublicSuffix(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) != 2 {
		return false
	}
	return slices.Contains(browserHostingSuffixes, domain) ||
		(len(labels[1]) == 2 && strings.Trim(labels[1], "abcdefghijklmnopqrstuvwxyz") == "" && slices.Contains(browserSecondLevels, labels[0]))
}

// A host whose last label is all digits is read as an IPv4 address by
// browsers; only a full dotted quad is one (Chromium refuses a removal for
// "https://foo.123" or "https://1.2.3" whole).
func browserHostUsable(host string) bool {
	labels := strings.Split(host, ".")
	if last := labels[len(labels)-1]; last == "" || strings.Trim(last, "0123456789") != "" {
		return true
	}
	ip := net.ParseIP(host)
	return len(labels) == 4 && ip != nil && ip.To4() != nil
}

// browserDomainForm is the form of a kept domain, without the rules for
// adding one.
func browserDomainForm(value string) (string, error) {
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
	// No IPv6 form: the guest cannot match one to the origins Chromium keeps.
	if value == "" || len(value) > 253 || !browserDataHost.MatchString(value) || strings.ContainsAny(value, "[]:") || (!strings.Contains(value, ".") && value != "localhost") {
		return "", errors.New("请输入网站域名，例如 github.com")
	}
	return value, nil
}

// A site as the guest names it: a host, or an IPv6 address with or without
// brackets.
func browserDataSite(value string) bool {
	if strings.Contains(value, ":") {
		return len(value) <= 64 && net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")) != nil
	}
	return value != "" && len(value) <= 253 && browserDataHost.MatchString(value) && browserHostUsable(value)
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
	case "history-url":
		if !browserHistoryID.MatchString(in.ID) || in.Time < 0 || in.Time > 1e15 {
			return invalid
		}
	case "history-delete":
		switch {
		case len(in.Entries) > 0:
			if len(in.Entries) > 100 || len(in.URLs) > 0 || in.Host != "" || in.End != 0 {
				return invalid
			}
			for _, entry := range in.Entries {
				if !browserHistoryID.MatchString(entry.ID) || entry.Time < 0 || entry.Time > 1e15 {
					return invalid
				}
			}
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
// clearKey is the clear a request asks for: what it removes, from when and
// with or without the kept list. Two equal keys are one clear.
func (in *browserDataInput) clearKey() string {
	types, sites := slices.Clone(in.Types), slices.Clone(in.Sites)
	slices.Sort(types)
	slices.Sort(sites)
	since := in.Since
	if in.Range != "" {
		since = 0
	}
	key, _ := json.Marshal([]any{in.Mode, cmp.Or(in.Session, "normal"), in.Range, since, types, in.Protect == "none", sites})
	return string(key)
}

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
	case "history-url":
		body["id"], body["time"] = in.ID, in.Time
	case "history-delete":
		if len(in.Entries) > 0 {
			body["entries"] = in.Entries
		} else if len(in.URLs) > 0 {
			body["urls"] = in.URLs
		} else if in.Host != "" {
			body["host"] = in.Host
		} else {
			body["start"], body["end"] = in.Start, in.End
		}
	}
	return body
}

// A regular clear of site data or the cache excludes the kept sites (the
// guest hands them to Chromium for both), so it needs the settings to be
// readable. History and downloads do not.
func (in *browserDataInput) needsKeptSites() bool {
	return in.Mode == "clear" && in.Protect != "none" && in.Session != "private" && (slices.Contains(in.Types, "site") || slices.Contains(in.Types, "cache"))
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
		return nil, errBrowserDataNoReply
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, browserDataLimit+1))
	if err != nil {
		return nil, errBrowserDataNoReply
	}
	if len(data) > browserDataLimit {
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
	// The clear a browser-clear job runs is followed here, also while the
	// host is busy with it, and read back after it ended.
	if input.Mode == "job" {
		if reply := s.browserClears.status(input.ID); reply != nil {
			writeJSONBytes(w, reply)
			return
		}
	}
	if !ready {
		http.Error(w, errBrowserNotReady.Error(), http.StatusConflict)
		return
	}
	if native {
		http.Error(w, "此环境的浏览器使用自己的设置页管理浏览数据", http.StatusConflict)
		return
	}
	settings, settingsErr := s.loadBrowserDataSettings()
	if settingsErr != nil && input.needsKeptSites() {
		http.Error(w, settingsErr.Error(), http.StatusServiceUnavailable)
		return
	}
	if input.Mode == "clear" || input.Mode == "clear-sites" {
		s.browserClear(w, token, input, settings)
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
	timeout := 15 * time.Second
	if input.Mode == "history-delete" || input.Mode == "summary" || input.Mode == "sites" {
		timeout = 45 * time.Second
	}
	data, err := s.browserDataGuest(r.Context(), token, input.guestBody(settings), timeout)
	if errors.Is(err, errBrowserDataOld) && input.Mode == "capabilities" {
		jsonReply(w, map[string]any{"available": false, "reason": "update"})
		return
	}
	if err != nil {
		browserDataFailure(w, err)
		return
	}
	writeJSONBytes(w, data)
}

func writeJSONBytes(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// browserDataFailure answers a failed guest call: an older controller and the
// guest's own sentences as a conflict, a lost guest as a bad gateway.
func browserDataFailure(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var guest browserDataGuestError
	if errors.Is(err, errBrowserDataOld) || errors.As(err, &guest) {
		status = http.StatusConflict
	}
	http.Error(w, err.Error(), status)
}

// browserClearTracker follows the clear a browser-clear job runs: the host
// job, the guest's job id and the guest's latest reply, or a failure the host
// wrote when it lost track.
type browserClearTracker struct {
	mu    sync.Mutex
	job   string
	key   string // the request the running clear stands for (clearKey)
	id    string
	reply []byte
}

// begin names the clear a host job runs before the guest has a job for it.
func (t *browserClearTracker) begin(job, key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.job, t.key, t.id, t.reply = job, key, "", nil
}

// keyFor is the request of the clear the host job runs, once it has begun.
func (t *browserClearTracker) keyFor(job string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.key, t.job != "" && t.job == job
}

func (t *browserClearTracker) set(job, id string, reply []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != job {
		t.key = ""
	}
	t.job, t.id, t.reply = job, id, reply
}

func (t *browserClearTracker) status(id string) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id == "" || t.id != id {
		return nil
	}
	return t.reply
}

func (t *browserClearTracker) forJob(job string) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == "" || t.job != job {
		return nil
	}
	return t.reply
}

// fail records an outcome the host decided (never guest text) and returns it.
func (t *browserClearTracker) fail(job, id, message string) error {
	reply, _ := json.Marshal(map[string]any{"job": map[string]any{"id": id, "state": "failed", "error": message}})
	t.set(job, id, reply)
	return errors.New(message)
}

type browserDataJobReply struct {
	Job struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Result struct {
			FreedBytes float64 `json:"freedBytes"`
		} `json:"result"`
	} `json:"job"`
}

// browserClear starts a clear as a host job and answers with the guest's
// job, which the window then polls. A clear asked for while another runs
// (another window, a double click) follows that one instead.
func (s *Server) browserClear(w http.ResponseWriter, token string, input browserDataInput, settings browserDataSettings) {
	body, key := input.guestBody(settings), input.clearKey()
	started := make(chan error, 1)
	job, err := s.submitJob("browser-clear", func() error { return s.runBrowserClear(token, key, body, started) })
	if err != nil {
		// A second window asking for the same clear follows the running one;
		// a different clear waits for it to finish.
		if running := s.jobs.snapshot(); errors.Is(err, errJobBusy) && running.Running && running.Kind == "browser-clear" {
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				runningKey, begun := s.browserClears.keyFor(running.ID)
				if begun && runningKey != key {
					http.Error(w, "另一项清除正在进行，完成后再试", http.StatusConflict)
					return
				}
				if reply := s.browserClears.forJob(running.ID); begun && reply != nil {
					writeJSONBytes(w, reply)
					return
				}
			}
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	select {
	case err = <-started:
	case <-time.After(30 * time.Second):
		err = errors.New("浏览器没有响应，请确认环境仍在运行后重试")
	}
	if err != nil {
		browserDataFailure(w, err)
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
	writeJSONBytes(w, s.browserClears.forJob(job.ID))
}

// runBrowserClear is the browser-clear job: it starts the guest's clear and
// follows it to the end.
func (s *Server) runBrowserClear(token, key string, body map[string]any, started chan<- error) error {
	execution := s.jobs.execution()
	s.browserClears.begin(execution.id, key)
	s.setMessage("正在清除浏览数据…")
	data, err := s.browserDataGuest(execution.ctx, token, body, 15*time.Second)
	var reply browserDataJobReply
	if err == nil && (json.Unmarshal(data, &reply) != nil || !browserDataJobID.MatchString(reply.Job.ID)) {
		err = errors.New("浏览数据结果无效，请重试")
	}
	if err != nil {
		started <- err
		return err
	}
	s.browserClears.set(execution.id, reply.Job.ID, data)
	started <- nil
	return s.followBrowserClear(token, execution.id, reply.Job.ID)
}

// followBrowserClear polls the guest's clear to its end on a context of its
// own, bounded by browserClearLimit: a stop that preempts this job waits for
// the clear rather than stopping Chromium in the middle of it.
func (s *Server) followBrowserClear(token, job, id string) error {
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), browserClearLimit)
	defer cancel()
	lease := s.browserLease("正在清除浏览数据…")
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return s.browserClears.fail(job, id, "清除用时过长，结果未知。请稍后重新打开查看")
		case <-time.After(250 * time.Millisecond):
		}
		lease.check()
		data, err := s.browserDataGuest(ctx, token, map[string]any{"mode": "job", "id": id}, 10*time.Second)
		var status browserDataJobReply
		if err == nil && json.Unmarshal(data, &status) == nil && status.Job.ID == id {
			failures = 0
			s.browserClears.set(job, id, data)
			switch status.Job.State {
			case "running":
				continue
			case "done":
				return nil
			}
			return errors.New("浏览数据没有全部清除，详情见浏览数据窗口")
		}
		if failures++; failures >= 3 {
			return s.browserClears.fail(job, id, "浏览器没有响应，清除结果未知。请稍后重新打开查看")
		}
	}
}

// browserLeaseWatch notes, in the running job's message, when the network
// gate's lease ran out while a browser job waited: the monitor does not renew
// it during a job, so networking closes until the full recheck the monitor
// schedules once the job is over.
type browserLeaseWatch struct {
	s        *Server
	deadline time.Time
	message  string
	noted    bool
}

func (s *Server) browserLease(message string) *browserLeaseWatch {
	watch := &browserLeaseWatch{s: s, message: message}
	s.mu.Lock()
	if gate := s.safety.Gate; gate != nil && gate.Mode != cage.GateClosed && !gate.Until.IsZero() {
		watch.deadline = gate.Deadline()
	}
	s.mu.Unlock()
	return watch
}

func (l *browserLeaseWatch) check() {
	if l.noted || l.deadline.IsZero() || time.Now().Before(l.deadline) {
		return
	}
	l.noted = true
	l.s.setMessage(l.message + "联网授权已到期，外网暂时关闭，完成后会自动重新检查联网")
	diagnostics.Log("warn", "browser-data", "network lease ran out during a browser job")
}

func (s *Server) updateBrowserDataSettings(change func(*browserDataSettings) error) error {
	browserDataMu.Lock()
	defer browserDataMu.Unlock()
	settings, err := s.loadBrowserDataSettings()
	if err != nil {
		return err
	}
	if err := change(&settings); err != nil {
		return err
	}
	settings.normalize()
	return writePrivate(s.browserDataSettingsPath(), settings)
}

// Host data only: it works while the environment is stopped, like favourites.
func (s *Server) browserDataSettingsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settings, err := s.loadBrowserDataSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		jsonReply(w, settings)
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
	switch {
	case errors.As(err, &inputError):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, errBrowserDataSettingsUnreadable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, "保存浏览数据设置失败，请重试", http.StatusInternalServerError)
		return
	}
	settings, err := s.loadBrowserDataSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonReply(w, settings)
}

// A reset restarts the browser controller on a fresh profile. It is a job:
// the environment monitor does not take the restart for a failure, and other
// operations wait. Open sign-in pages would be lost, so they block it. Once
// the controller accepted it, the reset happens whatever the job does next:
// the job follows the restart on its own bound (a stop waits for it), re-
// attaches the new controller as after a crash and reports the outcome the
// controller wrote; when it gives up it cancels the request.
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
		id := randomToken()[:16]
		accepted := true
		if _, err = s.browserDataGuest(ctx, token, map[string]any{"mode": "reset", "id": id}, 10*time.Second); err != nil {
			if errors.Is(err, errBrowserDataOld) {
				return errors.New("浏览器组件不支持重置，请先更新环境组件")
			}
			if !errors.Is(err, errBrowserDataNoReply) {
				return err
			}
			// The reply was lost: the controller may have taken the request
			// and be restarting, so it is followed as an accepted one.
			accepted = false
			diagnostics.Log("warn", "browser-data", "reset reply lost")
		}
		diagnostics.Log("info", "browser-data", "reset requested")
		// The controller restarts now: until it is back, the host and the
		// page treat the browser as not ready, as after a controller crash.
		s.mu.Lock()
		s.ready = false
		s.mu.Unlock()
		const restarting = "正在以全新资料重新启动浏览器…"
		s.setMessage(restarting)
		wait, cancel := context.WithTimeout(s.lifetimeContext(), browserRestartLimit)
		defer cancel()
		restarted, err := s.waitBrowserRestart(wait, token, s.browserLease(restarting), accepted)
		if err != nil {
			giveUp, done := context.WithTimeout(s.lifetimeContext(), 5*time.Second)
			defer done()
			_, _ = s.browserDataGuest(giveUp, token, map[string]any{"mode": "reset-cancel", "id": id}, 5*time.Second)
			diagnostics.Log("warn", "browser-data", "reset restart timed out")
			return errors.New("浏览器重新启动用时过长，暂时无法确认是否已重置。浏览器恢复后会自动重新连接，请到时再查看")
		}
		if !restarted {
			// The controller never went down: the request did not take
			// effect, and must not at a later start either.
			_, _ = s.browserDataGuest(wait, token, map[string]any{"mode": "reset-cancel", "id": id}, 5*time.Second)
			if err := s.reattachBrowser(wait, token); err != nil {
				return err
			}
			return errors.New("浏览器没有收到重置请求，资料没有改动。请重试")
		}
		if err := s.reattachBrowser(wait, token); err != nil {
			return err
		}
		var outcome struct {
			State string `json:"state"`
		}
		data, err := s.browserDataGuest(wait, token, map[string]any{"mode": "reset-status", "id": id}, 5*time.Second)
		if err != nil || json.Unmarshal(data, &outcome) != nil {
			return errors.New("浏览器已重新启动，但无法确认是否已重置，请重新打开浏览数据查看")
		}
		diagnostics.Log("info", "browser-data", "reset outcome="+map[string]string{"done": "done", "failed": "failed", "expired": "expired"}[outcome.State])
		switch outcome.State {
		case "done":
			return nil
		case "failed":
			return errors.New("浏览器资料未能重置（客机磁盘空间或权限问题），仍在使用原来的资料。请重试；仍失败时重启环境")
		case "incomplete":
			return errors.New("浏览器资料没能完整重置（客机磁盘空间或权限问题），原来的资料仍保留在环境中，没有删除。请重启环境后重试")
		}
		return errors.New("浏览器重新启动了，但没有完成重置，仍在使用原来的资料。请重试")
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, job)
}

// The controller closes Chromium and exits (or stops a Chromium that does
// not exit); systemd starts it again after RestartSec. Wait for the old one
// to stop answering, then for the new one.
func (s *Server) waitBrowserRestart(ctx context.Context, token string, lease *browserLeaseWatch, accepted bool) (bool, error) {
	down, began := false, time.Now()
	for {
		lease.check()
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := s.browserClient().request(probe, token, http.MethodGet, "/health", nil)
		cancel()
		switch {
		case err != nil:
			down = true
		case down:
			return true, nil
		case !accepted && time.Since(began) >= browserResetUnanswered:
			// A request whose reply was lost and that the controller never
			// acted on (it kills Chromium 20 s after a reset at the latest).
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// reattachBrowser follows a restarted browser controller as attach does
// after a controller crash: the homepage again, ready again (the page
// rebuilds its event stream when ready changes) and the window's visibility
// sent again, since a new controller starts with its window shown.
func (s *Server) reattachBrowser(ctx context.Context, token string) error {
	s.mu.Lock()
	home := browserHomeURL(s.preferences)
	s.mu.Unlock()
	if err := s.setBrowserHome(ctx, token, home); err != nil {
		return errors.New("浏览器已重新启动，但暂时连不上，稍后会自动重新连接")
	}
	s.mu.Lock()
	newlyReady := !s.ready
	s.ready = true
	if newlyReady {
		s.wakeMonitorsLocked()
	}
	s.mu.Unlock()
	if newlyReady {
		s.kickClaudeLaunchSchedule()
	}
	s.browserViews.reset()
	s.browserViews.background(s.changeBrowserVisibility)
	return nil
}

// Automatic cleaning, off by default. Called inside the stop job (Linux,
// before the guest stops) and after a start; "days" runs at the first of
// either once the interval has passed. Best effort and bounded: a failure is
// recorded and never fails the stop or the start. At stop the pages are not
// reloaded: a page reloaded then only stores new data before Chromium shuts
// down. What is stored about the run is made by the host.
func (s *Server) browserDataAutoClean(trigger string) {
	settings, err := s.loadBrowserDataSettings()
	if err != nil {
		diagnostics.Log("warn", "browser-data", "auto-clean skipped: settings unreadable")
		return
	}
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
	s.setMessage("正在自动清理浏览数据…")
	ctx, cancel := context.WithTimeout(s.operationContext(), 20*time.Second)
	defer cancel()
	input := browserDataInput{Mode: "clear", Types: auto.Types}
	body := input.guestBody(settings)
	if trigger == "stop" {
		body["reload"] = false
	}
	result, freed := "failed", int64(0)
	data, err := s.browserDataGuest(ctx, token, body, 10*time.Second)
	var started browserDataJobReply
	if errors.Is(err, errBrowserDataOld) {
		result = "unsupported"
	} else if err == nil && json.Unmarshal(data, &started) == nil && browserDataJobID.MatchString(started.Job.ID) {
		for {
			data, err = s.browserDataGuest(ctx, token, map[string]any{"mode": "job", "id": started.Job.ID}, 5*time.Second)
			var status browserDataJobReply
			if err != nil || json.Unmarshal(data, &status) != nil {
				break
			}
			if status.Job.State != "running" {
				if status.Job.State == "done" {
					result, freed = "done", int64(min(max(status.Job.Result.FreedBytes, 0), 1<<50))
				}
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	if result != "done" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result = "timeout"
	}
	diagnostics.Log("info", "browser-data", fmt.Sprintf("auto-clean trigger=%s types=%s result=%s freed=%d", trigger, strings.Join(auto.Types, ","), result, freed))
	now := time.Now().UTC()
	_ = s.updateBrowserDataSettings(func(settings *browserDataSettings) error {
		settings.Auto.Result, settings.Auto.Attempted, settings.Auto.Freed = result, now, freed
		if result == "done" {
			settings.Auto.Last = now
		}
		return nil
	})
}
