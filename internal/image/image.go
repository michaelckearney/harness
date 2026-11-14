// Package image is the container image a monorepo's pipelines run its
// framework in: whatever the framework needs, from its own Dockerfile, so
// the pipelines know nothing of what kind of framework it is.
//
// The image is built from framework/ci, its Dockerfile and anything it
// copies, and tagged with that folder's git tree, so a commit that leaves it
// as it was reuses the image already published. The Dockerfile's VOLUME
// folders are its caches: the pipelines keep them between runs.
package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michaelckearney/harness/internal/repo"
)

// Dir is the image's folder, relative to the root.
const Dir = repo.FrameworkDir + "/ci"

// The environment that runs the framework in the image.
const (
	// EnvImage is the image to run the framework in: in the pipelines,
	// what harness image printed. Elsewhere it is unset, and the framework
	// runs as it is.
	EnvImage = "HARNESS_IMAGE"
	// EnvCache is the folder that keeps the image's VOLUME folders, one
	// folder each, for the pipelines to cache.
	EnvCache = "HARNESS_IMAGE_CACHE"
)

// Tag is the image's name for the commit checked out at root:
// ghcr.io/<owner>/<name>:ci-<tree>. It is the repository's own package,
// which the pipelines' token may publish.
func Tag(ctx context.Context, root string, cfg *repo.Config) (string, error) {
	tree, err := run(ctx, root, nil, "git", "rev-parse", "HEAD:"+Dir)
	if err != nil {
		return "", fmt.Errorf("%s is not in the commit checked out; it holds the Dockerfile the pipelines run the framework in", Dir)
	}
	return fmt.Sprintf("ghcr.io/%s:ci-%s", strings.ToLower(cfg.Repo), tree[:12]), nil
}

// Ensure makes the image tag there: with push, published, built and
// pushed if it is not, for the default branch's pipeline; without, here to
// run, pulled when it is published, else built, for a pull request's, which
// cannot publish. A push the registry refuses is only a warning: the image
// is here to run, and pull requests build it until it is published.
func Ensure(ctx context.Context, root, repoName, tag string, push bool, log io.Writer) error {
	if push {
		if _, err := run(ctx, root, nil, "docker", "manifest", "inspect", tag); err == nil {
			fmt.Fprintf(log, "harness: %s is published already\n", tag)
			return nil
		}
	} else if _, err := run(ctx, root, log, "docker", "pull", "--quiet", tag); err == nil {
		return nil
	}
	fmt.Fprintf(log, "harness: building %s from %s\n", tag, Dir)
	// The source label links the package to the repository on GHCR.
	if _, err := run(ctx, root, log, "docker", "build", "--tag", tag,
		"--label", "org.opencontainers.image.source=https://github.com/"+repoName, filepath.Join(root, Dir)); err != nil {
		return fmt.Errorf("docker build %s: %v", Dir, err)
	}
	if push {
		if _, err := run(ctx, root, log, "docker", "push", "--quiet", tag); err != nil {
			fmt.Fprintf(log, "::warning::harness: could not publish %s (%v), so pull requests build it until it is; on GitHub, check that the package %s lets this repository's Actions write to it\n",
				tag, err, strings.ToLower(repoName))
		}
	}
	return nil
}

// Command is argv run in the image, as the user running the harness, as it
// would run it: the root, and any folder in mounts, at the same paths; dir
// the working folder; env the environment, and nothing else of the
// harness's. The image's VOLUME folders are kept in cache, when it is set.
//
// The image runs as the harness's user, which it may not know: its HOME,
// and the folders under it that are not VOLUMEs, must be writable by anyone.
func Command(ctx context.Context, img, cache, dir string, mounts, env, argv []string, stdin bool) (*exec.Cmd, error) {
	vols, err := volumes(ctx, img)
	if err != nil {
		return nil, err
	}
	var binds []string
	for _, m := range mounts {
		binds = append(binds, m+":"+m)
	}
	if cache != "" {
		for _, v := range vols {
			// Made here, so the harness's user owns it, not docker's.
			host := filepath.Join(cache, strings.ReplaceAll(strings.Trim(v, "/"), "/", "%"))
			if err := os.MkdirAll(host, 0o755); err != nil {
				return nil, err
			}
			binds = append(binds, host+":"+v)
		}
	}
	args := []string{"run", "--rm", "--init", "--network=host", "--ipc=host",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		// Browsers sandbox themselves with namespaces a container's
		// defaults refuse; the runner is thrown away after the job.
		"--privileged",
		"--workdir", dir}
	if stdin {
		args = append(args, "--interactive")
	}
	for _, b := range binds {
		args = append(args, "--volume", b)
	}
	for _, e := range env {
		args = append(args, "--env", e)
	}
	args = append(append(args, img), argv...)
	return exec.CommandContext(ctx, "docker", args...), nil
}

// volumes are the image's VOLUME folders.
func volumes(ctx context.Context, img string) ([]string, error) {
	inspect := func() (string, error) {
		return run(ctx, "", nil, "docker", "image", "inspect", "--format", "{{json .Config.Volumes}}", img)
	}
	out, err := inspect()
	if err != nil {
		if _, perr := run(ctx, "", os.Stderr, "docker", "pull", "--quiet", img); perr != nil {
			return nil, fmt.Errorf("%s is not here, and pulling it failed; run harness image first: %v", img, perr)
		}
		if out, err = inspect(); err != nil {
			return nil, err
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, err
	}
	var vols []string
	for v := range m {
		vols = append(vols, v)
	}
	sort.Strings(vols)
	return vols, nil
}

func run(ctx context.Context, dir string, log io.Writer, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	}
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(errb.String()); s != "" {
			return "", fmt.Errorf("%v: %s", err, s)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
