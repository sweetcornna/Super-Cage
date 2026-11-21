package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Blocker is one reason not to install now, named for the person who sees it.
type Blocker struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Interruptible blockers may be interrupted after the user confirmed the
	// list. Others (a running operation, a storage recovery) must finish.
	Interruptible bool `json:"interruptible"`
	// Unknown marks a state that could not be read. It always counts as busy.
	Unknown bool `json:"unknown,omitempty"`
}

// Blocker kinds shown by the UI.
const (
	KindJob         = "job"
	KindStorage     = "storage"
	KindTurn        = "turn"
	KindTerminal    = "terminal"
	KindAgent       = "agent"
	KindApp         = "app"
	KindViewer      = "viewer"
	KindAudio       = "audio"
	KindTransfer    = "transfer"
	KindPhone       = "phone"
	KindRemote      = "remote"
	KindInput       = "input"
	KindEnvironment = "environment"
	KindExternal    = "external"
	KindUnknown     = "unknown"
)

// Reading is what one feeder reports: its blockers and the monotonic facts
// (ids, counters) that change whenever its kind of work starts or ends.
type Reading struct {
	Blockers []Blocker
	Activity []string
}

// Feeder reads one source from state the workbench already holds. It must
// not poll the guest; a decision-time read is registered separately.
type Feeder func(ctx context.Context) (Reading, error)

type feeder struct {
	name, unknown string
	read          Feeder
	decisionOnly  bool
}

// Registry collects the blockers every feature feeds.
type Registry struct {
	mu       sync.Mutex
	feeders  []feeder
	holds    map[string]Blocker
	activity atomic.Uint64
}

// Register adds a feeder read on every snapshot. unknownLabel is shown when
// the feeder fails ("无法确认<source>的状态").
func (r *Registry) Register(name, unknownLabel string, read Feeder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.feeders = append(r.feeders, feeder{name: name, unknown: unknownLabel, read: read})
}

// RegisterDecision adds a feeder read only for a decision or a confirmation
// list: one bounded read of the guest or of the host's input idle time.
func (r *Registry) RegisterDecision(name, unknownLabel string, read Feeder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.feeders = append(r.feeders, feeder{name: name, unknown: unknownLabel, read: read, decisionOnly: true})
}

// Hold registers an external session (for example an SSH entry into the
// guest) as a blocker until release is called. Holding also counts as
// activity, so idle must hold again after it ends.
func (r *Registry) Hold(id, label string, interruptible bool) (release func()) {
	r.mu.Lock()
	if r.holds == nil {
		r.holds = map[string]Blocker{}
	}
	key := "external:" + id
	r.holds[key] = Blocker{ID: key, Kind: KindExternal, Label: label, Interruptible: interruptible}
	r.mu.Unlock()
	r.Touch()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.holds, key)
			r.mu.Unlock()
			r.Touch()
		})
	}
}

// Touch records that some work was admitted. Admission points call it so
// that work which starts and ends between two snapshots still changes the
// fingerprint.
func (r *Registry) Touch() { r.activity.Add(1) }

// Snapshot is the registry's view at one moment.
type Snapshot struct {
	At          time.Time `json:"at"`
	Blockers    []Blocker `json:"blockers"`
	Fingerprint string    `json:"-"`
	// Decision is true when the decision-only feeders were read.
	Decision bool `json:"-"`
}

// Idle reports whether nothing, known or unknown, blocks an installation.
func (s Snapshot) Idle() bool { return len(s.Blockers) == 0 }

// Snapshot reads every ordinary feeder and, when decision is true, also the
// decision-only feeders. A feeder that errors, panics or outlives ctx yields
// an unknown blocker. The fingerprint covers only activity facts, which the
// decision-only feeders do not report, so a timer's snapshot and a
// decision's snapshot of the same idle machine have the same fingerprint.
func (r *Registry) Snapshot(ctx context.Context, now time.Time, decision bool) Snapshot {
	r.mu.Lock()
	feeders := append([]feeder(nil), r.feeders...)
	holds := make([]Blocker, 0, len(r.holds))
	for _, b := range r.holds {
		holds = append(holds, b)
	}
	r.mu.Unlock()
	snapshot := Snapshot{At: now, Decision: decision}
	var activity []string
	for _, f := range feeders {
		if f.decisionOnly && !decision {
			continue
		}
		reading, err := readFeeder(ctx, f)
		if err != nil {
			snapshot.Blockers = append(snapshot.Blockers, Blocker{ID: "unknown:" + f.name, Kind: KindUnknown, Label: f.unknown, Interruptible: true, Unknown: true})
			continue
		}
		snapshot.Blockers = append(snapshot.Blockers, reading.Blockers...)
		for _, fact := range reading.Activity {
			activity = append(activity, f.name+"\x00"+fact)
		}
	}
	snapshot.Blockers = append(snapshot.Blockers, holds...)
	sort.SliceStable(snapshot.Blockers, func(i, j int) bool { return snapshot.Blockers[i].ID < snapshot.Blockers[j].ID })
	sort.Strings(activity)
	hash := sha256.New()
	fmt.Fprintf(hash, "touch=%d\n", r.activity.Load())
	for _, fact := range activity {
		hash.Write([]byte(fact))
		hash.Write([]byte{'\n'})
	}
	snapshot.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	return snapshot
}

// readFeeder turns a panic into an error: an unreadable source is busy.
func readFeeder(ctx context.Context, f feeder) (reading Reading, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("feeder %s panicked: %v", f.name, recovered)
		}
	}()
	if ctx.Err() != nil {
		return Reading{}, ctx.Err()
	}
	reading, err = f.read(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	for _, b := range reading.Blockers {
		if b.ID == "" || strings.TrimSpace(b.Label) == "" {
			return Reading{}, fmt.Errorf("feeder %s returned an unnamed blocker", f.name)
		}
	}
	return reading, err
}

// Hold tracks how long the registry has been idle with an unchanged
// fingerprint. It is updated from the workbench's existing hourly ticker and
// needs no polling: anything that ran between two observations changed the
// fingerprint.
type HoldTracker struct {
	Since       time.Time
	Fingerprint string
}

// Observe updates the tracker with a snapshot.
func (h *HoldTracker) Observe(s Snapshot) {
	switch {
	case !s.Idle():
		*h = HoldTracker{}
	case h.Fingerprint != s.Fingerprint || h.Since.IsZero():
		*h = HoldTracker{Since: s.At, Fingerprint: s.Fingerprint}
	}
}

// HeldFor is how long idle has held at now, provided the fingerprint is
// still fingerprint; otherwise zero.
func (h HoldTracker) HeldFor(now time.Time, fingerprint string) time.Duration {
	if h.Since.IsZero() || h.Fingerprint != fingerprint || now.Before(h.Since) {
		return 0
	}
	return now.Sub(h.Since)
}

// ConfirmationIDs lists the interruptible blockers a person must confirm.
func ConfirmationIDs(blockers []Blocker) []string {
	var ids []string
	for _, b := range blockers {
		if b.Interruptible {
			ids = append(ids, b.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
