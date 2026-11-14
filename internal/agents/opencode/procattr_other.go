//go:build !linux

package opencode

import "syscall"

// SysProcAttr puts an agent in its own process group, so stopping it stops
// everything it started.
func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
