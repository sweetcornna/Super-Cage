package workbench

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
)

// phoneActiveWindow is how recently a paired phone must have made a request
// to count as connected. A phone in the foreground polls far more often.
const phoneActiveWindow = 2 * time.Minute

// liveCount is the number of terminals whose process has not ended.
func (m *terminalManager) liveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, session := range m.sessions {
		if session.live() {
			count++
		}
	}
	return count
}

func (t *terminalSession) live() bool {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if !closed {
		return true
	}
	if t.done != nil {
		select {
		case <-t.done:
		default:
			return true
		}
	}
	return false
}

// updateActivity names every live terminal with the bytes it has written.
// A terminal that printed anything since the last look changes the
// fingerprint: an agent turn or a command with output is never idle.
func (m *terminalManager) updateActivity() (facts []string, tabs []policy.Terminal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	facts = append(facts, fmt.Sprintf("sequence=%d", m.sequence))
	for id, session := range m.sessions {
		if !session.live() {
			continue
		}
		session.mu.Lock()
		facts = append(facts, fmt.Sprintf("%s:%d", id, session.written))
		tabs = append(tabs, policy.Terminal{Title: session.title, Kind: session.kind, Workspace: session.workspace, Conversation: session.sharedID})
		session.mu.Unlock()
	}
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].Title < tabs[j].Title })
	return facts, tabs
}

func (d *displayViewDemand) viewerCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.viewers
}

// registerAppUpdateFeeders feeds the registry from state the workbench
// already holds. None of these reads touches the guest or the network.
func (s *Server) registerAppUpdateFeeders(r *policy.Registry) {
	r.Register("jobs", "无法确认后台操作的状态", func(context.Context) (policy.Reading, error) {
		job := s.jobs.snapshot()
		reading := policy.Reading{Activity: []string{job.ID, fmt.Sprint(job.Running)}}
		if job.Running {
			reading.Blockers = []policy.Blocker{{ID: "job:" + job.Kind, Kind: policy.KindJob, Label: "「" + jobName(job.Kind) + "」正在进行"}}
		}
		return reading, nil
	})
	r.Register("storage", "无法确认环境存储的状态", func(context.Context) (policy.Reading, error) {
		s.mu.Lock()
		pending := s.storageRecoveryError != ""
		s.mu.Unlock()
		if pending {
			return policy.Reading{Blockers: []policy.Blocker{{ID: "storage", Kind: policy.KindStorage, Label: "环境存储等待恢复"}}}, nil
		}
		return policy.Reading{}, nil
	})
	r.Register("terminals", "无法确认终端的状态", func(context.Context) (policy.Reading, error) {
		facts, _ := s.terminals.updateActivity()
		return policy.Reading{Activity: facts}, nil
	})
	r.Register("phone-turns", "无法确认手机会话的状态", func(context.Context) (policy.Reading, error) {
		s.mobileCLI.mu.Lock()
		defer s.mobileCLI.mu.Unlock()
		var reading policy.Reading
		for id, run := range s.mobileCLI.active {
			if run.cancel == nil {
				continue
			}
			reading.Activity = append(reading.Activity, id)
			reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "turn:phone:" + id, Kind: policy.KindTurn, Interruptible: true, Label: "手机发起的 Claude 轮次正在运行"})
		}
		return reading, nil
	})
	r.Register("viewers", "无法确认画面是否有人在看", func(context.Context) (policy.Reading, error) {
		var reading policy.Reading
		for _, view := range []struct {
			id, label string
			count     int
		}{{"viewer:display", "正在查看环境画面", s.desktopViews.viewerCount()}, {"viewer:browser", "正在查看浏览器画面", s.browserViews.viewerCount()}} {
			reading.Activity = append(reading.Activity, fmt.Sprintf("%s=%d", view.id, view.count))
			if view.count > 0 {
				reading.Blockers = append(reading.Blockers, policy.Blocker{ID: view.id, Kind: policy.KindViewer, Interruptible: true, Label: view.label})
			}
		}
		return reading, nil
	})
	r.Register("transfers", "无法确认下载和上传的状态", func(context.Context) (policy.Reading, error) {
		var reading policy.Reading
		s.mu.Lock()
		audio := s.audioClients
		for id, download := range s.nativeDownloads {
			select {
			case <-download.done:
				continue
			default:
			}
			reading.Activity = append(reading.Activity, "download:"+id)
			reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "download:" + id, Kind: policy.KindTransfer, Interruptible: true, Label: "浏览器下载「" + download.item.Name + "」正在保存到电脑"})
		}
		s.mu.Unlock()
		if audio > 0 {
			reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "audio", Kind: policy.KindAudio, Interruptible: true, Label: "正在播放环境声音"})
		}
		s.mobileUploads.mu.Lock()
		for id, upload := range s.mobileUploads.active {
			reading.Activity = append(reading.Activity, "upload:"+id)
			reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "upload:" + id, Kind: policy.KindTransfer, Interruptible: true, Label: "手机正在上传「" + upload.name + "」"})
		}
		s.mobileUploads.mu.Unlock()
		return reading, nil
	})
	r.Register("phone", "无法确认手机是否连接", func(context.Context) (policy.Reading, error) {
		last := s.appUpdate.lastPhone.Load()
		if last != 0 && time.Since(time.Unix(0, last)) < phoneActiveWindow {
			return policy.Reading{Blockers: []policy.Blocker{{ID: "phone", Kind: policy.KindPhone, Interruptible: true, Label: "已配对的手机正在连接"}}}, nil
		}
		return policy.Reading{}, nil
	})
	r.RegisterDecision("environment", "无法确认环境进程的状态", func(context.Context) (policy.Reading, error) {
		// The same probe the existing handoff uses, read only for a decision
		// (a bound Windows guest answers through PowerShell). A failed probe
		// must wait: stopping a guest whose state is unknown is not safe.
		if _, err := controllerVMOwners(s.profile); err != nil {
			return policy.Reading{Blockers: []policy.Blocker{{ID: "environment", Kind: policy.KindEnvironment, Unknown: true, Label: "无法确认环境进程的状态"}}}, nil
		}
		return policy.Reading{}, nil
	})
	r.RegisterDecision("guest", "无法确认环境内终端和应用的状态", func(ctx context.Context) (policy.Reading, error) {
		ctx, cancel := context.WithTimeout(ctx, appUpdateGuestTimeout)
		defer cancel()
		return s.appUpdate.guestRead(s)(ctx)
	})
}
