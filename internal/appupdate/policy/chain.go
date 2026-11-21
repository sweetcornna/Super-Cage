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

// Resumption is what a freshly started workbench must do with a record.
type Resumption struct {
	// Continue is the step to run next; empty when there is nothing to do.
	Continue Step
	// RolledBack is true when the old version is running again after an
	// installation that did not complete: the attempt failed, and the
	// environment comes back if it was running.
	RolledBack bool
	// Attention is true when the record cannot be acted on safely.
	Attention bool
}

// Resume decides, from the running version alone, how a chain continues
// after a process start. It never guesses: unknown records need attention.
func Resume(c *Chain, running string) Resumption {
	if c == nil {
		return Resumption{}
	}
	if !c.Known() {
		return Resumption{Attention: true}
	}
	switch c.Step {
	case StepDone, StepFailed, StepAttention:
		return Resumption{}
	case StepStopping, StepInstalling:
		switch running {
		case c.To:
			return Resumption{Continue: StepRelaunched}
		case c.From:
			return Resumption{RolledBack: true}
		}
		return Resumption{Attention: true}
	}
	if running != c.To {
		return Resumption{Attention: true}
	}
	return Resumption{Continue: c.Step}
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
