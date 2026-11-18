package retry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

func TestTransient(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("read tcp 10.1.0.4:443: read: connection reset by peer"), true},
		{errors.New("GET https://ghcr.io/token?scope=x: DENIED: denied"), true},
		{errors.New("exit status 1: Error response from daemon: error from registry: denied"), true},
		{errors.New("net/http: TLS handshake timeout"), true},
		{errors.New("dial tcp: i/o timeout"), true},
		{errors.New("toomanyrequests: retry later"), true},
		{errors.New("received unexpected HTTP status: 503 Service Unavailable"), true},
		{&transport.Error{StatusCode: http.StatusBadGateway}, true},
		{&transport.Error{StatusCode: http.StatusTooManyRequests}, true},
		{&transport.Error{StatusCode: http.StatusNotFound}, false},
		{errors.New("manifest unknown"), false},
		{errors.New("invalid reference format"), false},
		{errors.New("../x is outside the cache"), false},
		{nil, false},
	} {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("Transient(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestDo(t *testing.T) {
	Wait = time.Millisecond
	ctx := context.Background()

	// A transient failure is tried again, and said so.
	var log strings.Builder
	calls := 0
	err := Do(ctx, &log, "docker pull x", func() error {
		if calls++; calls < 3 {
			return errors.New("connection reset by peer")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("Do = %v after %d calls", err, calls)
	}
	if !strings.Contains(log.String(), "harness: retrying docker pull x (2/4): connection reset by peer") {
		t.Errorf("log:\n%s", log.String())
	}

	// One that is not, is not.
	calls = 0
	err = Do(ctx, io.Discard, "x", func() error { calls++; return errors.New("manifest unknown") })
	if err == nil || calls != 1 {
		t.Errorf("permanent: Do = %v after %d calls", err, calls)
	}

	// Nor one that keeps failing, past its attempts.
	calls = 0
	err = Do(ctx, io.Discard, "x", func() error { calls++; return errors.New("DENIED: denied") })
	if err == nil || calls != Attempts {
		t.Errorf("persistent: Do = %v after %d calls", err, calls)
	}
}
