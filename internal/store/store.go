// Package store keeps what harness serve cannot read back from GitHub: which
// agent and session belong to which issue, and which events a turn has
// already answered. Everything else is rebuilt from GitHub on every poll.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// States of a thread.
const (
	Idle    = "idle"
	Running = "running"
	Blocked = "blocked"
	Closed  = "closed"
)

// Thread is one issue and the pull request the bot opened for it.
type Thread struct {
	Repo   string
	Number int
	State  string
	// Agent is the agent that works the thread, and Session its session,
	// empty until the first turn.
	Agent   string
	Session string
	// Scope is the label of what the thread works on, such as
	// project:harbor, or "" for the whole monorepo.
	Scope   string
	PR      int  // 0 until the first push
	Nudged  bool // the current batch's turn has already been resumed once
	Batch   []Event
	Outcome string // "done" once the running turn has ended; empty while it runs
	Started time.Time
	Replied bool // the running turn has replied in the conversation
	// PRTitle and PRBody are the pull request's title and description as
	// the harness last published them, from PRSource, the sum of the bot's
	// pull request file then: what anyone else writes there is put back.
	PRTitle, PRBody, PRSource string
	// TurnHead is the branch's commit when the running turn started, so
	// done can tell whether the turn committed.
	TurnHead string
	// TestedTree is the tree a full harness test of the thread's scope
	// last ran on, and
	// TestedPass whether it passed.
	TestedTree string
	TestedPass bool
}

// Event is something on GitHub a turn answers. Key identifies it, so it is
// answered once.
type Event struct {
	Key  string    `json:"key"`
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	// Link is Markdown linking to the event, for one on the pull request,
	// which the bot's reply in the issue points back to.
	Link string `json:"link,omitempty"`
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS threads (
	repo TEXT NOT NULL,
	number INTEGER NOT NULL,
	state TEXT NOT NULL,
	session TEXT NOT NULL DEFAULT '',
	pr INTEGER NOT NULL DEFAULT 0,
	nudged INTEGER NOT NULL DEFAULT 0,
	batch TEXT NOT NULL DEFAULT '[]',
	outcome TEXT NOT NULL DEFAULT '',
	started INTEGER NOT NULL DEFAULT 0,
	replied INTEGER NOT NULL DEFAULT 0,
	described TEXT NOT NULL DEFAULT '',
	turn_head TEXT NOT NULL DEFAULT '',
	tested_tree TEXT NOT NULL DEFAULT '',
	tested_pass INTEGER NOT NULL DEFAULT 0,
	agent TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL DEFAULT '',
	pr_title TEXT NOT NULL DEFAULT '',
	pr_body TEXT NOT NULL DEFAULT '',
	pr_source TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repo, number)
);
CREATE TABLE IF NOT EXISTS handled (
	repo TEXT NOT NULL,
	number INTEGER NOT NULL,
	key TEXT NOT NULL,
	PRIMARY KEY (repo, number, key)
);
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Columns added since a database was made.
	for _, col := range added {
		if _, err := db.Exec(`ALTER TABLE threads ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	// The service's own, whatever its umask: the state directory is shared
	// with the bots' group. SQLite gives the -wal and -shm files the
	// database's mode.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, err
		}
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// added are the columns of threads newer than the first schema, which Open
// adds to an older database. (described is unused: it was the commit the
// last harness describe was for.)
var added = []string{
	"pr_title TEXT NOT NULL DEFAULT ''",
	"pr_body TEXT NOT NULL DEFAULT ''",
	"pr_source TEXT NOT NULL DEFAULT ''",
}

const columns = `repo, number, state, session, pr, nudged, batch, outcome, started, replied, pr_title, pr_body, pr_source, turn_head, tested_tree, tested_pass, agent, scope`

func scan(row interface{ Scan(...any) error }) (*Thread, error) {
	var t Thread
	var batch string
	var started int64
	if err := row.Scan(&t.Repo, &t.Number, &t.State, &t.Session, &t.PR, &t.Nudged, &batch, &t.Outcome, &started, &t.Replied, &t.PRTitle, &t.PRBody, &t.PRSource, &t.TurnHead, &t.TestedTree, &t.TestedPass, &t.Agent, &t.Scope); err != nil {
		return nil, err
	}
	if started > 0 {
		t.Started = time.Unix(started, 0)
	}
	return &t, json.Unmarshal([]byte(batch), &t.Batch)
}

// Thread returns a thread, or nil.
func (s *Store) Thread(repo string, n int) (*Thread, error) {
	t, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM threads WHERE repo = ? AND number = ?`, repo, n))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// Threads returns every thread that is not closed.
func (s *Store) Threads() ([]*Thread, error) {
	rows, err := s.db.Query(`SELECT ` + columns + ` FROM threads WHERE state != 'closed' ORDER BY repo, number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Thread
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Save writes a thread.
func (s *Store) Save(t *Thread) error {
	if t.Batch == nil {
		t.Batch = []Event{}
	}
	batch, _ := json.Marshal(t.Batch)
	var started int64
	if !t.Started.IsZero() {
		started = t.Started.Unix()
	}
	_, err := s.db.Exec(`INSERT INTO threads (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo, number) DO UPDATE SET state = excluded.state, session = excluded.session,
		pr = excluded.pr, nudged = excluded.nudged, batch = excluded.batch, outcome = excluded.outcome,
		started = excluded.started, replied = excluded.replied,
		pr_title = excluded.pr_title, pr_body = excluded.pr_body, pr_source = excluded.pr_source,
		turn_head = excluded.turn_head, tested_tree = excluded.tested_tree, tested_pass = excluded.tested_pass,
		agent = excluded.agent, scope = excluded.scope`,
		t.Repo, t.Number, t.State, t.Session, t.PR, t.Nudged, string(batch), t.Outcome, started, t.Replied, t.PRTitle, t.PRBody, t.PRSource,
		t.TurnHead, t.TestedTree, t.TestedPass, t.Agent, t.Scope)
	return err
}

// Handle records that a turn answered these events.
func (s *Store) Handle(repo string, n int, events []Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO handled (repo, number, key) VALUES (?, ?, ?)`, repo, n, e.Key); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Handled returns the keys of the events a thread has answered.
func (s *Store) Handled(repo string, n int) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT key FROM handled WHERE repo = ? AND number = ?`, repo, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

func (s *Store) Get(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
