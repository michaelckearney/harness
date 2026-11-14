// Command harness runs a monorepo's frameworks and projects, and the bots
// that work its GitHub issues.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/michaelckearney/harness/internal/cli"
	"github.com/michaelckearney/harness/internal/command"
	"github.com/michaelckearney/harness/internal/version"
)

const usage = `usage: harness <command>

In a monorepo, for its projects and its framework:

  test [-p <name>] [-since <ref>] [args]
                              test a project, or the framework and every
                              project; with -since, only what the changes
                              since <ref> affect; args go to the framework
  build [-p <project>] ...    build a project's release artifacts
  <verb> [-p <project>] ...   one of the framework's own verbs, listed below
  check                       check harness.yaml and the framework
  release -p <project>        release a project: see harness release -h
  image [-push]               the image the pipelines run the framework in:
                              see harness image -h
  ci <pipeline>               a pipeline, as one command: see harness ci -h
  init [--from <dir>]         make this folder a monorepo

-p names a project, or framework for the framework and every project.
Without it, a command is for the project whose folder it runs in, or,
anywhere else, for the framework and every project.

On a server:

  deploy -server <file> <url>         set up the bots for a monorepo: see harness deploy -h
  serve [-validate] --config <file>   the bots' service
  queue --config <file>               what the service is running and waiting

  version                     print the harness's version
`

// notInTurn are the harness's commands a bot has no use for during its
// turn. Which of the framework's verbs a bot may run, framework.yaml says.
var notInTurn = map[string]string{
	"check":   "harness check is the developer's",
	"release": "harness release is the default branch's pipeline's",
	"image":   "harness image is the pipelines'",
	"ci":      "harness ci is the pipelines'",
	"deploy":  "harness deploy is the server's",
	"init":    "harness init makes a new monorepo",
	"serve":   "harness serve is the server's service",
	"queue":   "harness queue is the server's",
}

// botUsage is the bot's help: its commands, and the framework's verbs it
// may run.
func botUsage() string {
	if v := cli.FrameworkVerbs(true); v != "" {
		return command.BotUsage + "\n" + v
	}
	return command.BotUsage
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if command.InTurn() {
		if why, ok := notInTurn[cmd]; ok {
			fmt.Fprintf(os.Stderr, "harness: %s.\n\n%s", why, botUsage())
			os.Exit(2)
		}
		if cmd == "-h" || cmd == "--help" || cmd == "help" {
			fmt.Print(botUsage())
			return
		}
	}
	switch {
	case cmd == "serve":
		os.Exit(command.Serve(args))
	case cmd == "queue":
		os.Exit(command.Queue(args))
	case command.IsBotCommand(cmd):
		os.Exit(command.Bot(cmd, args))
	case cmd == "version" || cmd == "--version" || cmd == "-version":
		fmt.Println(version.Get())
		return
	case cmd == "-h" || cmd == "--help" || cmd == "help":
		fmt.Print(usage)
		if v := cli.FrameworkVerbs(false); v != "" {
			fmt.Print("\n" + v)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch {
	case cmd == "check":
		err = cli.Check(os.Stdout)
	case cmd == "init":
		err = cli.Init(args, os.Stdout)
	case cmd == "release":
		err = cli.Release(ctx, args, os.Stdout, os.Stderr)
	case cmd == "deploy":
		err = cli.Deploy(ctx, args, os.Stdout, os.Stderr)
	case cmd == "image":
		err = cli.Image(ctx, args, os.Stdout, os.Stderr)
	case cmd == "ci":
		err = cli.Ci(ctx, args, os.Stdout, os.Stderr)
	default:
		err = cli.Verb(ctx, cmd, args, os.Stdout, os.Stderr)
	}
	var exit *cli.ExitError
	var unknown *cli.UnknownError
	var user *cli.Error
	switch {
	case err == nil:
	case errors.As(err, &unknown):
		fmt.Fprintf(os.Stderr, "harness: %v\n\n", err)
		if command.InTurn() {
			fmt.Fprint(os.Stderr, botUsage())
		} else {
			fmt.Fprint(os.Stderr, usage)
			if v := cli.FrameworkVerbs(false); v != "" {
				fmt.Fprint(os.Stderr, "\n"+v)
			}
		}
		os.Exit(2)
	case errors.As(err, &exit):
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(exit.Code)
	case errors.As(err, &user):
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "harness: unexpected error: %v\n", err)
		os.Exit(1)
	}
}
