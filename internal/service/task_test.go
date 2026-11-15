package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// push commits what the test wrote in its clone to main.
func (h *harness) push(msg string) {
	h.t.Helper()
	gitIn(h.t, h.work, "add", "-A")
	gitIn(h.t, h.work, "commit", "--quiet", "-m", msg)
	gitIn(h.t, h.work, "push", "--quiet", "origin", "main")
}

func TestTasks(t *testing.T) {
	h := setup(t)
	chat := newFakeChat()
	h.s.chat = chat
	h.s.cfg.Discord.Owner, h.s.cfg.Discord.DigestHour = "42", 99 // no digest
	yaml := h.read("../work/harness.yaml")
	task := func(before string) {
		write(t, filepath.Join(h.work, "harness.yaml"), yaml+
			fmt.Sprintf("tasks: { design: { prompt: tasks/design.md, before: %q, label: design, max_open: 1 } }\n", before))
	}
	write(t, filepath.Join(h.work, "tasks/design.md"), "Propose one small change to the UI.\n")
	task(`mkdir -p shots && echo png > shots/header.png && echo ran >> "$FAKE_DIR/before"`)
	h.push("Add the design task")
	runs := func() int { return strings.Count(h.read("prompts"), "This is a run of the monorepo's design task") }

	// A free slot: before runs, then the agent, which opens one issue as the
	// owner, with its scope and the task's label.
	write(t, filepath.Join(h.fake, "scenario"), "task")
	h.step()
	if runs() != 1 || h.read("before") != "ran\n" {
		t.Fatalf("runs %d, before %q", runs(), h.read("before"))
	}
	prompts := h.read("prompts")
	for _, want := range []string{"Propose one small change to the UI.", "fewer than 1 open issues carry its label, design: 0 do", "ran in your folder and passed"} {
		if !strings.Contains(prompts, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompts)
		}
	}
	if got := h.read("task-shots"); got != "header.png\n" {
		t.Errorf("what before left: %q", got)
	}
	issues := h.gh.issuesTitled("feat: tidy the header")
	if len(issues) != 1 || issues[0].User != "owner" || !strings.Contains(issues[0].Body, "From the design task.") {
		t.Fatalf("the issues: %+v\n%s", issues, h.read("out"))
	}
	n := issues[0].Number
	if got := fmt.Sprint(h.gh.issueJSON(&issues[0])["labels"]); got != "[map[name:framework] map[name:design]]" {
		t.Errorf("its labels: %s", got)
	}
	out := h.read("out")
	for _, want := range []string{"exactly one scope label", "already opened", "nobody to reply to", "changes nothing else", "A bot starts on it soon"} {
		if !strings.Contains(out, want) {
			t.Errorf("what the bot was told lacks %q:\n%s", want, out)
		}
	}
	if len(h.gh.issuesTitled("feat: another")) != 0 {
		t.Error("a run opened a second issue")
	}

	// The slot is taken: no run, while a bot works the issue.
	write(t, filepath.Join(h.fake, "scenario"), "quiet")
	h.step()
	if runs() != 1 {
		t.Errorf("ran with no free slot: %d", runs())
	}
	if th := h.thread(n); th.Scope != "framework" {
		t.Errorf("the issue's thread: %+v", th)
	}

	// The issue closes: a run, which opens nothing; then none until main
	// moves on.
	h.gh.close(n)
	write(t, filepath.Join(h.fake, "scenario"), "nothing")
	h.step()
	h.step()
	if runs() != 2 {
		t.Errorf("runs after the issue closed: %d", runs())
	}
	write(t, filepath.Join(h.work, "README.md"), "demo, changed\n")
	h.push("Change the README")
	h.step()
	if runs() != 3 {
		t.Errorf("runs after main moved on: %d", runs())
	}

	// before fails: the owner hears of it in Discord, once, and the agent
	// does not run.
	task(`echo "an e2e test failed"; exit 3`)
	h.push("Break before")
	h.step()
	h.step()
	if runs() != 3 {
		t.Errorf("the agent ran after before failed: %d", runs())
	}
	var posted []string
	for _, l := range chat.log {
		if strings.Contains(l, "did not run") {
			posted = append(posted, l)
		}
	}
	if len(posted) != 1 || posted[0] != "post #daily-digest ⚠️ The design task did not run" {
		t.Errorf("what Discord was told: %q", chat.log)
	}
	for _, m := range chat.msgs {
		if strings.Contains(m.Title, "did not run") && !strings.Contains(m.Description, "an e2e test failed") {
			t.Errorf("the message: %+v", m)
		}
	}
}
