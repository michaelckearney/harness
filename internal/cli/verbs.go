// Package cli is the harness's verbs, for a developer in a monorepo and for
// a bot in its turn: it finds what the verb is for, then runs it on each
// target by the contract.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/michaelckearney/harness/contract"
	"github.com/michaelckearney/harness/internal/command"
	"github.com/michaelckearney/harness/internal/dispatch"
	"github.com/michaelckearney/harness/internal/gitversion"
	"github.com/michaelckearney/harness/internal/repo"
)

// Error is a problem the user can fix.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// Verb runs `harness <verb> [-p <name>] [args...]`.
//
// The verb is test, build, or one the framework declares. The scope is the
// one -p names (a project, or framework), else, in a bot's turn, the
// issue's, else the folder the harness runs in: inside a project that
// project, anywhere else the framework and every project. test runs on
// every target of the scope, in turn, and stops at the first that fails; in
// a bot's turn, only on those the issue's changes affect, and with -since
// <ref>, as in a pull request's checks, only on those the changes since
// <ref> affect. The other verbs need one project.
func Verb(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := repo.FindRoot(cwd)
	if err != nil {
		return &Error{err.Error()}
	}
	cfg, err := repo.Load(root)
	if err != nil {
		return &Error{err.Error()}
	}
	m, err := contract.LoadManifest(filepath.Join(root, cfg.Framework))
	if err != nil {
		return &Error{err.Error()}
	}
	turn := command.InTurn()
	switch {
	case turn && (verb == contract.Build || m.Supports(verb) && !m.InTurn(verb)):
		return fail("harness %s is not for a bot's turn; harness help lists what is", verb)
	case !m.Supports(verb) && verb == contract.Build:
		return fail("the framework has no build, so its projects have nothing to release")
	case !m.Supports(verb):
		return &UnknownError{Verb: verb}
	}
	name, since, args := pick(args)
	if since != "" && (verb != contract.Test || turn) {
		return fail("-since is for harness test, outside a bot's turn")
	}

	var scope repo.Scope
	var changed []string
	switch {
	case name != "":
		if scope, err = named(cfg, name); err != nil {
			return err
		}
	case turn:
		raw, err := command.Scope()
		if err != nil {
			return &Error{err.Error()}
		}
		var ts TurnScope
		if err := json.Unmarshal([]byte(raw), &ts); err != nil {
			return err
		}
		scope, changed = ts.Scope, ts.Changed
	default:
		rel, err := filepath.Rel(root, cwd)
		if err != nil {
			return err
		}
		scope = cfg.ScopeOf(rel)
	}
	if since != "" {
		if changed, err = changedSince(root, since); err != nil {
			return err
		}
	}

	call := dispatch.Call{Root: root, Verb: verb, Args: args, Cwd: cwd, Turn: turn,
		Stdin: os.Stdin, Stdout: stdout, Stderr: stderr}

	if verb != contract.Test {
		targets := cfg.Targets(scope, false)
		if len(targets) != 1 {
			var names []string
			for _, t := range targets {
				names = append(names, t.Project)
			}
			return fail("harness %s runs on one project, and %s has %d; name one with -p <project>: %s", verb, scope, len(targets), strings.Join(names, ", "))
		}
		call.Target = targets[0]
		if verb == contract.Build {
			v, err := gitversion.Build(ctx, root, gitversion.For(call.Target.Project, call.Target.ProjectPath))
			if err != nil {
				return &Error{err.Error()}
			}
			call.Version = v
		}
		r, err := dispatch.Run(ctx, call)
		if err == nil && verb == contract.Build {
			for _, a := range r.Artifacts {
				fmt.Fprintf(stderr, "harness: built %s\n", filepath.Join(call.Target.ProjectPath, a))
			}
		}
		return wrap(err)
	}

	// A bot's full test of its issue's scope counts for done: the service
	// is told the tree before, and whether it passed after.
	var tree string
	if turn {
		if tree, err = command.TestBegin(scope.Label()); err != nil {
			fmt.Fprintf(stderr, "harness: this run will not count for done: %v\n", err)
		}
	}
	targets := cfg.Targets(scope, true)
	if turn && name == "" || since != "" {
		targets = cfg.Affected(scope, changed)
		if len(targets) < len(cfg.Targets(scope, true)) {
			var names []string
			for _, t := range targets {
				names = append(names, t.String())
			}
			fmt.Fprintf(stderr, "harness: the changes are in %s only, so that is what this tests\n", strings.Join(names, ", "))
		}
	}
	if len(targets) == 0 {
		return fail("%s has nothing to test", scope)
	}
	full := true
	var runErr error
	for _, t := range targets {
		if len(targets) > 1 {
			fmt.Fprintf(stderr, "harness: testing %s\n", t)
		}
		call.Target = t
		r, err := dispatch.Run(ctx, call)
		full = full && r.Full
		if err != nil {
			runErr = err
			break
		}
	}
	if tree != "" && ctx.Err() == nil {
		switch {
		case !full && runErr == nil:
			fmt.Fprintf(stderr, "harness: this run was not the whole test, so it does not count for done; run harness test with no arguments\n")
		case runErr != nil && !errors.As(runErr, new(*dispatch.ErrFailed)):
			// The harness could not run the tests: that tested nothing.
		default:
			if err := command.Tested(tree, runErr == nil); err != nil {
				fmt.Fprintf(stderr, "harness: this run will not count for done: %v\n", err)
			}
		}
	}
	if runErr == nil && len(targets) > 1 {
		fmt.Fprintf(stderr, "harness: %s passed\n", scope)
	}
	return wrap(runErr)
}

// pick takes -p <name> and -since <ref>, in either order, each also as
// -flag=value, from the front of the arguments.
func pick(args []string) (name, since string, rest []string) {
	for {
		flag, value, n := "", "", 0
		switch a := args; {
		case len(a) >= 2 && (a[0] == "-p" || a[0] == "--project" || a[0] == "-since" || a[0] == "--since"):
			flag, value, n = a[0], a[1], 2
		case len(a) >= 1 && strings.HasPrefix(a[0], "-") && strings.Contains(a[0], "="):
			flag, value, _ = strings.Cut(a[0], "=")
			n = 1
		}
		switch strings.TrimLeft(flag, "-") {
		case "p", "project":
			name = value
		case "since":
			since = value
		default:
			return name, since, args
		}
		args = args[n:]
	}
}

// changedSince are the files changed since the merge base of ref and HEAD,
// relative to the root: what a pull request onto ref changes.
func changedSince(root, ref string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", ref+"...HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fail("-since %s: git diff failed: %v", ref, err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// named is the scope a -p names: a project, or framework.
func named(cfg *repo.Config, name string) (repo.Scope, error) {
	if cfg.Projects[name] != nil {
		name = repo.ProjectKind + ":" + name
	}
	sc, err := cfg.ScopeNamed(name)
	if err != nil {
		return sc, fail("-p names a project or framework, and %q is neither; the projects are %s", name, strings.Join(cfg.ProjectNames(), ", "))
	}
	return sc, nil
}

// TurnScope is what the service says a turn works on: its issue's scope,
// and the files the issue's branch has changed, committed or not.
type TurnScope struct {
	repo.Scope
	Changed []string `json:"changed,omitempty"`
}

// UnknownError is a command that is neither the harness's nor a verb of the
// framework's.
type UnknownError struct{ Verb string }

func (e *UnknownError) Error() string {
	return fmt.Sprintf("unknown command %q: it is not the harness's, and the framework has no such verb", e.Verb)
}

// wrap turns a failed verb into the exit code the framework gave.
func wrap(err error) error {
	var df *dispatch.ErrFailed
	var de *dispatch.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &df):
		return &ExitError{Code: df.Code, Err: err}
	case errors.As(err, &de):
		return &Error{de.Msg}
	}
	return err
}

// ExitError is a verb that failed with an exit code of its own.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
