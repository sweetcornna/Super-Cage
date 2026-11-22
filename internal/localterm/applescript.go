package localterm

import (
	"errors"
	"strings"
)

// appleScriptError says why an AppleScript step failed in the owner's
// words. osascript reports AppleEvent errors by number: -1743 when macOS
// does not allow Super Cage to control the application, -1712 when the
// application did not answer in time, -2741 and -2753 when the application
// does not know the terms (a Ghostty older than 1.3).
func appleScriptError(output []byte, timedOut bool, err error) error {
	text := string(output)
	switch {
	case strings.Contains(text, "(-1743)"):
		return errors.New("macOS 没有允许 Super Cage 控制这个应用：在“系统设置 › 隐私与安全性 › 自动化”里打开后再试")
	case timedOut:
		return errors.New("等待 macOS 的权限确认超时：在弹出的窗口里选择后再试")
	case strings.Contains(text, "(-1712)"):
		return errors.New("应用没有及时回应：如果 macOS 在询问是否允许 Super Cage 控制它，先选择后再试")
	case strings.Contains(text, "(-2741)"), strings.Contains(text, "(-2753)"), strings.Contains(text, "(-2740)"):
		return errors.New("这个版本的应用不支持从 Super Cage 打开窗口：请更新应用（Ghostty 需要 1.3 或更新版本）")
	}
	return errors.New(firstLine(output, err))
}
