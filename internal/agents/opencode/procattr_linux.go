package opencode

import "syscall"

// SysProcAttr puts an agent in its own process group, so stopping it stops
// everything it started, and has it signalled when the harness dies. An
// agent running as the bots' user is out of reach of the service's own kill
// (and systemd's, from a user manager), so without the signal a crashed
// service would leave it running. Under sudo the signal reaches sudo, which
// relays it.
func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
