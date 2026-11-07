package execution

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bbsteel/session-insight/internal/model"
)

type flakySource struct {
	mutableSource
	mu   sync.Mutex
	fail bool
}

func (s *flakySource) read(ctx context.Context, key Key) ([]model.RenderEvent, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return nil, errors.New("session_changing")
	}
	return s.mutableSource.read(ctx, key)
}
func (s *flakySource) setFailure(fail bool) {
	s.mu.Lock()
	s.fail = fail
	s.mu.Unlock()
}

func TestTransientSourceFailureClearsAfterRecovery(t *testing.T) {
	keysrc := Key{"pi", "flaky"}
	src := &flakySource{}
	src.set(event("goal", "UserPrompt", "inspect the source", 0))
	src.setFailure(true)
	var calls atomic.Int32
	m := NewManager(newMemoryStore(), src.read, func(_ context.Context, _ int64, _, prompt string) (string, error) {
		calls.Add(1)
		return outputFor(promptRef(prompt)), nil
	}, testOpts())
	defer m.Close()
	_, _ = m.Configure(keysrc, "en", true, 7)
	until(t, func() bool { return board(m, keysrc).ErrorCode == "source_unavailable" })
	src.setFailure(false)
	until(t, func() bool {
		b := board(m, keysrc)
		return b.ErrorCode == "" && b.Status != "error"
	})
	until(t, func() bool { return board(m, keysrc).UpdatedAt != nil })
	if calls.Load() == 0 {
		t.Fatal("recovered source was not interpreted")
	}
}
