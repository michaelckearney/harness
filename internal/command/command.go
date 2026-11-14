// Package command is harness's commands for the bots: the ones a bot runs
// during its turn, such as `harness reply`, and `harness serve` and
// `harness queue`, which the machine runs.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/michaelckearney/harness/internal/api"
	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/rpc"
	"github.com/michaelckearney/harness/internal/service"
	"github.com/michaelckearney/harness/internal/version"
)

// BotUsage is the harness's help during a bot's turn: the commands a bot
// has.
const BotUsage = `usage: harness <command>, during a bot's turn

  harness status              the request, the conversation, your changes, CI
  harness diff                your changes against the default branch
  harness commit "<what>"     commit everything and push, and publish .harness/pr.md
  harness sync                merge the default branch into yours
  harness rm <path>...        delete files or folders from your worktree
  harness reply -             say something in the conversation
  harness done                end the turn
  harness test [args]         test what this issue works on; args go to the framework

test takes -p <project> first to test one project of several. With no
arguments it tests what your changes affect: the projects you changed, or,
when you changed the framework, the framework and every project.

Every turn ends with done, after a reply if a person asked for something.
If you committed this turn, run the whole harness test, with no arguments,
before done.

.harness/pr.md is your pull request. Its first line is the title, a
conventional commit such as "feat: add an export button" or "fix: keep the
window size" (feat! for a breaking change); the rest describes the changes.
Edit it with your file tools and keep it current; each commit publishes it.

The text for reply is Markdown, on standard input with -, or from a file.
An image in it, ![what it shows](path), is uploaded and shown in place, and
a video on a line of its own plays; a relative path starts from the file's
folder, and the file must be in your worktree.
`

// InTurn reports whether a bot is running the harness, during its turn: the
// service puts its socket in the bot's environment.
func InTurn() bool { return host.TurnSocket() != "" }

// IsBotCommand reports whether cmd is one of a bot's own commands.
func IsBotCommand(cmd string) bool {
	switch cmd {
	case "status", "diff", "commit", "sync", "reply", "done", "rm":
		return true
	}
	return false
}

// Bot runs one of a bot's commands, `harness <cmd> <args>`.
func Bot(cmd string, rest []string) int {
	if !InTurn() {
		return fail("harness %s is a bot's command, for its turn; a turn is the only place it works", cmd)
	}
	switch cmd {
	case "status", "diff", "sync":
		if len(rest) > 0 {
			return fail("%s takes no arguments", cmd)
		}
		return bot(cmd, nil, "", "")
	case "commit":
		if len(rest) == 0 {
			return fail("say what the commit does: harness commit \"Fix the date in the header\"")
		}
		return bot(cmd, rest, "", "")
	case "done":
		if len(rest) > 0 {
			return fail("done takes no arguments")
		}
		return bot(cmd, nil, "", "")
	case "rm":
		if len(rest) == 0 {
			return fail("say what to delete: harness rm backend/old.go")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fail("%v", err)
		}
		out, err := remove(cwd, rest)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println(out)
		return 0
	case "reply":
		if len(rest) != 1 {
			return fail("%s takes Markdown on standard input: harness %s - <<'EOF' ... EOF", cmd, cmd)
		}
		body, dir, err := readBody(rest[0])
		if err != nil {
			return fail("%v", err)
		}
		return bot(cmd, nil, body, dir)
	}
	return fail("%q is not a bot command; run harness help", cmd)
}

// TestBegin asks the service for the tree a full harness test of scope, a
// label such as project:harbor or "" for everything, is about to run on,
// during a bot's turn. An error leaves the run unrecorded.
func TestBegin(scope string) (string, error) {
	resp, err := botCall("test-begin", []string{scope})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Output), nil
}

// Tested tells the service whether a full harness test passed on tree.
func Tested(tree string, pass bool) error {
	result := "fail"
	if pass {
		result = "pass"
	}
	_, err := botCall("tested", []string{tree, result})
	return err
}

func botCall(cmd string, args []string) (*rpc.Response, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	resp, err := rpc.Call(host.TurnSocket(), rpc.Request{Cwd: cwd, Cmd: cmd, Args: args})
	if err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%s", resp.Output)
	}
	return resp, nil
}

// Queue runs `harness queue [--config <file>]`: what is running and waiting.
func Queue(args []string) int {
	if len(args) != 2 || args[0] != "--config" {
		return fail("usage: harness queue --config <file>")
	}
	cfg, err := host.Load(args[1])
	if err != nil {
		return fail("%v", err)
	}
	socket := cfg.Socket()
	return callAt(socket, rpc.Request{Cmd: "queue"})
}

// readBody reads Markdown from a file, or standard input for -, and says
// where its relative paths start.
func readBody(arg string) (string, string, error) {
	if arg == "-" {
		b, err := io.ReadAll(os.Stdin)
		dir, _ := os.Getwd()
		return string(b), dir, err
	}
	b, err := os.ReadFile(arg)
	if err != nil {
		return "", "", fmt.Errorf("cannot read %s: write the text to a file first, or pass - and send it on standard input", arg)
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", "", err
	}
	return string(b), filepath.Dir(abs), nil
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "harness: "+format+"\n", args...)
	return 1
}

func bot(cmd string, args []string, body, dir string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail("%v", err)
	}
	return call(rpc.Request{Cwd: cwd, Cmd: cmd, Args: args, Body: body, BodyDir: dir})
}

func call(req rpc.Request) int { return callAt(host.TurnSocket(), req) }

func callAt(socket string, req rpc.Request) int {
	resp, err := rpc.Call(socket, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		return 2
	}
	w := os.Stdout
	if resp.Code != 0 {
		w = os.Stderr
	}
	if resp.Output != "" {
		fmt.Fprintln(w, resp.Output)
	}
	return resp.Code
}

// Serve runs `harness serve [-validate] --config <file>`: the bots'
// service, or, with -validate, a check of its configuration.
func Serve(args []string) int {
	path, validate := "", false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-validate" || args[i] == "--validate":
			validate = true
		case args[i] == "--config" && i+1 < len(args):
			path = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			path = strings.TrimPrefix(args[i], "--config=")
		default:
			return fail("usage: harness serve [-validate] --config <file>")
		}
	}
	if path == "" {
		return fail("usage: harness serve [-validate] --config <file>")
	}
	cfg, err := host.Load(path)
	if err != nil {
		return fail("%v", err)
	}
	if validate {
		fmt.Printf("%s is valid: %s, agents %s\n", path, cfg.Repo, strings.Join(cfg.Agents, ", "))
		return 0
	}
	logger := log.New(os.Stderr, "", 0)
	s, err := service.New(cfg, logger)
	if err != nil {
		return fail("%v", err)
	}
	if s.ConfigPath, err = filepath.Abs(path); err != nil {
		return fail("%v", err)
	}
	// The first signal lets the running turns finish; a second stops them.
	// A restart between turns then never cuts a bot off mid-turn.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := make(chan struct{})
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
		<-signals
		close(now)
	}()
	// The API outlives the first signal, so the app can watch the last
	// turns finish.
	apiCtx, endAPI := context.WithCancel(context.Background())
	defer endAPI()
	if cfg.API.Listen != "" {
		ln, err := api.Listen(cfg.API)
		if err != nil {
			return fail("%v", err)
		}
		go func() {
			if err := api.New(s, cfg.API, version.Get(), nil, logger).Serve(apiCtx, ln); err != nil {
				logger.Printf("api: %v", err)
			}
		}()
	}
	if err := s.Run(ctx, now); errors.Is(err, service.ErrUpdated) {
		logger.Printf("serve: %v", err)
		return service.ExitUpdated
	} else if err != nil {
		return fail("%v", err)
	}
	return 0
}

// Scope asks the service what the turn's issue works on, as JSON.
func Scope() (string, error) {
	resp, err := botCall("scope", nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Output), nil
}

// remove deletes paths, relative to cwd, from the bot's worktree: the
// folder above cwd that holds .harness/pr.md. The next harness commit
// commits the deletion. It runs as the bot, so it deletes only what the bot
// could change anyway, and refuses anything outside the worktree, and its
// .git and .harness.
func remove(cwd string, paths []string) (string, error) {
	root, err := worktreeRoot(cwd)
	if err != nil {
		return "", err
	}
	var targets []string
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, p)
		}
		abs = filepath.Clean(abs)
		// A link in a parent folder must not lead out of the worktree.
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return "", fmt.Errorf("%s: no such file or folder", p)
		}
		abs = filepath.Join(parent, filepath.Base(abs))
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s is not in your worktree", p)
		}
		if first := strings.Split(rel, string(filepath.Separator))[0]; first == ".git" || first == ".harness" {
			return "", fmt.Errorf("%s is the harness's; leave it", p)
		}
		if _, err := os.Lstat(abs); err != nil {
			return "", fmt.Errorf("%s: no such file or folder", p)
		}
		targets = append(targets, abs)
	}
	var done []string
	for _, abs := range targets {
		if err := os.RemoveAll(abs); err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, abs)
		done = append(done, filepath.ToSlash(rel))
	}
	return "Deleted " + strings.Join(done, ", ") + ". harness commit commits the deletion.", nil
}

// worktreeRoot is the folder at or above dir that holds .harness/pr.md, with
// its links resolved.
func worktreeRoot(dir string) (string, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".harness", "pr.md")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("%s is not in a bot's worktree", dir)
		}
	}
}
