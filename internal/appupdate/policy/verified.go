package policy

import "strings"

// verifiedPlatforms lists the platforms (GOOS values, comma separated) whose
// unattended chain has been run end to end on a real machine. Silent mode is
// unavailable everywhere else, whatever the installation allows.
//
// It is empty: no platform has had that run yet. A build for the real-machine
// run lifts the gate for one platform without a code change:
//
//	go build -ldflags "-X github.com/sylvinus/agent-vm/internal/appupdate/policy.verifiedPlatforms=darwin" ./cmd/cage
//
// After a successful run the platform is written here, in a reviewed commit.
var verifiedPlatforms = ""

// NotVerifiedReason is shown in 设置 → 关于 while the gate is closed.
const NotVerifiedReason = "静默更新还没有在此平台的真机上验证，暂不可用；可以点击升级"

// SilentVerified reports whether silent mode may be offered on goos.
func SilentVerified(goos string) bool {
	if goos == "" {
		return false
	}
	for _, platform := range strings.Split(verifiedPlatforms, ",") {
		if strings.TrimSpace(platform) == goos {
			return true
		}
	}
	return false
}
