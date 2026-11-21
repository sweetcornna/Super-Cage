package policy

import (
	"fmt"
	"strings"
	"time"
)

// Code is the outcome of a decision.
type Code string

const (
	Allowed           Code = "allowed"
	ManualMode        Code = "manual_mode"
	SilentUnavailable Code = "silent_unavailable"
	NotEligible       Code = "not_eligible"
	FailedBefore      Code = "failed_before"
	TooNew            Code = "too_new"
	MustWait          Code = "must_wait"
	ConfirmRequired   Code = "confirm_required"
	Busy              Code = "busy"
	NotHeld           Code = "not_held"
	Changed           Code = "changed"
	UserActive        Code = "user_active"
	UserUnknown       Code = "user_unknown"
	Unreadable        Code = "unreadable"
	// StopFailed is the workbench's answer when the decision allowed the
	// installation but the environment did not stop; nothing was installed.
	StopFailed Code = "stop_failed"
	// The host's power and display state (unattended only).
	HostUnknown Code = "host_unknown"
	DarkWake    Code = "dark_wake"
	LidClosed   Code = "lid_closed"
	LowBattery  Code = "low_battery"
	DisplayHeld Code = "display_held"
	// RetryLater: an unattended request ended before the decision; the next
	// one waits as RetrySchedule says.
	RetryLater Code = "retry_later"
)

// Request is what an installer asks for.
type Request struct {
	Trigger Trigger
	Version string
	// Confirmed lists the blocker ids the person saw and accepted.
	Confirmed []string
	// Fingerprint is the activity fingerprint recorded with a silent request.
	Fingerprint string
}

// Facts are everything the decision reads, gathered by the workbench under
// its operation lock with admission sealed.
type Facts struct {
	Current     string
	Settings    Settings
	SettingsErr error
	Ledger      Ledger
	LedgerErr   error
	Snapshot    Snapshot
	Hold        HoldTracker
	// InputIdle is the host's time since the last keyboard or mouse input.
	InputIdle      time.Duration
	InputIdleKnown bool
	// Host is the host's power and display state; nil when unreadable.
	Host *HostState
	PublishedAt    string
	Now            time.Time
	// SilentUnavailable is non-empty when this installation cannot be
	// updated unattended (no publisher to pin, system package, elevation).
	SilentUnavailable string
}

// Decision is the answer to "may I install now?".
type Decision struct {
	Code     Code      `json:"code"`
	Message  string    `json:"message"`
	Blockers []Blocker `json:"blockers,omitempty"`
}

// Allowed reports whether the installation may proceed.
func (d Decision) Allowed() bool { return d.Code == Allowed }

// Decide is the whole rule set. It is pure: the same facts give the same
// answer, which the tests pin.
func Decide(r Request, f Facts) Decision {
	if err := Eligible(r.Version, f.Current); err != nil {
		return Decision{Code: NotEligible, Message: err.Error()}
	}
	var mustWait, interruptible []Blocker
	for _, b := range f.Snapshot.Blockers {
		if b.Interruptible {
			interruptible = append(interruptible, b)
		} else {
			mustWait = append(mustWait, b)
		}
	}
	if r.Trigger == Click {
		if len(mustWait) != 0 {
			return Decision{Code: MustWait, Message: "以下操作完成后才能升级：" + mustWait[0].Label, Blockers: mustWait}
		}
		confirmed := map[string]bool{}
		for _, id := range r.Confirmed {
			confirmed[id] = true
		}
		var unconfirmed []Blocker
		for _, b := range interruptible {
			if !confirmed[b.ID] {
				unconfirmed = append(unconfirmed, b)
			}
		}
		if len(unconfirmed) != 0 {
			return Decision{Code: ConfirmRequired, Message: "有新的任务正在进行，请确认后再升级", Blockers: interruptible}
		}
		return Decision{Code: Allowed, Message: "可以升级"}
	}
	if r.Trigger != SilentUpdate {
		return Decision{Code: NotEligible, Message: "升级请求无效"}
	}
	switch {
	case f.SettingsErr != nil || f.Settings.Mode != Silent:
		return Decision{Code: ManualMode, Message: "未开启空闲时自动静默更新，需要点击升级"}
	case f.SilentUnavailable != "":
		return Decision{Code: SilentUnavailable, Message: f.SilentUnavailable}
	case f.LedgerErr != nil:
		return Decision{Code: Unreadable, Message: "无法读取升级记录，不会自动升级；可以点击升级"}
	case f.Ledger.LostHistory:
		return Decision{Code: Unreadable, Message: "升级记录曾无法读取，已另存；点击升级成功一次后才会自动升级"}
	case f.Ledger.FailedBefore(r.Version):
		return Decision{Code: FailedBefore, Message: r.Version + " 上次升级没有完成，不会自动重试；可以点击升级重试"}
	case !OldEnough(f.PublishedAt, f.Now):
		return Decision{Code: TooNew, Message: "新版本发布未满 24 小时，稍后再自动升级"}
	case !f.Snapshot.Decision:
		return Decision{Code: Unreadable, Message: "空闲状态没有完整读取，暂不自动升级"}
	case len(f.Snapshot.Blockers) != 0:
		return Decision{Code: Busy, Message: "等待空闲：" + f.Snapshot.Blockers[0].Label, Blockers: f.Snapshot.Blockers}
	case r.Fingerprint != "" && r.Fingerprint != f.Snapshot.Fingerprint:
		return Decision{Code: Changed, Message: "空闲期间有新的活动，等待下一次空闲"}
	case !f.Hold.Held(f.Now, f.Snapshot.Fingerprint):
		return Decision{Code: NotHeld, Message: "等待电脑持续空闲"}
	case !f.InputIdleKnown:
		return Decision{Code: UserUnknown, Message: "无法确认电脑是否无人使用，暂不自动升级"}
	case f.InputIdle < UserIdle:
		return Decision{Code: UserActive, Message: "最近有人在使用电脑，等待空闲"}
	case f.Host == nil:
		return Decision{Code: HostUnknown, Message: "无法确认电脑的电源和显示状态，暂不自动升级"}
	case f.Host.DarkWake:
		return Decision{Code: DarkWake, Message: "电脑处于后台唤醒状态，等正常唤醒时再自动升级"}
	case f.Host.LidClosed:
		return Decision{Code: LidClosed, Message: "笔记本已合盖且没有外接显示器，暂不自动升级"}
	case f.Host.OnBattery && f.Host.Battery < MinBattery:
		return Decision{Code: LowBattery, Message: fmt.Sprintf("正在使用电池且电量低于 %d%%，接上电源后再自动升级", MinBattery)}
	case len(f.Host.DisplayHeld) != 0:
		return Decision{Code: DisplayHeld, Message: "有应用正在保持屏幕常亮（" + strings.Join(f.Host.DisplayHeld, "、") + "），等待空闲"}
	}
	return Decision{Code: Allowed, Message: "可以升级"}
}
