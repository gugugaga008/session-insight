package execution

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbsteel/session-insight/internal/model"
)

func TestManagerExplicitResumeClearsBackoffPreservesCursor(t *testing.T) {
	key := Key{Agent: "pi", Session: "resume-error"}
	events := []model.RenderEvent{event("old", "UserPrompt", "goal", 0), event("pending", "TextChunk", "new progress", 1)}
	s := initialState(events)
	s.Cursor = 1
	s.Digest = PrefixDigests(events)[1]
	s.Board.ProviderID = 1
	s.Board.Status = "paused"
	s.Failures = 7
	s.RetryAt = time.Now().Add(5 * time.Minute)
	store := newMemoryStore()
	if err := store.SaveExecution(key, s); err != nil {
		t.Fatal(err)
	}
	source := &mutableSource{}
	source.set(events...)
	var calls atomic.Int32
	m := NewManager(store, source.read, func(_ context.Context, _ int64, _, prompt string) (string, error) {
		calls.Add(1)
		ref := promptRef(prompt)
		if ref.EventID != "pending" {
			t.Errorf("resume skipped/replayed cursor: %+v", ref)
		}
		return outputFor(ref), nil
	}, testOpts())
	defer m.Close()
	if _, err := m.Configure(key, "en", true, 1); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.LoadExecution(key)
	if err != nil || !got.RetryAt.IsZero() || got.Failures != 0 || got.Cursor != 1 || got.Digest != s.Digest {
		t.Fatalf("bad explicit resume %+v err=%v", got, err)
	}
	until(t, func() bool {
		return calls.Load() == 1 && board(m, key).PendingEvents == 0 && board(m, key).UpdatedAt != nil
	})
}
