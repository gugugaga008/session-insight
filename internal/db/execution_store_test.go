package db

import (
	"testing"
	"time"

	"github.com/bbsteel/session-insight/internal/execution"
)

func TestExecutionStoreUpgradePersistenceAndExactKeys(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Model an existing v44 DB: upgrade creates the table even after a partial
	// previous migration, without touching session/index/ai-generation rows.
	if _, err = database.conn.Exec(`DROP TABLE execution_boards; DELETE FROM schema_migrations WHERE version=45`); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	k := execution.Key{Agent: "pi", Session: "same-id"}
	s := execution.State{Board: execution.Empty(), Locale: "zh-CN", Cursor: 12, Digest: "verified-prefix", Initialized: true, Failures: 2, RetryAt: time.Now().UTC().Add(time.Minute)}
	s.Board.Enabled = true
	s.Board.ProviderID = 8
	s.Board.Goal = "saved goal"
	if err = database.SaveExecution(k, s); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := database.LoadExecution(execution.Key{Agent: "claude", Session: k.Session}); err != nil || ok {
		t.Fatal("cross-agent lookup", err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	got, ok, err := database.LoadExecution(k)
	if err != nil || !ok || got.Cursor != s.Cursor || got.Digest != s.Digest || got.Locale != s.Locale || got.Board.Goal != s.Board.Goal || !got.RetryAt.Equal(s.RetryAt) {
		t.Fatalf("persistence mismatch %+v %v", got, err)
	}
	records, err := database.EnabledExecutions()
	if err != nil || len(records) != 1 || records[0].Key != k {
		t.Fatal(records, err)
	}
	got.Board.Enabled = false
	if err = database.SaveExecution(k, got); err != nil {
		t.Fatal(err)
	}
	records, err = database.EnabledExecutions()
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
	var aiCount int
	if err = database.conn.QueryRow(`SELECT COUNT(*) FROM ai_generations`).Scan(&aiCount); err != nil || aiCount != 0 {
		t.Fatal("execution used generation history", err)
	}
}
