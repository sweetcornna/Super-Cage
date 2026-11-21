//go:build !darwin

package localterm

// sessionFromLaunchd: only macOS has launchd.
func sessionFromLaunchd() []string { return nil }
