package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Attempt records one decision to install a version, or one unattended
// request handed to the native shell. A decision is written before the
// environment is stopped, so an attempt that a crash or a power loss
// interrupted stays unfinished, and an unfinished attempt reads as failed.
type Attempt struct {
	// ID is the chain record's id for a decided installation, and the
	// request's id for an unattended request.
	ID         string     `json:"id,omitempty"`
	Version    string     `json:"version"`
	From       string     `json:"from"`
	Trigger    Trigger    `json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Result is "ok" or "failed"; empty while the attempt has not finished.
	Result string `json:"result,omitempty"`
	Step   Step   `json:"step,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Stage is empty for a decided installation. For an unattended request
	// it says how far the shell got: requested (nothing reported yet),
	// download, verify or prepare (it failed there), changed (the release
	// changed under it), or decided (it reached the decision).
	Stage string `json:"stage,omitempty"`
	// Release identifies the published files an unattended request was for.
	Release string `json:"release,omitempty"`
}

// Stages of an unattended request.
const (
	StageRequested = "requested"
	StageDownload  = "download"
	StageVerify    = "verify"
	StagePrepare   = "prepare"
	StageChanged   = "changed"
	StageDecided   = "decided"
)

// RetrySchedule is how long silent mode waits before it hands the same
// release to the shell again after requests that ended before the decision
// for any reason other than a failed verification (no report at all, a
// network failure, a local failure): after the first such request one hour,
// after the second six, after the third a day; after the fourth it waits for
// another release. A failed verification of the files is never retried
// unattended. A click is never limited by this.
var RetrySchedule = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}

// Ledger is the bounded history of attempts and requests, newest last.
type Ledger struct {
	Attempts []Attempt `json:"attempts"`
	// LostHistory is set when an unreadable record was moved aside: the
	// versions that failed before are unknown, so silent mode waits for one
	// successful update before it installs anything by itself.
	LostHistory bool `json:"lost_history,omitempty"`
}

// ledgerLimit bounds the decided attempts and, separately, the requests
// kept, so that requests never push a failed version out of the history.
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

// SetLedgerAside moves an unreadable history out of the way, keeping it next
// to the new one, and returns the empty history that replaces it.
func SetLedgerAside(profile string, now time.Time) (Ledger, error) {
	aside := filepath.Join(Dir(profile), "attempts.unreadable-"+now.UTC().Format("20060102T150405.000000000")+".json")
	if err := os.Rename(ledgerPath(profile), aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Ledger{}, err
	}
	return Ledger{LostHistory: true}, nil
}

// SaveLedger keeps the newest attempts and the newest requests only.
func SaveLedger(profile string, l Ledger) error {
	decided, requests := 0, 0
	keep := make([]bool, len(l.Attempts))
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		count := &decided
		if l.Attempts[i].Stage != "" {
			count = &requests
		}
		if *count < ledgerLimit {
			keep[i] = true
			*count++
		}
	}
	kept := make([]Attempt, 0, decided+requests)
	for i, a := range l.Attempts {
		if keep[i] {
			kept = append(kept, a)
		}
	}
	l.Attempts = kept
	return writePrivate(ledgerPath(profile), l)
}

// Begin appends an unfinished attempt.
func (l *Ledger) Begin(a Attempt) { l.Attempts = append(l.Attempts, a) }

// Finish closes the decided attempt id, or, without an id, the newest
// unfinished decided attempt for version. It reports whether one was found.
func (l *Ledger) Finish(id, version string, ok bool, step Step, reason string, now time.Time) bool {
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		a := &l.Attempts[i]
		if a.Stage != "" || a.Result != "" || (id != "" && a.ID != id) || (id == "" && a.Version != version) {
			continue
		}
		a.Result, a.Step, a.Reason = "failed", step, reason
		if ok {
			a.Result, a.Reason = "ok", ""
			l.LostHistory = false
		}
		finished := now
		a.FinishedAt = &finished
		return true
	}
	return false
}

// Undo records that an update which had completed (attempt id, or the newest
// attempt for version) was rolled back afterwards: a failure of that version.
func (l *Ledger) Undo(id, version, reason string, now time.Time) bool {
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		a := &l.Attempts[i]
		if a.Stage != "" || (id != "" && a.ID != id) || (id == "" && a.Version != version) {
			continue
		}
		finished := now
		a.Result, a.Step, a.Reason, a.FinishedAt = "failed", StepDone, reason, &finished
		return true
	}
	return false
}

// FailedBefore reports whether any decided attempt for version failed or
// never finished. Silent mode never retries such a version by itself.
func (l Ledger) FailedBefore(version string) bool {
	for _, a := range l.Attempts {
		if a.Stage == "" && a.Version == version && a.Result != "ok" {
			return true
		}
	}
	return false
}

// Request records an unattended request handed to the shell. It counts
// against RetrySchedule until the shell reaches the decision.
func (l *Ledger) Request(id, version, from, release string, now time.Time) {
	l.Begin(Attempt{ID: id, Version: version, From: from, Trigger: SilentUpdate, StartedAt: now, Stage: StageRequested, Release: release})
}

// Report records how a request ended before the decision (a Stage* value).
// It reports whether the request was found.
func (l *Ledger) Report(id, stage, reason string, now time.Time) bool {
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		a := &l.Attempts[i]
		if a.ID != id || a.Stage != StageRequested {
			continue
		}
		finished := now
		a.Stage, a.Reason, a.FinishedAt = stage, reason, &finished
		a.Result = "failed"
		if stage == StageDecided || stage == StageChanged {
			a.Result = ""
		}
		return true
	}
	return false
}

// SilentRetry says when silent mode may hand release (of version) to the
// shell again: at once for files it never asked for, never after their
// verification failed, and otherwise as RetrySchedule says.
func (l Ledger) SilentRetry(version, release string, now time.Time) (time.Time, bool) {
	var n int
	var last time.Time
	for _, a := range l.Attempts {
		if a.Stage == "" || a.Version != version || a.Release != release {
			continue
		}
		switch a.Stage {
		case StageVerify:
			return time.Time{}, false
		case StageDecided, StageChanged:
			continue
		}
		n++
		if a.StartedAt.After(last) {
			last = a.StartedAt
		}
	}
	switch {
	case n == 0:
		return now, true
	case n > len(RetrySchedule):
		return time.Time{}, false
	}
	return last.Add(RetrySchedule[n-1]), true
}

// Last is the newest decided attempt, or nil.
func (l Ledger) Last() *Attempt {
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		if l.Attempts[i].Stage == "" {
			a := l.Attempts[i]
			return &a
		}
	}
	return nil
}
