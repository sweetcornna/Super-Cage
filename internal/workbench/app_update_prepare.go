package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
	"github.com/sylvinus/agent-vm/internal/version"
)

// AppUpdateOptions is what the native shell asks for before it installs: a
// click with the interruptions the person confirmed, or an unattended
// request the workload's hourly tick wrote.
type AppUpdateOptions struct {
	Version   string
	Trigger   string
	Confirmed []string
	Request   string
	// Stopped is told the attempt as soon as the workload has agreed and
	// stopped the environment, before the rest of the handoff: whatever goes
	// wrong after this, the shell knows which record to close.
	Stopped func(attempt string)
}

// AppUpdatePrepared is the answer printed by "cage prepare-update --app-update".
// Status is "prepared" (stop done, install now), "blocked", or "abandoned"
// (the workload stopped the environment for Attempt, but the handoff did
// not complete: nothing will be installed and the record must be closed).
type AppUpdatePrepared struct {
	Status string `json:"status"`
	// Attempt identifies the update record; the installer writes it back
	// into last-result.json so a rollback can be matched to it.
	Attempt  string           `json:"attempt,omitempty"`
	Code     policy.Code      `json:"code,omitempty"`
	Message  string           `json:"message,omitempty"`
	Blockers []policy.Blocker `json:"blockers,omitempty"`
}

// appUpdateHandoffTimeout covers the decision, the guest read and stopping
// the environment (two minutes), inside the shell's 180 seconds.
const appUpdateHandoffTimeout = 150 * time.Second

func appUpdateBlocked(code policy.Code, message string) AppUpdatePrepared {
	return AppUpdatePrepared{Status: "blocked", Code: code, Message: message}
}

func fromDesktopResult(result DesktopResult) AppUpdatePrepared {
	if result.Status == "prepared" {
		return AppUpdatePrepared{Status: "prepared"}
	}
	message := result.Message
	if message == "" {
		message = "环境未能安全停止，更新未安装"
	}
	return appUpdateBlocked(policy.Busy, message)
}

// PrepareAppUpdate asks the local workload "may I install now?". The workload
// answers under its operation lock and, when the answer is yes, stops the
// environment in the same operation and exits. The desktop gateway is then
// released as before.
func PrepareAppUpdate(fullConfig string, options AppUpdateOptions) (AppUpdatePrepared, error) {
	trigger := policy.Trigger(options.Trigger)
	if trigger != policy.Click && trigger != policy.SilentUpdate {
		return AppUpdatePrepared{}, errors.New("升级触发方式无效")
	}
	if err := policy.Eligible(options.Version, version.Version); err != nil {
		return appUpdateBlocked(policy.NotEligible, err.Error()), nil
	}
	profile := filepath.Dir(fullConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	c, err := waitControllerReady(ctx, profile)
	cancel()
	if errors.Is(err, errControllerUnavailable) {
		if trigger == policy.SilentUpdate {
			return appUpdateBlocked(policy.Unreadable, "本机后台没有运行，无法确认电脑空闲，不会自动升级"), nil
		}
		// Nothing runs locally: the existing lock and VM checks decide.
		result, err := PrepareDesktopUpdate(fullConfig, true)
		return fromDesktopResult(result), err
	}
	if err != nil {
		return AppUpdatePrepared{}, err
	}
	defer c.close()
	reply, status, err := c.appUpdateHandoff(options)
	if err != nil {
		return AppUpdatePrepared{}, err
	}
	switch status {
	case http.StatusOK:
		if reply.Status != "prepared" {
			return AppUpdatePrepared{}, errors.New("后台响应无效，未继续更新。")
		}
		return finishAppUpdateHandoff(reply, options.Stopped, c.waitForExit, func() error {
			gateway, err := connectController(filepath.Dir(desktopConfigPath(fullConfig)))
			if errors.Is(err, errControllerUnavailable) {
				return nil
			}
			if err != nil {
				return err
			}
			defer gateway.close()
			return gateway.releaseDesktopEntry()
		})
	case http.StatusConflict:
		if reply.Status != "blocked" {
			reply = appUpdateBlocked(policy.Busy, "本机后台拒绝了这次升级")
		}
		return reply, nil
	case http.StatusNotFound:
		if trigger == policy.SilentUpdate {
			return appUpdateBlocked(policy.Unreadable, "本机后台版本不支持自动升级"), nil
		}
		result, err := PrepareDesktopUpdate(fullConfig, true)
		return fromDesktopResult(result), err
	}
	return AppUpdatePrepared{}, fmt.Errorf("本机后台未接受升级请求（HTTP %d）", status)
}

// finishAppUpdateHandoff runs what follows the workload's yes. The shell is
// told the attempt first, before anything else can fail, so that a failure
// or a kill afterwards is an abandonment it records; then the workload's
// exit and the desktop entry's release. A failure after the stop is
// "abandoned" with the attempt: the workbench restores the environment.
func finishAppUpdateHandoff(reply AppUpdatePrepared, stopped func(attempt string), waitExit, release func() error) (AppUpdatePrepared, error) {
	if stopped != nil {
		stopped(reply.Attempt)
	}
	for _, step := range []func() error{waitExit, release} {
		if err := step(); err != nil {
			return AppUpdatePrepared{Status: "abandoned", Attempt: reply.Attempt, Message: "环境已停止，但升级没有继续：" + err.Error()}, err
		}
	}
	return reply, nil
}

func (c *controllerClient) appUpdateHandoff(options AppUpdateOptions) (AppUpdatePrepared, int, error) {
	body, err := json.Marshal(appUpdateHandoffRequest{Token: c.record.Token, Version: options.Version, Trigger: options.Trigger, Confirmed: options.Confirmed, Request: options.Request})
	if err != nil {
		return AppUpdatePrepared{}, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), appUpdateHandoffTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/api/desktop-local/app-update-handoff", bytes.NewReader(body))
	if err != nil {
		return AppUpdatePrepared{}, 0, err
	}
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Content-Type", "application/json")
	client := *c.client
	client.Timeout = appUpdateHandoffTimeout
	res, err := client.Do(req)
	if err != nil {
		return AppUpdatePrepared{}, 0, err
	}
	defer res.Body.Close()
	var reply AppUpdatePrepared
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusConflict {
		if json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&reply) != nil {
			if res.StatusCode == http.StatusOK {
				return AppUpdatePrepared{}, 0, errors.New("后台响应无效，未继续更新。")
			}
			reply = appUpdateBlocked(policy.Busy, "本机后台拒绝了这次升级")
		}
	}
	return reply, res.StatusCode, nil
}
