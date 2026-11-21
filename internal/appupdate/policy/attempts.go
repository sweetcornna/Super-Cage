package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Attempt records one decision to install a version. It is written before
// the environment is stopped, so an attempt that a crash or a power loss
// interrupted stays unfinished, and an unfinished attempt reads as failed.
type Attempt struct {
	Version    string     `json:"version"`
	From       string     `json:"from"`
	Trigger    Trigger    `json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Result is "ok" or "failed"; empty while the attempt has not finished.
	Result string `json:"result,omitempty"`
	Step   Step   `json:"step,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Ledger is the bounded history of attempts, newest last.
type Ledger struct {
	Attempts []Attempt `json:"attempts"`
}

const ledgerLimit = 20

func ledgerPath(profile string) string { return filepath.Join(Dir(profile), "attempts.json") }

// LoadLedger reads the history. A missing file is an empty history. An
// unreadable file returns an error: callers must then refuse unattended
// installs, because they cannot prove that a version has not failed before.
func LoadLedger(profile string) (Ledger, error) {
	data, err := readSmall(ledgerPath(profile))
	if errors.Is(err, os.ErrNotExist) {
		return Ledger{}, nil
	}
	if err != nil {
		return Ledger{}, err
	}
	var l Ledger
	if json.Unmarshal(data, &l) != nil {
		return Ledger{}, errors.New("升级记录无效")
	}
	return l, nil
}

// SaveLedger keeps the newest attempts only.
func SaveLedger(profile string, l Ledger) error {
	if len(l.Attempts) > ledgerLimit {
		l.Attempts = append([]Attempt(nil), l.Attempts[len(l.Attempts)-ledgerLimit:]...)
	}
	return writePrivate(ledgerPath(profile), l)
}

// Begin appends an unfinished attempt.
func (l *Ledger) Begin(a Attempt) { l.Attempts = append(l.Attempts, a) }

// Finish closes the newest unfinished attempt for version. It reports
// whether one was found.
func (l *Ledger) Finish(version string, ok bool, step Step, reason string, now time.Time) bool {
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		a := &l.Attempts[i]
		if a.Version != version || a.Result != "" {
			continue
		}
		a.Result, a.Step, a.Reason = "failed", step, reason
		if ok {
			a.Result, a.Reason = "ok", ""
		}
		finished := now
		a.FinishedAt = &finished
		return true
	}
	return false
}

// FailedBefore reports whether any attempt for version failed or never
// finished. Silent mode never retries such a version by itself.
func (l Ledger) FailedBefore(version string) bool {
	for _, a := range l.Attempts {
		if a.Version == version && a.Result != "ok" {
			return true
		}
	}
	return false
}

// Last is the newest attempt, or nil.
func (l Ledger) Last() *Attempt {
	if len(l.Attempts) == 0 {
		return nil
	}
	a := l.Attempts[len(l.Attempts)-1]
	return &a
}
