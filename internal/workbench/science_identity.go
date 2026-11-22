package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Claude Science phase 2: the identity the Science frame must present is the
// guest browser's, never the host's (owner's condition). This file holds the
// guest identity (read from the guest browser the gate confirms), the probe
// comparison (layer 2) and the outbound audit (layer 3).

// scienceIdentity is what the Windows shell applies to the frame through CDP
// and what the probe must read back in every context.
type scienceIdentity struct {
	Timezone string `json:"timezone"`
	// Locale is the ICU locale for Emulation.setLocaleOverride.
	Locale string `json:"locale"`
	// Language and Languages are navigator.language(s) in the guest browser.
	Language  string   `json:"language"`
	Languages []string `json:"languages"`
	// AcceptLanguage is for the CDP override (which also yields
	// navigator.languages); UpstreamAcceptLanguage is the header the guest's
	// Chromium itself sends, which the proxy sets on every upstream request.
	AcceptLanguage         string            `json:"accept_language"`
	UpstreamAcceptLanguage string            `json:"upstream_accept_language"`
	UserAgent              string            `json:"user_agent"`
	Platform               string            `json:"platform"`
	Metadata               scienceUAMetadata `json:"ua_metadata"`
}

// scienceUAMetadata is CDP Emulation.UserAgentMetadata. Only the low-entropy
// hints are observed in the guest; the rest are the fixed values of an x86-64
// Linux Chromium.
type scienceUAMetadata struct {
	Brands          []scienceUABrand `json:"brands"`
	FullVersionList []scienceUABrand `json:"fullVersionList"`
	Platform        string           `json:"platform"`
	PlatformVersion string           `json:"platformVersion"`
	Architecture    string           `json:"architecture"`
	Model           string           `json:"model"`
	Mobile          bool             `json:"mobile"`
	Bitness         string           `json:"bitness"`
}

type scienceUABrand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

// scienceGuestAudit is the subset of the browser agent's /audit answer
// (browser-audit.mjs readBrowserSettings with device:true) used here.
type scienceGuestAudit struct {
	Timezone         string   `json:"timezone"`
	ExpectedTimezone string   `json:"expected_timezone"`
	Language         string   `json:"language"`
	Languages        []string `json:"languages"`
	Platform         string   `json:"platform"`
	Device           *struct {
		UserAgent   string `json:"user_agent"`
		Platform    string `json:"platform"`
		ClientHints *struct {
			Brands   []scienceUABrand `json:"brands"`
			Platform string           `json:"platform"`
			Mobile   bool             `json:"mobile"`
		} `json:"client_hints"`
	} `json:"device"`
}

var (
	scienceLanguageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
	scienceZoneName    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(?:/[A-Za-z0-9_+\-]+)*$`)
)

// scienceAcceptLanguage mirrors Chromium's expansion of --accept-lang: each
// regional tag is followed by its base language unless listed, with q-values
// falling by 0.1 ("ja-JP" -> "ja-JP,ja;q=0.9").
func scienceAcceptLanguage(languages []string) string {
	var expanded []string
	for _, tag := range languages {
		if !slices.Contains(expanded, tag) {
			expanded = append(expanded, tag)
		}
		if base, _, regional := strings.Cut(tag, "-"); regional && !slices.Contains(languages, base) && !slices.Contains(expanded, base) {
			expanded = append(expanded, base)
		}
	}
	parts := make([]string, 0, len(expanded))
	for i, tag := range expanded {
		if i == 0 {
			parts = append(parts, tag)
			continue
		}
		q := 10 - i
		if q < 1 {
			q = 1
		}
		parts = append(parts, fmt.Sprintf("%s;q=0.%d", tag, q))
	}
	return strings.Join(parts, ",")
}

// identityFromGuestAudit builds the identity from the guest browser's own
// answer, refusing anything the gate would not confirm for that browser.
func identityFromGuestAudit(audit scienceGuestAudit, timezone, browserLanguage string) (scienceIdentity, error) {
	if audit.Device == nil || audit.Device.UserAgent == "" {
		return scienceIdentity{}, errors.New("隔离浏览器没有返回它的浏览器标识，请更新环境组件后重试")
	}
	if timezone == "" || !sameScienceZone(audit.Timezone, timezone) || !sameScienceZone(audit.ExpectedTimezone, timezone) || !scienceZoneName.MatchString(timezone) {
		return scienceIdentity{}, errors.New("隔离浏览器的时区与已确认的环境时区不一致，请重新检测网络后再试")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return scienceIdentity{}, errors.New("无法识别环境时区，请重新检测网络后再试")
	}
	japanese := strings.EqualFold(browserLanguage, "ja-JP") && strings.EqualFold(audit.Language, "ja")
	if audit.Language == "" || (!strings.EqualFold(audit.Language, browserLanguage) && !japanese) || !scienceLanguageTag.MatchString(audit.Language) {
		return scienceIdentity{}, errors.New("隔离浏览器的语言与已确认的环境语言不一致，请重新检测网络后再试")
	}
	languages := audit.Languages
	if len(languages) == 0 {
		languages = []string{audit.Language}
	}
	if len(languages) > 8 {
		return scienceIdentity{}, errors.New("隔离浏览器返回的语言列表无效")
	}
	for _, tag := range languages {
		if !scienceLanguageTag.MatchString(tag) {
			return scienceIdentity{}, errors.New("隔离浏览器返回的语言列表无效")
		}
	}
	ua := audit.Device.UserAgent
	if !strings.HasPrefix(audit.Platform, "Linux") || !strings.Contains(ua, "X11; Linux") || len(ua) > 512 || strings.ContainsAny(ua, "\x00\r\n\"\\") {
		return scienceIdentity{}, errors.New("隔离浏览器的系统标识不是 Linux，无法使用本机渲染")
	}
	metadata := scienceUAMetadata{Platform: "Linux", Architecture: "x86", Bitness: "64", Brands: []scienceUABrand{}, FullVersionList: []scienceUABrand{}}
	if hints := audit.Device.ClientHints; hints != nil {
		if hints.Platform != "" && hints.Platform != "Linux" {
			return scienceIdentity{}, errors.New("隔离浏览器的系统标识不是 Linux，无法使用本机渲染")
		}
		if len(hints.Brands) > 8 {
			return scienceIdentity{}, errors.New("隔离浏览器返回的品牌信息无效")
		}
		for _, brand := range hints.Brands {
			if len(brand.Brand) > 64 || len(brand.Version) > 32 || strings.ContainsAny(brand.Brand+brand.Version, "\x00\r\n\"\\") {
				return scienceIdentity{}, errors.New("隔离浏览器返回的品牌信息无效")
			}
		}
		metadata.Brands, metadata.Mobile = append(metadata.Brands, hints.Brands...), hints.Mobile
	}
	return scienceIdentity{
		Timezone: timezone, Locale: audit.Language, Language: audit.Language, Languages: languages,
		AcceptLanguage: strings.Join(languages, ","), UpstreamAcceptLanguage: scienceAcceptLanguage([]string{audit.Language}),
		UserAgent: ua, Platform: audit.Platform, Metadata: metadata,
	}, nil
}

// readScienceIdentity asks the guest browser (the one the gate checks) for
// its identity. There is no fallback: without a live answer host rendering is
// unavailable.
func (s *Server) readScienceIdentity(ctx context.Context) (scienceIdentity, error) {
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return scienceIdentity{}, err
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		return scienceIdentity{}, errors.New("隔离浏览器仍在启动，请稍后再试")
	}
	body, _ := json.Marshal(map[string]any{"network": false, "timezone": cfg.Timezone, "device": true})
	data, err := s.guestAPI(ctx, token, "POST", "/audit", body)
	if err != nil {
		return scienceIdentity{}, errors.New("暂时读不到隔离浏览器的时区与语言，请稍后重试")
	}
	var audit scienceGuestAudit
	if json.Unmarshal(data, &audit) != nil {
		return scienceIdentity{}, errors.New("隔离浏览器返回的身份信息无效，请更新环境组件后重试")
	}
	return identityFromGuestAudit(audit, cfg.Timezone, cfg.BrowserLanguage)
}

// chromeZoneName is the name Chrome's Intl reports for zone (round-2 N8): an
// override of Asia/Kolkata reads back as Asia/Calcutta.
func chromeZoneName(zone string) string {
	if name, ok := scienceChromeZone[zone]; ok {
		return name
	}
	return zone
}

// sameScienceZone reports whether two names are one zone as Chrome sees it.
// Chrome reports both names the same way, so a page cannot tell them apart.
func sameScienceZone(a, b string) bool { return a != "" && chromeZoneName(a) == chromeZoneName(b) }

// scienceZoneNames lists the names a page may use for zone, for the audit:
// zone itself, Chrome's name for it and the other area/location names Chrome
// reports the same way (bare names such as GMT or Zulu would match ordinary
// text).
func scienceZoneNames(zone string) []string {
	names := []string{zone}
	reported := chromeZoneName(zone)
	if reported != zone {
		names = append(names, reported)
	}
	for name, other := range scienceChromeZone {
		if other == reported && name != zone && strings.Contains(name, "/") {
			names = append(names, name)
		}
	}
	slices.Sort(names[1:])
	return names
}

// jsOffset is new Date().getTimezoneOffset() in zone at now: minutes, positive
// west of UTC.
func jsOffset(zone string, now time.Time) (int, bool) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return 0, false
	}
	_, seconds := now.In(location).Zone()
	return -seconds / 60, true
}

// scienceProbeReport is what one context of the probe page reports.
type scienceProbeReport struct {
	Context      string   `json:"ctx"`
	Timezone     string   `json:"tz"`
	Offset       int      `json:"offset"`
	Language     string   `json:"language"`
	Languages    []string `json:"languages"`
	UserAgent    string   `json:"ua"`
	Platform     string   `json:"platform"`
	UAPlatform   *string  `json:"ua_platform"`
	SharedWorker bool     `json:"shared_worker"`
	// RTC (main context only) is the number of ICE candidates the page could
	// gather: 0 when the shell's WebRTC restriction is in force, -1 when
	// there is no RTCPeerConnection. Anything else fails the probe.
	RTC *int `json:"rtc,omitempty"`
}

var scienceProbeContexts = []string{"main", "nested", "dedicated", "blob"}

// checkScienceProbe is layer 2: every context must report the guest's values.
// The returned reason is Chinese and names the first context and value that
// differ; it never echoes the reported value.
func checkScienceProbe(guest scienceIdentity, reports []scienceProbeReport, now time.Time) string {
	names := map[string]string{"main": "页面", "nested": "嵌套页面", "dedicated": "后台线程", "blob": "临时后台线程"}
	byContext := map[string]scienceProbeReport{}
	for _, report := range reports {
		if _, known := names[report.Context]; !known {
			return "核对结果包含未知的页面环境"
		}
		if _, seen := byContext[report.Context]; seen {
			return "核对结果重复"
		}
		byContext[report.Context] = report
	}
	offset, ok := jsOffset(guest.Timezone, now)
	if !ok {
		return "无法识别环境时区"
	}
	for _, name := range scienceProbeContexts {
		report, present := byContext[name]
		label := names[name]
		switch {
		case !present:
			return label + "没有完成身份核对"
		case !sameScienceZone(report.Timezone, guest.Timezone):
			return label + "看到的时区不是环境时区"
		case report.Offset != offset:
			return label + "看到的 UTC 偏移不是环境的"
		case report.Language != guest.Language || !slices.Equal(report.Languages, guest.Languages):
			return label + "看到的语言不是环境语言"
		case report.UserAgent != guest.UserAgent:
			return label + "看到的浏览器标识不是环境浏览器的"
		case report.Platform != guest.Platform:
			return label + "看到的系统平台不是环境的"
		case report.UAPlatform != nil && *report.UAPlatform != guest.Metadata.Platform:
			return label + "的 Client Hints 平台不是环境的"
		case report.SharedWorker:
			return label + "仍可创建共享后台线程，其时区与语言无法替换"
		case name == "main" && (report.RTC == nil || (*report.RTC != 0 && *report.RTC != -1)):
			return "页面仍能直接建立网络连接（WebRTC），本机渲染需要的网络限制没有生效"
		}
	}
	return ""
}

// scienceHostIdentity is what the host WebView reports without overrides:
// measured by the Windows shell in the top document before it applies them.
type scienceHostIdentity struct {
	Timezone  string   `json:"timezone"`
	Offset    *int     `json:"offset"`
	Languages []string `json:"languages"`
}

func (h scienceHostIdentity) valid() bool {
	if h.Timezone != "" && (!scienceZoneName.MatchString(h.Timezone) || len(h.Timezone) > 64) {
		return false
	}
	if h.Offset != nil && (*h.Offset < -900 || *h.Offset > 900) {
		return false
	}
	if len(h.Languages) > 8 {
		return false
	}
	for _, tag := range h.Languages {
		if !scienceLanguageTag.MatchString(tag) {
			return false
		}
	}
	return true
}

// scienceAudit is layer 3: it refuses outbound data that carries a host
// value the guest does not share. Patterns are narrow on purpose (whole zone
// names, offsets in date notation or under offset-like keys, regional
// language tags as whole tokens); typed text that happens to contain them is
// refused too, which the status explains.
type scienceAudit struct {
	zone     []string
	offsets  []*regexp.Regexp
	locales  []*regexp.Regexp
	Distinct scienceAuditDistinct
}

// scienceAuditDistinct says which host values differ from the guest's. A
// value that is equal cannot be told apart and is not searched for.
type scienceAuditDistinct struct {
	Timezone bool `json:"timezone"`
	Offset   bool `json:"offset"`
	Locale   bool `json:"locale"`
	// Known is false when the host values were not measured.
	Known bool `json:"known"`
}

func newScienceAudit(host scienceHostIdentity, guest scienceIdentity, now time.Time) *scienceAudit {
	audit := &scienceAudit{}
	if host.Timezone == "" && host.Offset == nil && len(host.Languages) == 0 {
		return audit
	}
	audit.Distinct.Known = true
	if host.Timezone != "" && !sameScienceZone(host.Timezone, guest.Timezone) {
		audit.Distinct.Timezone = true
		for _, name := range scienceZoneNames(host.Timezone) {
			audit.zone = append(audit.zone, name, strings.ReplaceAll(name, "/", `\/`), strings.ReplaceAll(name, "/", "%2F"), strings.ReplaceAll(name, "/", "%2f"))
		}
	}
	guestOffset, _ := jsOffset(guest.Timezone, now)
	if host.Offset != nil && *host.Offset != guestOffset {
		audit.Distinct.Offset = true
		east := -*host.Offset
		sign := "+"
		if east < 0 {
			sign, east = "-", -east
		}
		hours, minutes := east/60, east%60
		quoted := regexp.QuoteMeta
		hhmm, hh := fmt.Sprintf("%02d:?%02d", hours, minutes), fmt.Sprintf("%02d%02d", hours, minutes)
		audit.offsets = []*regexp.Regexp{
			// Date.prototype.toString / toTimeString: "GMT+0800".
			regexp.MustCompile(`(?i)\b(?:GMT|UTC)` + quoted(sign) + `(?:` + hhmm + `|` + hh + `)\b`),
			// An ISO time with this offset: "23:53:45+08:00".
			regexp.MustCompile(`\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?` + quoted(sign) + hhmm + `\b`),
			// JSON or form fields named like an offset holding the host value in
			// minutes (either sign convention) or seconds.
			regexp.MustCompile(fmt.Sprintf(`(?i)(?:offset|tzo|utcoffset)["']?\s*[:=]\s*["']?(?:%d|%d|%d)\b`, *host.Offset, -*host.Offset, -*host.Offset*60)),
		}
	}
	for _, tag := range host.Languages {
		if slices.ContainsFunc(guest.Languages, func(g string) bool { return strings.EqualFold(g, tag) }) || strings.EqualFold(tag, guest.Language) {
			continue
		}
		base, region, regional := strings.Cut(tag, "-")
		if !regional {
			continue
		}
		audit.Distinct.Locale = true
		audit.locales = append(audit.locales, regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9-])`+regexp.QuoteMeta(base)+`[-_]`+regexp.QuoteMeta(region)+`(?:$|[^A-Za-z0-9-])`))
	}
	return audit
}

// scan returns the category of the first host value found, or "".
func (a *scienceAudit) scan(data []byte) string {
	if a == nil || len(data) == 0 {
		return ""
	}
	// No copy of data: the socket audit scans every chunk.
	for _, zone := range a.zone {
		if bytes.Contains(data, []byte(zone)) {
			return "timezone"
		}
	}
	for _, pattern := range a.offsets {
		if pattern.Match(data) {
			return "offset"
		}
	}
	for _, pattern := range a.locales {
		if pattern.Match(data) {
			return "locale"
		}
	}
	return ""
}

// window is how much of a previous chunk a streaming scan keeps.
func (a *scienceAudit) window() int { return 96 }

var scienceAuditMessages = map[string]string{
	"timezone": "已拦截一次发往 Claude Science 的请求：其中含有本机时区名称。",
	"offset":   "已拦截一次发往 Claude Science 的请求：其中含有本机的 UTC 偏移。",
	"locale":   "已拦截一次发往 Claude Science 的请求：其中含有本机的语言区域。",
}
