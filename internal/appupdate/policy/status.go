package policy

import (
	"github.com/sylvinus/agent-vm/internal/appupdate/catalog"
)

// Pending is a newer release the workbench knows about.
type Pending struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
	// Installable is false when the release can only be installed by hand.
	Installable bool   `json:"installable"`
	Reason      string `json:"reason,omitempty"`
	// Requested is true once a silent request was written for the shell.
	Requested bool `json:"requested,omitempty"`
}

// ChainView is the chain as the UI shows it.
type ChainView struct {
	Step       Step       `json:"step"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Trigger    Trigger    `json:"trigger"`
	FailedStep Step       `json:"failed_step,omitempty"`
	Error      string     `json:"error,omitempty"`
	Next       string     `json:"next,omitempty"`
	NotesURL   string     `json:"notes_url,omitempty"`
	Terminals  []Terminal `json:"terminals,omitempty"`
}

// Status is what GET /api/app-update answers.
type Status struct {
	Mode                    Mode       `json:"mode"`
	ModeError               string     `json:"mode_error,omitempty"`
	SilentAvailable         bool       `json:"silent_available"`
	SilentUnavailableReason string     `json:"silent_unavailable_reason,omitempty"`
	Pending                 *Pending   `json:"pending"`
	Waiting                 *Decision  `json:"waiting"`
	LastAttempt             *Attempt   `json:"last_attempt"`
	Chain                   *ChainView `json:"chain"`
}

// ReleaseNotes is the public page of a version's release notes.
func ReleaseNotes(version string) string {
	return "https://github.com/" + catalog.Repository + "/releases/tag/v" + version
}

// View turns a chain record into what the UI shows.
func View(c *Chain) *ChainView {
	if c == nil {
		return nil
	}
	if !c.Known() {
		return &ChainView{Step: StepAttention, Error: "升级进度记录来自其他版本，无法继续；可以重新点击升级"}
	}
	v := &ChainView{Step: c.Step, From: c.From, To: c.To, Trigger: c.Trigger, FailedStep: c.FailedStep, Error: c.Error, NotesURL: ReleaseNotes(c.To)}
	if c.Step == StepFailed {
		v.Next = NextAction(c.FailedStep)
	}
	if c.Step == StepDone {
		v.Terminals = c.Environment.Terminals
	}
	return v
}
