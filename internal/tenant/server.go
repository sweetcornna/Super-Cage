package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Runtime interface {
	Prepare(context.Context, LaunchSpec) (*Workspace, error)
}

type LaunchSpec struct {
	VMID     string
	SystemID string
	Image    string
}

type Config struct {
	Origin                    string
	Development               bool
	EnablePreviewRegistration bool
	LegacyDebianImage         string
	StateDir                  string
	UI                        fs.FS
	Runtime                   Runtime
	Systems                   []System
	GuestAccount              *GuestAccount
	AccountVMLimit            int
	TotalVMLimit              int
	ParallelStarts            int
	TrustLoopbackClientIP     bool
	// FreezeAfter is how long a workspace may stay idle before it is frozen.
	// Zero turns idle cooling off. StopAfter, counted on the same idle clock,
	// shuts a frozen workspace down and keeps its disk; zero keeps it frozen.
	// BusyCPU is the share of one CPU above which a running instance counts
	// as active even without browser traffic.
	FreezeAfter time.Duration
	StopAfter   time.Duration
	BusyCPU     float64
	// MemoryInterval is how often running VMs are resized to fit the host;
	// zero turns elastic memory off. MemoryReserve is the host memory kept
	// for the host itself (zero: the larger of 2 GiB and 15% of host memory)
	// and MemoryMin the smallest size given to a VM (zero: DefaultMemoryMin).
	MemoryInterval time.Duration
	MemoryReserve  int64
	MemoryMin      int64
	// SelfUpdate configures optional admin-gated one-click updates from the
	// web portal. Disabled unless Explicitly enabled with admin usernames.
	SelfUpdate SelfUpdateConfig
}

type Server struct {
	site          site
	cfg           Config
	context       context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	workers       sync.WaitGroup
	closeOnce     sync.Once
	store         *store
	sessions      map[string]*loginSession
	grants        map[string]vmGrant
	tickets       map[string]entryTicket
	rates         map[rateKey]rateWindow
	workspaces    map[string]*Workspace
	systems       map[string]System
	defaultSystem System
	starting      map[string]bool
	startFailed   map[string]bool
	startSlots    chan struct{}
	hashSlots     chan struct{}
	now           func() time.Time
	sessionTTL    time.Duration
	cooling       cooling
	memory        memoryBalancing
	selfUpdate    *selfUpdater
}

func NewServer(cfg Config) (*Server, error) {
	site, err := parseSite(cfg.Origin, cfg.Development)
	if err != nil {
		return nil, err
	}
	if cfg.AccountVMLimit < 1 || cfg.TotalVMLimit < 1 || cfg.ParallelStarts < 1 || cfg.ParallelStarts > 32 {
		return nil, errors.New("VM limits must be positive and parallel starts must be between 1 and 32")
	}
	if cfg.Runtime == nil || cfg.UI == nil {
		return nil, errors.New("a runtime and portal UI are required")
	}
	if cfg.FreezeAfter < 0 || (cfg.FreezeAfter > 0 && cfg.FreezeAfter < time.Minute) || cfg.StopAfter < 0 || (cfg.StopAfter > 0 && cfg.StopAfter < cfg.FreezeAfter) || cfg.BusyCPU < 0 || cfg.BusyCPU > 64 {
		return nil, errors.New("freeze-after must be 0 or at least one minute, stop-after must be 0 or no shorter than freeze-after, and the busy CPU share must be between 0 and 64")
	}
	if (cfg.MemoryInterval != 0 && (cfg.MemoryInterval < 5*time.Second || cfg.MemoryInterval > 10*time.Minute)) || cfg.MemoryReserve < 0 || (cfg.MemoryMin != 0 && cfg.MemoryMin < MinimumMemory) {
		return nil, errors.New("memory-interval must be 0 or between 5s and 10m, memory-reserve must not be negative, and memory-min must be at least 1GiB")
	}
	updater, err := newSelfUpdater(cfg.SelfUpdate, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	systems, err := systemIndex(cfg.Systems)
	if err != nil {
		return nil, err
	}
	cfg.Systems = slices.Clone(cfg.Systems)
	if cfg.LegacyDebianImage != "" && !secretPattern.MatchString(cfg.LegacyDebianImage) {
		return nil, errors.New("legacy Debian image must be a full Incus fingerprint")
	}
	state, err := openStore(cfg.StateDir, cfg.LegacyDebianImage)
	if err != nil {
		return nil, err
	}
	if cfg.GuestAccount == nil {
		// New blank deployments get a managed guest account by default. The
		// private per-deployment secret is created beside the tenant state and
		// reused across restarts; an explicit -guest-account-file overrides it.
		cfg.GuestAccount, err = loadOrCreateGuestAccount(cfg.StateDir)
		if err != nil {
			state.close()
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		site: site, cfg: cfg, context: ctx, cancel: cancel, store: state,
		sessions: make(map[string]*loginSession), grants: make(map[string]vmGrant), tickets: make(map[string]entryTicket),
		rates: make(map[rateKey]rateWindow), workspaces: make(map[string]*Workspace), starting: make(map[string]bool), systems: systems,
		startFailed: make(map[string]bool),
		startSlots:  make(chan struct{}, cfg.ParallelStarts), hashSlots: make(chan struct{}, 4),
		now: time.Now, sessionTTL: 24 * time.Hour,
		selfUpdate: updater,
	}
	if len(cfg.Systems) != 0 {
		server.defaultSystem = cfg.Systems[0]
	}
	server.workers.Go(server.maintain)
	server.startCooling()
	server.startMemoryBalancer()
	return server, nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.cancel()
		s.mu.Unlock()
		s.workers.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, workspace := range s.workspaces {
			workspace.close()
		}
		s.store.close()
	})
}

func (s *Server) maintain() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.context.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.prune(s.now())
			s.mu.Unlock()
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	baseHeaders(w.Header())
	if r.URL.IsAbs() || r.URL.Host != "" || !validPath(r.URL) || r.Method == http.MethodConnect {
		problem(w, http.StatusBadRequest, "invalid_input", "Invalid request target.")
		return
	}
	if r.Host == s.site.host {
		s.portal(w, r)
		return
	}
	if id := s.site.vmID(r.Host); id != "" {
		s.workbench(w, r, id)
		return
	}
	problem(w, http.StatusForbidden, "origin_refused", "Request host is not allowed.")
}

func baseHeaders(headers http.Header) {
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Referrer-Policy", "no-referrer")
}

func problem(w http.ResponseWriter, status int, code, message string) {
	jsonReply(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func jsonReply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func unavailable(w http.ResponseWriter) {
	problem(w, http.StatusServiceUnavailable, "runtime_unavailable", "The service or workspace is temporarily unavailable.")
}

func invalidInput(w http.ResponseWriter) {
	problem(w, http.StatusBadRequest, "invalid_input", "Invalid request fields.")
}

func notFound(w http.ResponseWriter) {
	problem(w, http.StatusNotFound, "not_found", "Resource not found.")
}

func vmNotFound(w http.ResponseWriter) {
	problem(w, http.StatusNotFound, "vm_not_found", "Workspace not found.")
}

func unauthenticated(w http.ResponseWriter) {
	problem(w, http.StatusUnauthorized, "unauthenticated", "A valid sign-in is required.")
}

func rateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	problem(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again shortly.")
}

func refuseOrigin(w http.ResponseWriter) {
	problem(w, http.StatusForbidden, "origin_refused", "Request origin is not allowed.")
}

func exactHeader(headers http.Header, name, value string) bool {
	values := headers.Values(name)
	return len(values) == 1 && values[0] == value
}

func safeMethod(method string) bool { return method == http.MethodGet || method == http.MethodHead }

func ownSource(r *http.Request, origin string, requireOrigin bool) bool {
	if (requireOrigin || len(r.Header.Values("Origin")) != 0) && !exactHeader(r.Header, "Origin", origin) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	default:
		return false
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || len(r.Header.Values("Content-Type")) != 1 || contentType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		problem(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Use application/json with UTF-8.")
		return false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err == nil {
		_ = controller.SetReadDeadline(time.Time{})
	}
	if err != nil || !utf8.Valid(body) || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		invalidInput(w)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		invalidInput(w)
		return false
	}
	return true
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	address := net.ParseIP(host)
	if s.cfg.TrustLoopbackClientIP && address != nil && address.IsLoopback() {
		if forwarded := net.ParseIP(r.Header.Get("X-Real-IP")); forwarded != nil && len(r.Header.Values("X-Real-IP")) == 1 {
			return forwarded.String()
		}
	}
	return host
}

func (s *Server) portal(w http.ResponseWriter, r *http.Request) {
	if !safeMethod(r.Method) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; manifest-src 'self'; frame-src "+s.site.scheme+"://*."+s.site.name+s.site.portSuffix+"; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	if !ownSource(r, s.site.origin, !safeMethod(r.Method)) {
		refuseOrigin(w)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") && safeMethod(r.Method) {
		name, contentType := "", ""
		switch r.URL.Path {
		case "/", "/index.html":
			name, contentType = "index.html", "text/html; charset=utf-8"
		case "/app.mjs":
			name, contentType = "app.mjs", "text/javascript; charset=utf-8"
		case "/styles.css":
			name, contentType = "styles.css", "text/css; charset=utf-8"
		}
		if name == "" && servePortalIcon(w, r) {
			return
		}
		if name != "" {
			body, err := fs.ReadFile(s.cfg.UI, name)
			if err != nil {
				unavailable(w)
				return
			}
			servePortalFile(w, r, contentType, body)
			return
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /api/tenant/register":
		s.authenticate(w, r, true)
	case "POST /api/tenant/login":
		s.authenticate(w, r, false)
	case "GET /api/tenant/session":
		s.session(w, r)
	case "POST /api/tenant/logout":
		s.logout(w, r)
	case "GET /api/tenant/systems":
		s.listSystems(w, r)
	case "GET /api/tenant/vms":
		s.listVMs(w, r)
	case "POST /api/tenant/vms":
		s.createVM(w, r)
	case "GET /api/tenant/update":
		s.handleGetSelfUpdate(w, r)
	case "POST /api/tenant/update/check":
		s.handleCheckSelfUpdate(w, r)
	case "POST /api/tenant/update/apply":
		s.handleApplySelfUpdate(w, r)
	default:
		const prefix = "/api/tenant/vms/"
		if id, ok := strings.CutPrefix(r.URL.Path, prefix); ok && r.Method == http.MethodPost && strings.HasSuffix(id, "/open") {
			s.openVM(w, r, strings.TrimSuffix(id, "/open"))
			return
		}
		notFound(w)
	}
}
