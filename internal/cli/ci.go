package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/michaelckearney/harness/internal/cache"
	"github.com/michaelckearney/harness/internal/image"
	"github.com/michaelckearney/harness/internal/repo"
)

const ciUsage = `usage: harness ci <pipeline>

The pipelines, each one command, so a CI service only checks out the
monorepo, installs the harness harness.yaml pins, and runs one of these:

  pr <base>          a pull request onto <base>, such as origin/main:
                     harness check, then harness test on what it affects,
                     from the default branch's test cache, which it does
                     not change
  test               the default branch: harness check, then harness test on
                     everything, then the test cache pull requests start from
  release            the default branch: publish the framework's image if it
                     changed, then harness release, with its own cache

Nothing here deploys: the servers update themselves to the harness
harness.yaml pins (harness deploy -h).

They run the framework in its image (harness image -h), and keep its caches
in the registry beside it, as ci-cache-test and ci-cache-release. With
GH_TOKEN set, they log in to the registry with it: a pull request's needs
to read packages, the default branch's to write them.
`

// Ci runs `harness ci`.
func Ci(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, ciUsage)
		if len(args) == 0 {
			return fail("harness ci needs a pipeline")
		}
		return nil
	}
	pipeline, args := args[0], args[1:]
	switch pipeline {
	case "pr":
		if len(args) != 1 {
			return fail("harness ci pr needs the base it merges onto, such as origin/main\n\n%s", ciUsage)
		}
		return ciPR(ctx, args[0], stdout, stderr)
	case "test":
		return ciTest(ctx, stdout, stderr)
	case "release":
		return ciRelease(ctx, stdout, stderr)
	}
	return fail("harness ci has no pipeline %q\n\n%s", pipeline, ciUsage)
}

func ciPR(ctx context.Context, base string, stdout, stderr io.Writer) error {
	if err := Check(stdout); err != nil {
		return err
	}
	img, err := ciImage(ctx, false, stderr)
	if err != nil {
		return err
	}
	if err := cache.Restore(ctx, cache.Ref(img, "test"), os.Getenv(image.EnvCache), stderr); err != nil {
		return &Error{err.Error()}
	}
	return Verb(ctx, "test", []string{"-p", "framework", "-since", base}, stdout, stderr)
}

func ciTest(ctx context.Context, stdout, stderr io.Writer) error {
	if err := Check(stdout); err != nil {
		return err
	}
	img, err := ciImage(ctx, false, stderr)
	if err != nil {
		return err
	}
	ref, dir := cache.Ref(img, "test"), os.Getenv(image.EnvCache)
	if err := cache.Restore(ctx, ref, dir, stderr); err != nil {
		return &Error{err.Error()}
	}
	testErr := Verb(ctx, "test", []string{"-p", "framework"}, stdout, stderr)
	// What the tests downloaded and built is worth keeping even when one
	// failed: the next run needs it all the same.
	if err := ciSave(ctx, ref, dir, stderr); err != nil {
		return err
	}
	return testErr
}

func ciRelease(ctx context.Context, stdout, stderr io.Writer) error {
	img, err := ciImage(ctx, true, stderr)
	if err != nil {
		return err
	}
	var plan bytes.Buffer
	if err := Release(ctx, []string{"-plan"}, &plan, stderr); err != nil {
		return err
	}
	if strings.TrimSpace(plan.String()) == "" {
		fmt.Fprintln(stderr, "harness: nothing to release")
		return nil
	}
	fmt.Fprint(stderr, plan.String())
	ref, dir := cache.Ref(img, "release"), os.Getenv(image.EnvCache)
	if err := cache.Restore(ctx, ref, dir, stderr); err != nil {
		return &Error{err.Error()}
	}
	relErr := Release(ctx, nil, stdout, stderr)
	if err := ciSave(ctx, ref, dir, stderr); err != nil {
		return err
	}
	return relErr
}

// ciImage readies the runner and the framework's image, published with push,
// and sets the environment that runs the framework in it, with its caches in
// a folder of their own. It returns the image's name.
func ciImage(ctx context.Context, push bool, stderr io.Writer) (string, error) {
	root, err := ciRoot()
	if err != nil {
		return "", err
	}
	cfg, err := repo.Load(root)
	if err != nil {
		return "", &Error{err.Error()}
	}
	tag, err := image.Tag(ctx, root, cfg)
	if err != nil {
		return "", &Error{err.Error()}
	}
	if token := os.Getenv("GH_TOKEN"); token != "" {
		registry, _, _ := strings.Cut(tag, "/")
		owner, _, _ := strings.Cut(cfg.Repo, "/")
		login := exec.CommandContext(ctx, "docker", "login", registry, "-u", owner, "--password-stdin")
		login.Stdin = strings.NewReader(token)
		if out, err := login.CombinedOutput(); err != nil {
			return "", fail("docker login %s: %v: %s", registry, err, bytes.TrimSpace(out))
		}
	}
	allowUserNamespaces(ctx, stderr)
	if err := image.Ensure(ctx, root, cfg.Repo, tag, push, stderr); err != nil {
		return "", &Error{err.Error()}
	}
	dir, err := os.MkdirTemp("", "harness-image-cache-")
	if err != nil {
		return "", err
	}
	os.Setenv(image.EnvImage, tag)
	os.Setenv(image.EnvCache, dir)
	return tag, nil
}

// allowUserNamespaces lifts Ubuntu's restriction on the user namespaces
// browsers sandbox themselves in, where the runner has it: the runner is
// thrown away after the job.
func allowUserNamespaces(ctx context.Context, stderr io.Writer) {
	const knob = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	if b, err := os.ReadFile(knob); err != nil || strings.TrimSpace(string(b)) != "1" {
		return
	}
	if out, err := exec.CommandContext(ctx, "sudo", "-n", "sysctl", "-q", "-w", "kernel.apparmor_restrict_unprivileged_userns=0").CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "::warning::harness: could not allow user namespaces, which the browsers' sandboxes need: %v: %s\n", err, bytes.TrimSpace(out))
	}
}

// ciSave publishes the cache, unless the run was stopped, and says how much
// room the runner has left.
func ciSave(ctx context.Context, ref, dir string, stderr io.Writer) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	if err := cache.Save(ctx, ref, dir, stderr); err != nil {
		return &Error{err.Error()}
	}
	if df, err := exec.CommandContext(ctx, "df", "-h", "/").Output(); err == nil {
		fmt.Fprintf(stderr, "harness: the runner's disk after the run:\n%s", df)
	}
	return nil
}

func ciRoot() (string, error) {
	cwd, _ := os.Getwd()
	root, err := repo.FindRoot(cwd)
	if err != nil {
		return "", &Error{err.Error()}
	}
	return root, nil
}
