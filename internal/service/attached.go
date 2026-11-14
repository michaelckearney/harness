package service

import (
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// attachedRef is a file the owner attached to an issue or comment, as the
// Markdown names it.
var attachedRef = regexp.MustCompile(`https://github\.com/user-attachments/assets/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// signedRef is where the rendered HTML of a private repository's comment
// says the file is: a link that works without credentials for a few
// minutes, with the attachment's id in its path.
var signedRef = regexp.MustCompile(`https://private-user-images\.githubusercontent\.com/[^"'\s<>]+`)

var uuid = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

const maxAttached = 50 << 20

var attachedClient = &http.Client{Timeout: time.Minute}

// attachedFiles replaces each file the owner attached to a body with the
// path of a copy on this machine, so the bot can look at it. In a private
// repository the Markdown's link needs GitHub credentials, which the bot
// does not have, so it saw only the link.
//
// Copies are kept in the thread's folder under the state directory, outside
// the worktree, so they are never committed, and fetched once each. A file
// that cannot be fetched keeps its link.
func (s *Service) attachedFiles(p *proj, n int, body, rendered string) string {
	dir := filepath.Join(s.cfg.StateDir, "attachments", p.cfg.Repo, fmt.Sprint(n))
	out, errs := localize(body, rendered, dir, attachedClient.Get)
	for _, err := range errs {
		s.log.Printf("%s#%d: %v", p.cfg.Repo, n, err)
	}
	return out
}

func localize(body, rendered, dir string, get func(string) (*http.Response, error)) (string, []error) {
	if !attachedRef.MatchString(body) {
		return body, nil
	}
	signed := map[string]string{}
	for _, u := range signedRef.FindAllString(rendered, -1) {
		u = html.UnescapeString(u)
		p, _, _ := strings.Cut(u, "?")
		if id := strings.ToLower(uuid.FindString(p)); id != "" && signed[id] == "" {
			signed[id] = u
		}
	}
	var errs []error
	out := attachedRef.ReplaceAllStringFunc(body, func(ref string) string {
		id := strings.ToLower(attachedRef.FindStringSubmatch(ref)[1])
		src := signed[id]
		if src == "" {
			// A public repository's link works as it is.
			src = ref
		}
		file, err := fetchAttached(dir, id, src, get)
		if err != nil {
			errs = append(errs, fmt.Errorf("attachment %s: %w", id, err))
			return ref
		}
		return file
	})
	return out, errs
}

func fetchAttached(dir, id, src string, get func(string) (*http.Response, error)) (string, error) {
	if have, _ := filepath.Glob(filepath.Join(dir, id+".*")); len(have) > 0 {
		return have[0], nil
	}
	resp, err := get(src)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	u, _, _ := strings.Cut(src, "?")
	ext := strings.ToLower(path.Ext(u))
	if !slices.Contains(mediaTypes, ext) {
		ext = ""
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		exts, _ := mime.ExtensionsByType(ct)
		for _, e := range exts {
			if slices.Contains(mediaTypes, e) {
				ext = e
				break
			}
		}
	}
	if ext == "" {
		return "", errors.New("not an image or video")
	}
	// The group's, for a bots' user: the state directory's own mode keeps
	// anyone else out.
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".part-"+id+"-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxAttached+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxAttached {
		return "", fmt.Errorf("larger than %d MB", maxAttached>>20)
	}
	file := filepath.Join(dir, id+ext)
	// CreateTemp makes it the service's alone; a bots' user reads it.
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return "", err
	}
	return file, os.Rename(tmp.Name(), file)
}
