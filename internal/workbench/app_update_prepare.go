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
}

// AppUpdatePrepared is the answer printed by "cage prepare-update --app-update".
// Status is "prepared" (stop done, install now) or "blocked".
type AppUpdatePrepared struct {
	Status   string           `json:"status"`
	Code     policy.Code      `json:"code,omitempty"`
	Message  string           `json:"message,omitempty"`
	Blockers []policy.Blocker `json:"blockers,omitempty"`
}

// appUpdateHandoffTimeout covers the decision, the guest read and stopping
// the environment, which the existing stop allows two minutes for.
const appUpdateHandoffTimeout = 4 * time.Minute

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
		if err = c.waitForExit(); err != nil {
			return AppUpdatePrepared{}, err
		}
		gateway, err := connectController(filepath.Dir(desktopConfigPath(fullConfig)))
		if errors.Is(err, errControllerUnavailable) {
			return AppUpdatePrepared{Status: "prepared"}, nil
		}
		if err != nil {
			return AppUpdatePrepared{}, err
		}
		defer gateway.close()
		if err = gateway.releaseDesktopEntry(); err != nil {
			return AppUpdatePrepared{}, err
		}
		return AppUpdatePrepared{Status: "prepared"}, nil
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
