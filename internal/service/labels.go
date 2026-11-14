package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/michaelckearney/harness/internal/repo"
)

// classify reads which agent works an issue, and on what, from the labels a
// trusted login put on it:
//
//	agent:<name>        the agent, such as agent:claude-code; agents.default without one
//	project:<name>      a project
//	framework           the framework, and every project
//
// An issue with no scope label is no bot's: scope is "", and nothing
// works it until a trigger user labels it.
func (s *Service) classify(p *proj, n int) (agent, scope string, err error) {
	cfg := p.repo
	labels, err := s.gh.LabelsBy(p.cfg.Repo, n, cfg.Trusted)
	if err != nil {
		return "", "", err
	}
	agent = cfg.Agents.Default
	var sc repo.Scope
	var agentLabels, scopeLabels []string
	for _, l := range labels {
		kind, name, _ := strings.Cut(l, ":")
		switch kind {
		case "agent":
			agentLabels = append(agentLabels, l)
			agent = name
		case repo.ProjectKind, repo.FrameworkKind:
			scopeLabels = append(scopeLabels, l)
			if sc, err = cfg.ScopeNamed(l); err != nil {
				return "", "", labelError(err.Error())
			}
		}
	}
	if len(agentLabels) > 1 {
		return "", "", labelError("this issue has more than one agent label (" + strings.Join(agentLabels, ", ") + "); keep one")
	}
	if len(scopeLabels) > 1 {
		return "", "", labelError("this issue has more than one scope label (" + strings.Join(scopeLabels, ", ") + "); keep one")
	}
	if !slices.Contains(cfg.Agents.Allowed, agent) {
		return "", "", labelError(fmt.Sprintf("agent %q is not in agents.allowed in harness.yaml; the allowed agents are %s", agent, strings.Join(cfg.Agents.Allowed, ", ")))
	}
	if len(scopeLabels) == 0 {
		return agent, "", nil
	}
	return agent, sc.Label(), nil
}

// labelError is a problem with an issue's labels, which the owner can fix.
type labelError string

func (e labelError) Error() string { return string(e) }

// labelProblem tells the owner once what is wrong with an issue's labels.
// Only the server that runs the default agent says so, so the issue hears
// it once however many servers watch it.
func (s *Service) labelProblem(p *proj, n int, err error) {
	if _, ok := err.(labelError); !ok {
		s.log.Printf("%s: labels: %v", key(p.cfg.Repo, n), err)
		return
	}
	if p.repo == nil || !s.cfg.Runs(p.repo.Agents.Default) {
		return
	}
	k := fmt.Sprintf("label-problem:%s#%d", p.cfg.Repo, n)
	if last, _ := s.st.Get(k); last == err.Error() {
		return
	}
	if cerr := s.gh.Comment(p.cfg.Repo, n, "harness: "+err.Error()+". Nothing works this issue until that is fixed."); cerr != nil {
		s.log.Printf("%s: %v", key(p.cfg.Repo, n), cerr)
		return
	}
	s.st.Set(k, err.Error())
}

// scopeOf is a thread's scope, by its label, as harness.yaml has it now. A
// label harness.yaml no longer knows writes nothing until the next poll
// tells the owner.
func (s *Service) scopeOf(p *proj, label string) repo.Scope {
	if label == "" {
		return repo.Scope{}
	}
	s.mu.Lock()
	cfg := p.repo
	s.mu.Unlock()
	if cfg != nil {
		if sc, err := cfg.ScopeNamed(label); err == nil {
			return sc
		}
	}
	kind, name, _ := strings.Cut(label, ":")
	return repo.Scope{Kind: kind, Name: name}
}
