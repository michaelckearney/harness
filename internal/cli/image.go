package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/michaelckearney/harness/internal/image"
	"github.com/michaelckearney/harness/internal/repo"
)

const imageUsage = `usage: harness image [-push]

For the pipelines. Makes the image the framework runs in there, from
framework/ci/Dockerfile: here to run, pulled when it is published, else
built; or, with -push, published. Prints its name, ghcr.io/<owner>/<name>:ci-<tree>, where <tree> is
framework/ci's git tree, so a commit that leaves the folder as it was
reuses the image.

With HARNESS_IMAGE set to that name, the harness runs the framework in the
image; with HARNESS_IMAGE_CACHE set to a folder as well, the image's VOLUME
folders are kept there, for the pipeline to cache.

  -push   publish the image, built and pushed if it is not (the default
          branch's pipeline); a push the registry refuses is a warning, and
          the image built is used
`

// Image runs `harness image`.
func Image(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	push := fs.Bool("push", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, imageUsage)
			return nil
		}
		return fail("harness image: %v\n\n%s", err, imageUsage)
	}
	cwd, _ := os.Getwd()
	root, err := repo.FindRoot(cwd)
	if err != nil {
		return &Error{err.Error()}
	}
	cfg, err := repo.Load(root)
	if err != nil {
		return &Error{err.Error()}
	}
	tag, err := image.Tag(ctx, root, cfg)
	if err != nil {
		return &Error{err.Error()}
	}
	if err := image.Ensure(ctx, root, cfg.Repo, tag, *push, stderr); err != nil {
		return &Error{err.Error()}
	}
	fmt.Fprintln(stdout, tag)
	return nil
}
