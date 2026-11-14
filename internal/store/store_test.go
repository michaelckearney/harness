package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database made before the pull request columns gets them, and keeps its
// threads.
func TestAnOlderDatabaseGetsTheNewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (
		repo TEXT NOT NULL, number INTEGER NOT NULL, state TEXT NOT NULL,
		session TEXT NOT NULL DEFAULT '', pr INTEGER NOT NULL DEFAULT 0, nudged INTEGER NOT NULL DEFAULT 0,
		batch TEXT NOT NULL DEFAULT '[]', outcome TEXT NOT NULL DEFAULT '', started INTEGER NOT NULL DEFAULT 0,
		replied INTEGER NOT NULL DEFAULT 0, described TEXT NOT NULL DEFAULT '', turn_head TEXT NOT NULL DEFAULT '',
		tested_tree TEXT NOT NULL DEFAULT '', tested_pass INTEGER NOT NULL DEFAULT 0, agent TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL DEFAULT '', PRIMARY KEY (repo, number));
		INSERT INTO threads (repo, number, state, pr, described) VALUES ('o/r', 4, 'idle', 7, 'abc');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ { // and opening it again changes nothing
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		th, err := s.Thread("o/r", 4)
		if err != nil || th == nil || th.PR != 7 {
			t.Fatalf("thread: %+v, %v", th, err)
		}
		th.PRTitle, th.PRBody, th.PRSource = "fix: a thing", "Fixes it.", "sum"
		if err := s.Save(th); err != nil {
			t.Fatal(err)
		}
		if th, _ = s.Thread("o/r", 4); th.PRTitle != "fix: a thing" || th.PRBody != "Fixes it." || th.PRSource != "sum" {
			t.Errorf("saved: %+v", th)
		}
		s.Close()
	}
}
