// Package retry retries the pipelines' calls to the container registry, which
// fail now and then for a moment: a connection reset, a token refused, a
// gateway that times out. A run should not fail for those, nor wait long for
// a registry that is really down or a permission that is really missing.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Attempts is how many times Do tries.
const Attempts = 4

// Wait is the first wait between attempts; each one after doubles it, so
// about half a minute in all. Tests shorten it.
var Wait = 2 * time.Second

// Do runs fn until it succeeds, fails for good, or has had its attempts,
// saying on log before each retry what failed. It returns fn's last error.
func Do(ctx context.Context, log io.Writer, what string, fn func() error) error {
	wait := Wait
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || attempt == Attempts || !Transient(err) || ctx.Err() != nil {
			return err
		}
		fmt.Fprintf(log, "harness: retrying %s (%d/%d): %v\n", what, attempt+1, Attempts, err)
		jitter := time.Duration(rand.Int64N(int64(wait)/2 + 1))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait + jitter):
		}
		wait *= 2
	}
}

// transient is what the registry, docker, and the network say when trying
// again may well work.
var transient = []string{
	"denied", // GHCR refuses a token now and then, and takes it again a moment later
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"timeout", // i/o timeout, TLS handshake timeout, Client.Timeout
	"toomanyrequests",
	"too many requests",
	"internal server error",
	"bad gateway",
	"service unavailable",
	"gateway timeout",
}

// Transient is whether err is worth trying again.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	var te *transport.Error
	if errors.As(err, &te) && (te.StatusCode >= 500 || te.StatusCode == http.StatusTooManyRequests) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, t := range transient {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
