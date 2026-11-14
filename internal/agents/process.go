package agents

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/michaelckearney/harness/internal/agents/opencode"
)

// runLines runs an agent's command for one prompt and hands each line of
// its standard output to fn: Claude Code and Codex both stream JSON, one
// event a line. Standard error goes to the launcher's log.
func runLines(ctx context.Context, l Launcher, dir string, argv, extra []string, fn func(line []byte)) error {
	argv, env := l.Command(argv, extra)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	if l.Log != nil {
		cmd.Stderr = l.Log
	} else {
		cmd.Stderr = io.Discard
	}
	cmd.SysProcAttr = opencode.SysProcAttr()
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start %s: %w", argv[0], err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
