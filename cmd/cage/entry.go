package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/workbench"
)

// cage entry … and cage ssh … run before the profile is opened: the
// ProxyCommand runs once per SSH connection and must not take the storage
// lease or start anything.
func runEntry(args []string) int {
	path := os.Getenv(cage.ConfigEnv)
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude-cage", "config.json")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Super Cage：无法确定配置位置")
		return 255
	}
	if args[0] == "entry" {
		if len(args) > 1 && args[1] == "proxy" {
			// The slim cage-attach beside cage is the proxy, as for cage
			// attach: a Host block written as cage entry proxy then keeps
			// only that small program for the connection.
			if exe, err := os.Executable(); err == nil {
				if slim := attach.Beside(exe); slim != "" {
					if code, ok := execAttach(slim, args); ok {
						return code
					}
				}
			}
		}
		return workbench.EntryMain(args[1:], path, os.Stdin, os.Stdout, os.Stderr)
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "用法：cage ssh <环境> [ssh 参数…]")
		return 2
	}
	argv, err := workbench.SSHCommand(path, args[1], args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Super Cage："+err.Error())
		return 255
	}
	return execSSH(argv)
}
