package agentcli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
	"github.com/sylvinus/agent-vm/internal/workbench"
)

func selectUpgrade(data json.RawMessage, id string, in io.Reader, menu io.Writer) (json.RawMessage, error) {
	var catalog struct {
		Ready      bool   `json:"ready"`
		Running    bool   `json:"vm_running"`
		ProbeError string `json:"probe_error"`
		Agents     []struct {
			Spec    agentcatalog.AgentSpec `json:"spec"`
			Runtime struct {
				Installed bool   `json:"installed"`
				Version   string `json:"version"`
			} `json:"runtime"`
			Config struct {
				Preferred string `json:"preferred_channel"`
			} `json:"config"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	if !catalog.Ready || !catalog.Running || catalog.ProbeError != "" {
		return nil, &workbench.AgentError{Code: "probe_unavailable", Message: "无法确认已安装的 Agent，请检查客机运行状态。" + catalog.ProbeError}
	}
	type choice struct {
		ID      string `json:"agent_id"`
		Channel string `json:"channel"`
	}
	var choices []choice
	for _, item := range catalog.Agents {
		if !item.Runtime.Installed {
			continue
		}
		channel, ok := item.Spec.FindChannel(item.Config.Preferred)
		if !ok || channel.Kind == "existing" {
			for _, candidate := range item.Spec.Channels {
				if candidate.Kind != "existing" {
					channel, ok = candidate, true
					break
				}
			}
		}
		if !ok || channel.Kind == "existing" {
			continue
		}
		choices = append(choices, choice{item.Spec.ID, channel.ID})
		if id == "" {
			fmt.Fprintf(menu, "%d. %s (%s) %s · %s\n", len(choices), item.Spec.Name, item.Spec.ID, item.Runtime.Version, channel.Label)
		}
	}
	if len(choices) == 0 {
		return nil, &workbench.AgentError{Code: "no_installed_agents", Message: "没有检测到可升级的 Agent CLI"}
	}
	if id == "" {
		fmt.Fprint(menu, "选择要升级的 Agent（编号或 ID，回车取消）：")
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		id = strings.TrimSpace(line)
		if id == "" {
			return nil, nil
		}
		if n, err := strconv.Atoi(id); err == nil && n > 0 && n <= len(choices) {
			id = choices[n-1].ID
		}
	}
	for _, c := range choices {
		if c.ID == id {
			return json.Marshal(c)
		}
	}
	return nil, &workbench.AgentError{Code: "invalid_input", Message: "选择的 Agent 未安装或没有可用的升级渠道"}
}
