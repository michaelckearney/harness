package deploy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Releases is where the harness's releases are, by tag. Tests replace it.
var Releases = "https://github.com/michaelckearney/harness/releases/download/"

// Binary is the released harness-<goos>-<arch> of version, checked against
// the release's SHA256SUMS.
func Binary(ctx context.Context, version, goos, arch string) ([]byte, error) {
	base := Releases + version + "/"
	file := "harness-" + goos + "-" + arch
	sums, err := get(ctx, base+"SHA256SUMS")
	if err != nil {
		return nil, err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == file {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("harness %s has no %s in its SHA256SUMS", version, file)
	}
	data, err := get(ctx, base+file)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s of harness %s does not match its SHA256SUMS", file, version)
	}
	return data, nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
