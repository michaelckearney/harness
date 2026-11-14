// Package dispatch runs a verb on a framework, by the contract: the
// framework's command, in its folder, with the verb, its arguments, and the
// contract's environment.
package dispatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/michaelckearney/harness/contract"
	"github.com/michaelckearney/harness/internal/image"
	"github.com/michaelckearney/harness/internal/repo"
)

// Call is one verb on one target.
type Call struct {
	Root   string // the monorepo's root
	Target repo.Target
	Verb   string
	Args   []string
	Cwd    string
	// Version is passed for build.
	Version string
	Turn    bool
	// Env is added to the command's environment.
	Env            []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Error is a problem with the call itself, not the verb failing.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ErrFailed is returned when the framework ran the verb and it failed.
type ErrFailed struct {
	Target repo.Target
	Verb   string
	Code   int
}

func (e *ErrFailed) Error() string {
	return fmt.Sprintf("%s failed on %s (exit code %d)", e.Verb, e.Target, e.Code)
}

// Run runs the call and returns what the framework said.
func Run(ctx context.Context, c Call) (contract.Result, error) {
	fwDir := filepath.Join(c.Root, c.Target.FrameworkPath)
	m, err := contract.LoadManifest(fwDir)
	if err != nil {
		return contract.Result{}, &Error{err.Error()}
	}
	if !m.Supports(c.Verb) {
		return contract.Result{}, &Error{fmt.Sprintf("the framework has no verb %s; harness help lists its verbs", c.Verb)}
	}
	if c.Target.Project == "" && c.Verb != contract.Test {
		return contract.Result{}, &Error{fmt.Sprintf("%s needs a project; the framework's own code only has test", c.Verb)}
	}
	// The result is in a folder of its own, which a container can share.
	tmp, err := os.MkdirTemp("", "harness-")
	if err != nil {
		return contract.Result{}, err
	}
	defer os.RemoveAll(tmp)
	result := filepath.Join(tmp, "result.json")
	if err := os.WriteFile(result, nil, 0o666); err != nil {
		return contract.Result{}, err
	}

	project := ""
	if c.Target.Project != "" {
		project = filepath.Join(c.Root, c.Target.ProjectPath)
	}
	argv := append(append([]string{}, m.Command...), c.Verb)
	argv = append(argv, c.Args...)
	turn := "0"
	if c.Turn {
		turn = "1"
	}
	env := append([]string{
		contract.EnvRoot + "=" + c.Root,
		contract.EnvFramework + "=" + fwDir,
		contract.EnvProject + "=" + project,
		contract.EnvProjectName + "=" + c.Target.Project,
		contract.EnvCwd + "=" + c.Cwd,
		contract.EnvResult + "=" + result,
		contract.EnvVersion + "=" + c.Version,
		contract.EnvTurn + "=" + turn,
	}, c.Env...)
	var cmd *exec.Cmd
	if img := os.Getenv(image.EnvImage); img != "" {
		// In the pipelines, the framework runs in its image, with the
		// contract's environment and the pipeline's own, and nothing of
		// the runner's.
		for _, e := range os.Environ() {
			if strings.HasPrefix(e, "CI=") || strings.HasPrefix(e, "GITHUB_") || strings.HasPrefix(e, "RUNNER_") {
				env = append(env, e)
			}
		}
		cmd, err = image.Command(ctx, img, os.Getenv(image.EnvCache), fwDir, []string{c.Root, tmp}, env, argv, c.Stdin != nil)
		if err != nil {
			return contract.Result{}, &Error{err.Error()}
		}
	} else {
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = fwDir
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	// The framework's whole process group stops with the harness: a test
	// run starts browsers and servers of its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	runErr := cmd.Run()
	r, err := contract.Read(result)
	if err != nil {
		return r, &Error{fmt.Sprintf("the framework: %v", err)}
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return r, &ErrFailed{Target: c.Target, Verb: c.Verb, Code: ee.ExitCode()}
		}
		return r, &Error{fmt.Sprintf("cannot run the framework (%v): %v", m.Command, runErr)}
	}
	return r, nil
}
