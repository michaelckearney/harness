package service

import (
	"bytes"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/michaelckearney/harness/internal/repo"
)

// syncSettings reads harness.yaml from the default branch when the default
// branch has moved. One that is not valid leaves the bots as they were; a
// monorepo with none has no bots until it does.
func (s *Service) syncSettings(p *proj) error {
	g, err := s.git(p, "")
	if err != nil {
		return err
	}
	ref := "refs/remotes/origin/" + p.base
	blob, err := g.Run("rev-parse", "-q", "--verify", ref+":"+repo.File)
	if err != nil {
		blob = ""
	}
	if blob == p.tree {
		return nil
	}
	if blob == "" {
		s.mu.Lock()
		p.tree, p.repo, p.pin = blob, nil, ""
		s.mu.Unlock()
		s.log.Printf("%s: %s has no %s; no bots until it does", p.cfg.Repo, p.base, repo.File)
		return nil
	}
	data, err := g.Run("show", ref+":"+repo.File)
	if err != nil {
		return err
	}
	// The pin counts even in a harness.yaml this harness cannot read: one
	// that only the harness it pins can is how a monorepo moves to it.
	s.mu.Lock()
	p.pin = pinOf([]byte(data))
	s.mu.Unlock()
	cfg, err := repo.Parse([]byte(data))
	if err == nil && cfg.Repo != p.cfg.Repo {
		err = &repo.Error{Msg: "repo is " + cfg.Repo + ", but this service is for " + p.cfg.Repo}
	}
	if err != nil {
		p.tree = blob
		s.log.Printf("%s: %v; the bots are unchanged", p.cfg.Repo, err)
		return nil
	}
	s.mu.Lock()
	p.repo, p.tree = cfg, blob
	s.mu.Unlock()
	s.log.Printf("%s: %s read from %s %s", p.cfg.Repo, repo.File, p.base, short(blob))
	return nil
}

// pinOf is the harness a harness.yaml pins, read without checking the rest.
func pinOf(data []byte) string {
	var v struct {
		Harness string `yaml:"harness"`
	}
	if yaml.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.Harness
}

// writeFile replaces a file only when its content changes.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return os.Chmod(path, mode)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
