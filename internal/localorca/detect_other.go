//go:build !darwin && !windows && !linux

package localorca

func detectLocal() Installation {
	return Installation{}
}
