package execution

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbsteel/session-insight/internal/model"
)

type memoryStore struct {
	mu     sync.Mutex
	states map[Key]State
	fail   bool
}

func newMemoryStore() *memoryStore { return &memoryStore{states: map[Key]State{}} }
func copyState(s State) State {
	raw, _ := json.Marshal(s)
	var cp State
	_ = json.Unmarshal(raw, &cp)
	return cp
}
func (s *memoryStore) LoadExecution(k Key) (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.states[k]
	return copyState(v), ok, nil
}
func (s *memoryStore) SaveExecution(k Key, v State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk failed")
	}
	s.states[k] = copyState(v)
	return nil
}
func (s *memoryStore) EnabledExecutions() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Record{}
	for k, v := range s.states {
		if v.Board.Enabled {
			out = append(out, Record{k, copyState(v)})
		}
	}
	return out, nil
}

type mutableSource struct {
	mu     sync.Mutex
	events []model.RenderEvent
}

func (s *mutableSource) read(context.Context, Key) ([]model.RenderEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RenderEvent{}, s.events...), nil
}
func (s *mutableSource) set(events ...model.RenderEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append([]model.RenderEvent{}, events...)
}
func testOpts() Options {
	return Options{Poll: 3 * time.Millisecond, Interval: 12 * time.Millisecond, Cooldown: 8 * time.Millisecond, Timeout: 300 * time.Millisecond, RetryBase: 30 * time.Millisecond}
}
func until(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func board(m *Manager, k Key) Board { b, _ := m.Get(k); return b }
func promptRef(prompt string) Ref {
	var w Window
	_ = json.Unmarshal([]byte(prompt), &w)
	return w.Events[len(w.Events)-1].Ref
}

func TestManagerNoDuplicateCallsIdleAndAccumulatesDuringGeneration(t *testing.T) {
	k := Key{"pi", "same"}
	src := &mutableSource{}
	src.set(event("1", "UserPrompt", "goal", 0))
	store := newMemoryStore()
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	m := NewManager(store, src.read, func(ctx context.Context, _ int64, _, prompt string) (string, error) {
		n := calls.Add(1)
		started <- struct{}{}
		if n == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return outputFor(promptRef(prompt)), nil
	}, testOpts())
	defer m.Close()
	// Merely opening an unenabled board cannot authorize generation.
	for i := 0; i < 5; i++ {
		_, _ = m.Get(k)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("GET spent")
	}
	for i := 0; i < 10; i++ {
		if _, err := m.Configure(k, "en", true, 1); err != nil {
			t.Fatal(err)
		}
	}
	until(t, func() bool { return calls.Load() == 1 })
	src.set(event("1", "UserPrompt", "goal", 0), event("2", "ToolInvocation", "", 0))
	until(t, func() bool { return board(m, k).PendingEvents == 2 && board(m, k).Activity.Kind == "tool" })
	time.Sleep(25 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("overlapping generation")
	}
	close(release)
	until(t, func() bool {
		return calls.Load() == 2 && board(m, k).PendingEvents == 0 && board(m, k).UpdatedAt != nil
	})
	time.Sleep(35 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatal("idle repeated generation")
	}
	state, _, _ := store.LoadExecution(k)
	if state.Cursor != 2 {
		t.Fatal("cursor not persisted")
	}
}
func TestManagerFailureBackoffRetriesSameDataAndPauseCancellation(t *testing.T) {
	k := Key{"pi", "retry"}
	src := &mutableSource{}
	src.set(event("1", "UserPrompt", "goal", 0))
	store := newMemoryStore()
	var calls atomic.Int32
	cancelled := make(chan struct{}, 1)
	opts := testOpts()
	opts.RetryBase = 200 * time.Millisecond
	m := NewManager(store, src.read, func(ctx context.Context, _ int64, _, prompt string) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "", errors.New("HTTP 429 provider secret should not escape")
		}
		if n == 2 {
			return outputFor(promptRef(prompt)), nil
		}
		<-ctx.Done()
		cancelled <- struct{}{}
		return "", ctx.Err()
	}, opts)
	defer m.Close()
	_, _ = m.Configure(k, "en", true, 1)
	until(t, func() bool { return board(m, k).Status == "error" })
	state, _, _ := store.LoadExecution(k)
	if state.Cursor != 0 || state.Failures != 1 || state.Board.ErrorCode != "generation_failed" {
		t.Fatal(state)
	}
	// Refresh cannot evade backoff, nor can a repeated tab's enable request.
	_, _ = m.Refresh(k)
	_, _ = m.Configure(k, "en", true, 1)
	time.Sleep(12 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("retry storm")
	}
	until(t, func() bool { return board(m, k).PendingEvents == 0 && calls.Load() == 2 })
	src.set(event("1", "UserPrompt", "goal", 0), event("2", "TextChunk", "new progress", 0))
	until(t, func() bool { return calls.Load() == 3 })
	_, err := m.Configure(k, "en", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		select {
		case <-cancelled:
			return true
		default:
			return false
		}
	})
	if b := board(m, k); b.Enabled || b.Status != "paused" {
		t.Fatal(b)
	}
	state, _, _ = store.LoadExecution(k)
	if state.Cursor != 1 || state.Board.Enabled {
		t.Fatal("pause advanced cursor")
	}
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 3 {
		t.Fatal("paused spend")
	}
}
func TestManagerRewriteDuringRequestRejectsStaleOutput(t *testing.T) {
	k := Key{"pi", "rewrite"}
	src := &mutableSource{}
	src.set(event("1", "UserPrompt", "first", 0))
	store := newMemoryStore()
	var calls atomic.Int32
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	m := NewManager(store, src.read, func(ctx context.Context, _ int64, _, prompt string) (string, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			cancelled <- struct{}{}
			<-release
			return outputFor(promptRef(prompt)), nil
		}
		return outputFor(promptRef(prompt)), nil
	}, testOpts())
	defer m.Close()
	_, _ = m.Configure(k, "en", true, 1)
	until(t, func() bool { return calls.Load() == 1 })
	src.set(event("1", "UserPrompt", "replacement same event count", 0))
	until(t, func() bool {
		select {
		case <-cancelled:
			return true
		default:
			return false
		}
	})
	if board(m, k).UpdatedAt != nil {
		t.Fatal("stale result applied")
	}
	close(release)
	until(t, func() bool { return calls.Load() == 2 && board(m, k).PendingEvents == 0 })
	state, _, _ := store.LoadExecution(k)
	events, _ := src.read(context.Background(), k)
	if state.Digest != PrefixDigests(events)[1] {
		t.Fatal("rewrite cursor incorrect")
	}
}
func TestManagerPersistenceRestoreLocaleAndProviderChange(t *testing.T) {
	k := Key{"pi", "restore"}
	src := &mutableSource{}
	src.set(event("1", "UserPrompt", "goal", 0))
	store := newMemoryStore()
	var calls atomic.Int32
	var langMu sync.Mutex
	lastSystem := ""
	var provider atomic.Int64
	generate := func(_ context.Context, p int64, system, prompt string) (string, error) {
		calls.Add(1)
		langMu.Lock()
		lastSystem = system
		langMu.Unlock()
		provider.Store(p)
		return outputFor(promptRef(prompt)), nil
	}
	m := NewManager(store, src.read, generate, testOpts())
	_, _ = m.Configure(k, "en", true, 1)
	until(t, func() bool { return board(m, k).PendingEvents == 0 && board(m, k).UpdatedAt != nil })
	m.Close()
	m = NewManager(store, src.read, generate, testOpts())
	defer m.Close()
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("restart reprocessed unchanged data")
	}
	_, _ = m.Configure(k, "zh-CN", true, 1)
	until(t, func() bool { return calls.Load() == 2 && board(m, k).PendingEvents == 0 })
	langMu.Lock()
	expected := SystemInstruction("zh-CN")
	actual := lastSystem
	langMu.Unlock()
	if actual != expected {
		t.Fatal("locale not applied")
	}
	_, _ = m.Configure(k, "zh-CN", true, 2)
	until(t, func() bool { return calls.Load() == 3 && board(m, k).PendingEvents == 0 })
	if provider.Load() != 2 {
		t.Fatal("provider not applied")
	}
	// Exact key separation, even when two adapters use the same session ID.
	if b := board(m, Key{"claude", k.Session}); b.Enabled || b.UpdatedAt != nil {
		t.Fatal("cross-agent state leak")
	}
}
func TestManagerTimeoutAndPersistenceFailureDoNotAdvance(t *testing.T) {
	k := Key{"pi", "timeout"}
	src := &mutableSource{}
	src.set(event("1", "UserPrompt", "goal", 0))
	store := newMemoryStore()
	opts := testOpts()
	opts.Timeout = 15 * time.Millisecond
	m := NewManager(store, src.read, func(ctx context.Context, _ int64, _, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() }, opts)
	defer m.Close()
	_, _ = m.Configure(k, "en", true, 1)
	until(t, func() bool { return board(m, k).ErrorCode == "generation_timeout" })
	s, _, _ := store.LoadExecution(k)
	if s.Cursor != 0 {
		t.Fatal("timeout skipped events")
	}
	_, _ = m.Configure(k, "en", false, 0)
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	if _, err := m.Configure(k, "en", true, 1); err == nil {
		t.Fatal("enable succeeded without persistence")
	}
	if board(m, k).Enabled {
		t.Fatal("failed persistence authorized generation")
	}
}
