package execution

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/bbsteel/session-insight/internal/model"
)

type Record struct {
	Key   Key
	State State
}
type Store interface {
	LoadExecution(Key) (State, bool, error)
	SaveExecution(Key, State) error
	EnabledExecutions() ([]Record, error)
}
type Source func(context.Context, Key) ([]model.RenderEvent, error)
type Generator func(context.Context, int64, string, string) (string, error)
type Options struct {
	Poll      time.Duration
	Interval  time.Duration
	Cooldown  time.Duration
	Timeout   time.Duration
	RetryBase time.Duration
}
type entry struct {
	state         State
	observeMu     sync.Mutex
	wake          chan struct{}
	started       bool
	busy          bool
	epoch         uint64
	cancel        context.CancelFunc
	pendingSince  time.Time
	lastAttempt   time.Time
	observedCount int
	flightEnd     int
	flightDigest  string
}

// Manager owns exactly one worker and at most one generation per agent/session
// in this server. Locale and provider changes fence/cancel obsolete responses.
// Observe continues on its own ticker while a generation is in flight.
type Manager struct {
	mu       sync.Mutex
	entries  map[Key]*entry
	store    Store
	source   Source
	generate Generator
	opts     Options
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	closed   bool
}

func NewManager(store Store, source Source, generate Generator, opts Options) *Manager {
	if opts.Poll <= 0 {
		opts.Poll = time.Second
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = 3 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = 5 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{entries: map[Key]*entry{}, store: store, source: source, generate: generate, opts: opts, ctx: ctx, cancel: cancel}
}
func (m *Manager) Restore() error {
	records, err := m.store.EnabledExecutions()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		if _, ok := m.entries[r.Key]; ok {
			continue
		}
		e := m.newEntry(r.State)
		m.entries[r.Key] = e
		m.startLocked(r.Key, e)
	}
	return nil
}
func (m *Manager) newEntry(s State) *entry {
	if s.ScopeVersion != RequestScopeVersion {
		// Old saved cards could describe unrelated previous requests. Clear
		// those interpretations even for a paused board without spending.
		enabled, provider, activity, status := s.Board.Enabled, s.Board.ProviderID, s.Board.Activity, s.Board.Status
		s.Board = Empty()
		s.Board.Enabled, s.Board.ProviderID, s.Board.Activity, s.Board.Status = enabled, provider, activity, status
		s.Cursor, s.Digest, s.TaskID = 0, "", ""
		s.Initialized = false
		s.ScopeVersion = RequestScopeVersion
		s.RetryAt, s.Failures = time.Time{}, 0
	}
	normalize(&s.Board)
	if s.Board.Enabled {
		s.Board.Status = "watching"
	} else if s.Board.Status != "idle" {
		s.Board.Status = "paused"
	}
	return &entry{state: s, wake: make(chan struct{}, 1)}
}
func (m *Manager) getLocked(key Key) (*entry, error) {
	if m.closed {
		return nil, errors.New("closed")
	}
	if e := m.entries[key]; e != nil {
		return e, nil
	}
	s, ok, err := m.store.LoadExecution(key)
	if err != nil {
		return nil, err
	}
	if !ok {
		s = State{Board: Empty(), Locale: "en"}
	}
	e := m.newEntry(s)
	m.entries[key] = e
	if s.Board.Enabled {
		m.startLocked(key, e)
	}
	return e, nil
}
func cloneBoard(b Board) Board {
	raw, _ := json.Marshal(b)
	var cp Board
	_ = json.Unmarshal(raw, &cp)
	normalize(&cp)
	return cp
}
func (m *Manager) Get(key Key) (Board, error) {
	m.mu.Lock()
	e, err := m.getLocked(key)
	if err != nil {
		m.mu.Unlock()
		return Empty(), err
	}
	paused := !e.state.Board.Enabled
	if !paused {
		board := cloneBoard(e.state.Board)
		m.mu.Unlock()
		return board, nil
	}
	m.mu.Unlock()
	// Pausing stops model requests, not source observation. A new user turn
	// must invalidate old cards even when explanations remain paused.
	_ = m.observe(key, e, false)
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneBoard(e.state.Board), nil
}
func (m *Manager) Configure(key Key, locale string, enabled bool, provider int64) (Board, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.getLocked(key)
	if err != nil {
		return Empty(), err
	}
	next := e.state
	resuming := enabled && !next.Board.Enabled
	changed := next.Locale != locale || (provider != 0 && provider != next.Board.ProviderID)
	next.Locale = locale
	next.Board.Enabled = enabled
	if provider != 0 {
		next.Board.ProviderID = provider
	}
	if changed {
		// An explicit language/model change authorizes a fresh bounded
		// interpretation even if the source has not grown. Clear old-language
		// interpretations/history; the next observation bootstraps recent tail.
		activity := next.Board.Activity
		next.Board = Empty()
		next.Board.Activity = activity
		next.Board.Enabled = enabled
		if provider != 0 {
			next.Board.ProviderID = provider
		} else {
			next.Board.ProviderID = e.state.Board.ProviderID
		}
		next.Initialized = false
		next.Cursor = 0
		next.Digest = ""
		next.RetryAt = time.Time{}
		next.Failures = 0
	}
	if resuming {
		// A deliberate pause/start retries pending data promptly. Repeated
		// enabled POSTs from another tab still respect automatic backoff.
		next.RetryAt = time.Time{}
		next.Failures = 0
	}
	if enabled && next.Board.ProviderID == 0 {
		return Empty(), errors.New("no_provider")
	}
	if enabled {
		next.Board.Status = "watching"
	} else {
		next.Board.Status = "paused"
	}
	next.Board.ErrorCode = ""
	// Save enable/pause before changing process state; an unsuccessful write
	// cannot accidentally authorize background spend after an HTTP failure.
	if err = m.store.SaveExecution(key, next); err != nil {
		return Empty(), err
	}
	if changed || !enabled {
		e.epoch++
		if e.cancel != nil {
			e.cancel()
		}
	}
	e.state = next
	if changed {
		e.pendingSince = time.Time{}
	}
	if enabled {
		m.startLocked(key, e)
		m.signal(e)
	}
	return cloneBoard(e.state.Board), nil
}
func (m *Manager) Refresh(key Key) (Board, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.getLocked(key)
	if err != nil {
		return Empty(), err
	}
	if e.state.Board.Enabled {
		e.pendingSince = time.Now().Add(-m.opts.Interval)
		m.signal(e)
	}
	return cloneBoard(e.state.Board), nil
}
func (m *Manager) signal(e *entry) {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) startLocked(key Key, e *entry) {
	if e.started || m.closed {
		return
	}
	e.started = true
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		tick := time.NewTicker(m.opts.Poll)
		defer tick.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-tick.C:
			case <-e.wake:
			}
			m.observe(key, e, true)
		}
	}()
}

// observe serializes each reader access and state installation, avoiding an old
// snapshot racing a newer one. Source adapters are not required to be live.
func (m *Manager) observe(key Key, e *entry, schedule bool) error {
	e.observeMu.Lock()
	defer e.observeMu.Unlock()
	m.mu.Lock()
	canObserve := !m.closed && (e.state.Board.Enabled || !schedule)
	m.mu.Unlock()
	if !canObserve {
		return nil
	}
	events, err := m.source(m.ctx, key)
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || (!e.state.Board.Enabled && schedule) {
		return nil
	}
	previousStatus := e.state.Board.Status
	if err != nil {
		m.failLocked(key, e, "source_unavailable", now)
		return err
	}
	activity := ObserveActivity(events)
	events = CursorEvents(events)
	digests := PrefixDigests(events)
	reset := Reconcile(&e.state, events, digests, now)
	e.state.Board.Activity = activity
	// An edit in data currently being summarized is also a rewrite, even if
	// that data lies beyond the last successful cursor.
	staleFlight := e.busy && (e.flightEnd > len(events) || e.flightDigest != digests[min(e.flightEnd, len(events))])
	if reset || staleFlight {
		e.epoch++
		if e.cancel != nil {
			e.cancel()
		}
		e.pendingSince = now
	}
	e.observedCount = len(events)
	if e.state.Board.PendingEvents > 0 && e.pendingSince.IsZero() {
		e.pendingSince = now
	}
	if e.state.Board.PendingEvents == 0 {
		e.pendingSince = time.Time{}
	}
	w := BuildWindow(events, e.state)
	e.state.Board.SourceLimited = w.SourceLimited
	if !e.state.Board.Enabled {
		e.state.Board.Status = "idle"
		if previousStatus != "idle" {
			e.state.Board.Status = "paused"
		}
	} else if e.state.Board.Status != "error" {
		if e.busy {
			e.state.Board.Status = "summarizing"
		} else {
			e.state.Board.Status = "watching"
		}
	}
	if err := m.store.SaveExecution(key, e.state); err != nil {
		e.state.Board.Status = "error"
		e.state.Board.ErrorCode = "persistence_failed"
		return err
	}
	if schedule && !e.busy && Due(now, e.pendingSince, e.lastAttempt, e.state.RetryAt, e.state.Board.PendingEvents, w.Urgent, w.Runes, m.opts.Interval, m.opts.Cooldown) {
		m.launchLocked(key, e, w, digests[w.End], now)
	}
	return nil
}
func (m *Manager) failLocked(key Key, e *entry, code string, now time.Time) {
	e.state.Board.Status = "error"
	e.state.Board.ErrorCode = code
	e.state.Failures++
	delay := m.opts.RetryBase
	for i := 1; i < e.state.Failures && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	e.state.RetryAt = now.Add(delay)
	if err := m.store.SaveExecution(key, e.state); err != nil {
		e.state.Board.ErrorCode = "persistence_failed"
	}
}
func (m *Manager) launchLocked(key Key, e *entry, w Window, digest string, now time.Time) {
	body, err := json.Marshal(w)
	if err != nil {
		m.failLocked(key, e, "invalid_input", now)
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.opts.Timeout)
	e.cancel = cancel
	e.busy = true
	e.lastAttempt = now
	e.flightEnd = w.End
	e.flightDigest = digest
	e.state.Board.Status = "summarizing"
	e.state.Board.ErrorCode = ""
	epoch, provider, locale := e.epoch, e.state.Board.ProviderID, e.state.Locale
	go func() {
		defer cancel()
		raw, genErr := m.generate(ctx, provider, SystemInstruction(locale), string(body))
		if ctx.Err() != nil {
			genErr = ctx.Err()
		}
		var out Output
		if genErr == nil {
			out, genErr = ValidateOutput(raw, w)
		}
		// Verify source again immediately before committing a successful AI
		// update. Prefix hashes fence rewrites during a slow model request.
		if genErr == nil {
			genErr = m.observe(key, e, false)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		e.busy = false
		e.cancel = nil
		if m.closed || !e.state.Board.Enabled || epoch != e.epoch {
			m.signal(e)
			return
		}
		if genErr != nil {
			code := "generation_failed"
			if genErr.Error() == "no_provider" {
				code = "no_provider"
			}
			if errors.Is(genErr, context.DeadlineExceeded) {
				code = "generation_timeout"
			}
			if genErr.Error() == "invalid_citations" || genErr.Error() == "invalid_output" || genErr.Error() == "output_too_large" {
				code = genErr.Error()
			}
			m.failLocked(key, e, code, time.Now())
			return
		}
		next := e.state
		ApplyOutput(&next.Board, out, time.Now())
		next.Cursor = w.End
		next.Digest = digest
		next.Failures = 0
		next.RetryAt = time.Time{}
		next.Board.PendingEvents = max(0, e.observedCount-w.End)
		next.Board.Status = "watching"
		if err := m.store.SaveExecution(key, next); err != nil {
			m.failLocked(key, e, "persistence_failed", time.Now())
			return
		}
		e.state = next
		if next.Board.PendingEvents == 0 {
			e.pendingSince = time.Time{}
		}
		m.signal(e)
	}()
}

// Close cancels monitors and in-flight requests. Persisted enabled flags are
// intentionally retained so startup Restore can resume verified cursors.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	for _, e := range m.entries {
		if e.cancel != nil {
			e.cancel()
		}
	}
	m.mu.Unlock()
	m.workers.Wait()
}
