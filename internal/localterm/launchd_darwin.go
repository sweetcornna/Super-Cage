package localterm

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// sessionFromLaunchd asks launchd for the owner's login session variables:
// their names from launchctl print, each value from launchctl getenv.
func sessionFromLaunchd() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	printed, err := exec.CommandContext(ctx, "/bin/launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())).Output()
	if err != nil {
		return nil
	}
	var env []string
	for _, name := range launchdNames(string(printed)) {
		value, err := exec.CommandContext(ctx, "/bin/launchctl", "getenv", name).Output()
		if err == nil {
			env = append(env, name+"="+strings.TrimSuffix(string(value), "\n"))
		}
	}
	return env
}
