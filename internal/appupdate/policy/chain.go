package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ChainSchema is the chain record's format. A record with another schema is
// never acted on; the button is shown instead.
const ChainSchema = 1

// Step is where an update chain stands.
type Step string

const (
	StepStopping   Step = "stopping"
	StepInstalling Step = "installing"
	StepRelaunched Step = "relaunched"
	StepRestoring  Step = "restoring"
	StepComponents Step = "components"
	StepTools      Step = "tools"
	StepNetwork    Step = "network"
	StepDone       Step = "done"
	StepFailed     Step = "failed"
	StepAttention  Step = "attention"
)

// Terminal is a terminal tab to reopen after the update. Conversation is the
// Claude or Codex conversation it was bound to, if any.
type Terminal struct {
	Title        string `json:"title"`
	Kind         string `json:"kind"`
	Workspace    string `json:"workspace,omitempty"`
	Conversation string `json:"conversation,omitempty"`
}

// Environment is what the update must bring back.
type Environment struct {
	Running bool `json:"running"`
	// NetworkPaused keeps a pause the person set: the restored environment
	// starts with its network still paused.
	NetworkPaused bool       `json:"network_paused,omitempty"`
	Workspace     string     `json:"workspace,omitempty"`
	Terminals     []Terminal `json:"terminals,omitempty"`
}

// Chain is written by the version being replaced at the moment it decides to
// install, read by the new version after relaunch, and read again by the old
// version after a rollback.
type Chain struct {
	Schema      int         `json:"schema"`
	ID          string      `json:"id"`
	From        string      `json:"from"`
	To          string      `json:"to"`
	Trigger     Trigger     `json:"trigger"`
	Step        Step        `json:"step"`
	FailedStep  Step        `json:"failed_step,omitempty"`
	Error       string      `json:"error,omitempty"`
	Environment Environment `json:"environment"`
	StartedAt   time.Time   `json:"started_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

func chainPath(profile string) string { return filepath.Join(Dir(profile), "chain.json") }

// LoadChain returns nil without error when there is no record: the first
// update from a version without chains, or a manual replacement.
func LoadChain(profile string) (*Chain, error) {
	data, err := readSmall(chainPath(profile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Chain
	if json.Unmarshal(data, &c) != nil {
		return nil, errors.New("升级进度记录无效")
	}
	return &c, nil
}

// SaveChain stores the record atomically.
func SaveChain(profile string, c *Chain) error { return writePrivate(chainPath(profile), c) }

var transitions = map[Step][]Step{
	StepStopping:   {StepInstalling, StepRelaunched, StepFailed},
	StepInstalling: {StepRelaunched, StepFailed, StepAttention},
	StepRelaunched: {StepRestoring, StepDone, StepAttention},
	StepRestoring:  {StepComponents, StepTools, StepFailed, StepAttention},
	StepComponents: {StepTools, StepFailed, StepAttention},
	StepTools:      {StepNetwork, StepFailed, StepAttention},
	StepNetwork:    {StepDone, StepFailed, StepAttention},
}

// Known reports whether the record is one this version understands.
func (c *Chain) Known() bool {
	if c == nil || c.Schema != ChainSchema || c.From == "" || c.To == "" {
		return false
	}
	switch c.Step {
	case StepDone, StepFailed, StepAttention:
		return true
	}
	_, ok := transitions[c.Step]
	return ok
}

// Active reports whether the chain still has work to do.
func (c *Chain) Active() bool {
	return c != nil && c.Known() && c.Step != StepDone && c.Step != StepFailed && c.Step != StepAttention
}

// Advance moves the chain to next, refusing transitions the state machine
// does not have.
func (c *Chain) Advance(next Step, now time.Time) error {
	for _, allowed := range transitions[c.Step] {
		if allowed == next {
			c.Step, c.UpdatedAt = next, now
			return nil
		}
	}
	return errors.New("升级进度不能从 " + string(c.Step) + " 变为 " + string(next))
}

// Fail ends the chain at step with a reason the person can act on.
func (c *Chain) Fail(step Step, reason string, now time.Time) {
	c.FailedStep, c.Error, c.Step, c.UpdatedAt = step, reason, StepFailed, now
}

// The bounds within which a record is acted on.
const (
	// ChainFresh: a record last written longer ago is retired with a note
	// and never acted on.
	ChainFresh = 24 * time.Hour
	// InstallerWait: how long the new version waits, after it starts, for
	// the installer's result naming its attempt.
	InstallerWait = 3 * time.Minute
	// ResultFresh: an installer result written longer ago belongs to an
	// earlier start and proves nothing about this one.
	ResultFresh = 15 * time.Minute
	// InstallerGrace: how long the previous version leaves an installing
	// record alone when no result names it; the installer may still work.
	InstallerGrace = 30 * time.Minute
)

// InstallerResult is the installer helper's updates/last-result.json.
type InstallerResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Version string `json:"version"`
	Attempt string `json:"attempt"`
	// WrittenAt is the file's modification time.
	WrittenAt time.Time `json:"-"`
}

// names reports whether r is the installer's result for c's attempt.
func (r *InstallerResult) names(c *Chain) bool {
	return r != nil && r.Attempt != "" && r.Attempt == c.ID && r.Version == c.To
}

// Resumption is what a freshly started workbench must do with a record.
type Resumption struct {
	// Continue is the step to run next; empty when there is nothing to do.
	Continue Step
	// Wait: the new version runs, but the installer has not reported on
	// this attempt yet. Ask again when its result appears, until
	// InstallerWait has passed; then retire the record.
	Wait bool
	// RolledBack: the installer reported that this attempt failed and kept
	// the previous version, which runs again. The attempt failed; the
	// environment comes back if it was running.
	RolledBack bool
	// Undone: the update had completed and the previous version runs again.
	// It counts as a failure of the new version, which silent mode then
	// never installs by itself.
	Undone bool
	// Retire: the record cannot be acted on safely. It is closed with this
	// note and nothing is done to the environment.
	Retire string
	// Hold: the record is left as it is for now, with this note.
	Hold string
	// Attention is true when the record cannot be read as a known record.
	Attention bool
}

// Resume decides how a chain continues after a process start, from the
// running version, the installer's result and the record's age. It never
// guesses: a record is acted on only when it is recent and, across the
// replacement, when the installer's result names its attempt.
func Resume(c *Chain, running string, now time.Time, result *InstallerResult) Resumption {
	if c == nil {
		return Resumption{}
	}
	if !c.Known() {
		return Resumption{Attention: true}
	}
	switch c.Step {
	case StepFailed, StepAttention:
		return Resumption{}
	case StepDone:
		if running == c.From && running != c.To {
			return Resumption{Undone: true}
		}
		return Resumption{}
	}
	if c.UpdatedAt.After(now.Add(time.Minute)) || now.Sub(c.UpdatedAt) > ChainFresh {
		return Resumption{Retire: "升级记录已过期，没有据此改动环境"}
	}
	switch c.Step {
	case StepStopping:
		// Nothing was installed; the stop was interrupted part way.
		return Resumption{Retire: "升级在停止环境时中断，没有安装更新；环境保持现状，需要时请手动启动"}
	case StepInstalling:
		named := result.names(c)
		switch running {
		case c.To:
			switch {
			case !named:
				return Resumption{Wait: true}
			case !result.Success:
				// The installer is bringing the previous version back.
				return Resumption{Hold: "安装程序正在恢复原版本"}
			case now.Sub(result.WrittenAt) > ResultFresh:
				return Resumption{Retire: "安装程序的结果来自更早的启动，没有据此恢复环境"}
			}
			return Resumption{Continue: StepRelaunched}
		case c.From:
			switch {
			case named && !result.Success:
				return Resumption{RolledBack: true}
			case named:
				return Resumption{Undone: true}
			case now.Sub(c.UpdatedAt) < InstallerGrace:
				return Resumption{Hold: "升级没有完成，环境保持停止；可以重新点击升级，或手动启动环境"}
			}
			return Resumption{Retire: "升级没有完成，也没有安装程序的结果；环境保持停止，可以重新点击升级或手动启动环境"}
		}
		return Resumption{Attention: true}
	}
	// Relaunched or later: the installer had confirmed the new version.
	switch running {
	case c.To:
		return Resumption{Continue: c.Step}
	case c.From:
		return Resumption{Undone: true}
	}
	return Resumption{Attention: true}
}

// Retire closes a record that is not acted on, with a note for the person.
func (c *Chain) Retire(note string, now time.Time) {
	c.FailedStep, c.Error, c.Step, c.UpdatedAt = c.Step, note, StepAttention, now
}

// NextAction names the button a failed chain offers, for the UI.
func NextAction(step Step) string {
	switch step {
	case StepStopping, StepInstalling:
		return "retry_update"
	case StepRestoring:
		return "start_environment"
	case StepComponents:
		return "update_components"
	case StepTools:
		return "update_tools"
	case StepNetwork:
		return "check_network"
	}
	return ""
}
