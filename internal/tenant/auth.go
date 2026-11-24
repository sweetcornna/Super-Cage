package tenant

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const (
	maxAccounts     = 10000
	maxSessions     = 4096
	maxUserSessions = 16
	maxRateKeys     = 8192
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)

type loginSession struct {
	user        user
	expires     time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	grants      map[string]string
	ticketCount int
}

type vmGrant struct {
	login *loginSession
	vmID  string
}

type entryTicket struct {
	grant   vmGrant
	expires time.Time
}

type rateKey struct{ group, value string }
type rateWindow struct {
	expires time.Time
	used    int
}

func randomBytes(size int) []byte {
	value := make([]byte, size)
	_, _ = rand.Read(value)
	return value
}

func secret(size int) string { return hex.EncodeToString(randomBytes(size)) }

func canonicalUsername(value string) string {
	value = strings.TrimSpace(value)
	if !usernamePattern.MatchString(value) {
		return ""
	}
	return strings.ToLower(value)
}

func hashPassword(password string, salt []byte) []byte {
	hash, err := scrypt.Key([]byte(password), salt, 1<<15, 8, 1, 32)
	if err != nil {
		panic("invalid server password hashing parameters")
	}
	return hash
}

func (s *Server) allowRate(group, value string, limit int, now time.Time) bool {
	key := rateKey{group, value}
	window := s.rates[key]
	if !now.Before(window.expires) {
		if len(s.rates) >= maxRateKeys {
			s.pruneRates(now)
			if len(s.rates) >= maxRateKeys {
				return false
			}
		}
		window = rateWindow{expires: now.Add(time.Minute)}
	}
	if window.used >= limit {
		return false
	}
	window.used++
	s.rates[key] = window
	return true
}

func (s *Server) pruneRates(now time.Time) {
	for key, window := range s.rates {
		if !now.Before(window.expires) {
			delete(s.rates, key)
		}
	}
}

func (s *Server) prune(now time.Time) {
	s.pruneRates(now)
	for token, ticket := range s.tickets {
		if !now.Before(ticket.expires) || !s.live(ticket.grant.login, now) {
			s.deleteTicket(token, ticket)
		}
	}
	for key, session := range s.sessions {
		if !s.live(session, now) {
			s.revoke(key, session)
		}
	}
}

func (s *Server) deleteTicket(token string, ticket entryTicket) {
	delete(s.tickets, token)
	ticket.grant.login.ticketCount--
}

func cookieValue(r *http.Request, name string) string {
	cookies := r.CookiesNamed(name)
	if len(cookies) == 1 && secretPattern.MatchString(cookies[0].Value) {
		return cookies[0].Value
	}
	return ""
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	cookie := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: !s.cfg.Development,
		SameSite: http.SameSiteStrictMode, Expires: expires}
	http.SetCookie(w, cookie)
}

func (s *Server) live(session *loginSession, now time.Time) bool {
	return session != nil && session.ctx.Err() == nil && now.Before(session.expires)
}

func (s *Server) current(r *http.Request) *loginSession {
	session := s.sessions[cookieValue(r, s.site.loginCookie)]
	if s.live(session, s.now()) {
		return session
	}
	return nil
}

func (s *Server) expectedSessionLocked(w http.ResponseWriter, r *http.Request) *loginSession {
	session := s.current(r)
	if session == nil {
		unauthenticated(w)
		return nil
	}
	if len(r.Header.Values("X-Cage-Expected-User")) != 0 && !exactHeader(r.Header, "X-Cage-Expected-User", session.user.ID) {
		problem(w, http.StatusConflict, "session_changed", "The signed-in account has changed. Refresh your session and try again.")
		return nil
	}
	return session
}

func (s *Server) revoke(key string, session *loginSession) {
	session.cancel()
	delete(s.sessions, key)
	for _, token := range session.grants {
		delete(s.grants, token)
	}
	for token, ticket := range s.tickets {
		if ticket.grant.login == session {
			s.deleteTicket(token, ticket)
		}
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, register bool) {
	if register && !s.cfg.Development && !s.cfg.EnablePreviewRegistration {
		problem(w, http.StatusForbidden, "registration_closed", "Self-service registration is closed. Contact the administrator for an account.")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	name := canonicalUsername(input.Username)
	if name == "" || len(input.Password) > 128 || utf8.RuneCountInString(input.Password) < 12 {
		invalidInput(w)
		return
	}
	group, ipLimit := "login", 60
	if register {
		group, ipLimit = "register", 15
	}
	s.mu.Lock()
	now := s.now()
	allowed := s.allowRate(group, s.clientIP(r), ipLimit, now)
	if !register && allowed {
		allowed = s.allowRate("login-account", name, 12, now)
	}
	record, exists := s.store.Users[name]
	s.mu.Unlock()
	if !allowed {
		rateLimited(w)
		return
	}
	select {
	case s.hashSlots <- struct{}{}:
		defer func() { <-s.hashSlots }()
	default:
		rateLimited(w)
		return
	}
	if register {
		record = userRecord{user: user{Username: name, ID: secret(16)}, Salt: randomBytes(16)}
		record.Hash = hashPassword(input.Password, record.Salt)
	} else {
		if !exists {
			record.Salt, record.Hash = make([]byte, 16), make([]byte, 32)
		}
		if subtle.ConstantTimeCompare(hashPassword(input.Password, record.Salt), record.Hash) != 1 || !exists {
			problem(w, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password.")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.context.Err() != nil || r.Context().Err() != nil || s.store.blocked {
		unavailable(w)
		return
	}
	if register {
		if _, exists := s.store.Users[name]; exists {
			problem(w, http.StatusConflict, "username_taken", "Username is already registered.")
			return
		}
		if len(s.store.Users) >= maxAccounts {
			unavailable(w)
			return
		}
	}
	now = s.now()
	s.prune(now)
	previousKey := cookieValue(r, s.site.loginCookie)
	activeCount, userCount := len(s.sessions), 0
	for key, session := range s.sessions {
		if key == previousKey {
			activeCount--
		} else if session.user.ID == record.ID {
			userCount++
		}
	}
	if activeCount >= maxSessions || userCount >= maxUserSessions {
		rateLimited(w)
		return
	}
	status := http.StatusOK
	if register {
		updated := s.store.database
		updated.Users = maps.Clone(updated.Users)
		updated.Users[name] = record
		if s.store.save(updated) != nil {
			unavailable(w)
			return
		}
		status = http.StatusCreated
	}
	if previous := s.sessions[previousKey]; previous != nil {
		s.revoke(previousKey, previous)
	}
	expires := now.Add(s.sessionTTL)
	ctx, cancel := context.WithTimeout(s.context, s.sessionTTL)
	token := secret(32)
	s.sessions[token] = &loginSession{user: record.user, expires: expires, ctx: ctx, cancel: cancel, grants: make(map[string]string)}
	s.setCookie(w, s.site.loginCookie, token, expires)
	reply := map[string]any{"user": record.user}
	if s.canSelfUpdate(record.user.Username) {
		reply["self_update"] = true
	}
	jsonReply(w, status, reply)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	session := s.current(r)
	s.mu.Unlock()
	if session == nil {
		unauthenticated(w)
		return
	}
	reply := map[string]any{"user": session.user}
	if s.canSelfUpdate(session.user.Username) {
		reply["self_update"] = true
	}
	jsonReply(w, http.StatusOK, reply)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	s.mu.Lock()
	session := s.expectedSessionLocked(w, r)
	if session != nil {
		s.revoke(cookieValue(r, s.site.loginCookie), session)
	}
	s.mu.Unlock()
	if session == nil {
		return
	}
	// A delayed response must not overwrite a newer login in another tab.
	w.WriteHeader(http.StatusNoContent)
}
