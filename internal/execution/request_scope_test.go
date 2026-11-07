package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbsteel/session-insight/internal/model"
)

func TestNewRequestCannotInheritUnrelatedStagesOrBacklog(t *testing.T) {
	events := []model.RenderEvent{
		event("old-goal", "UserPrompt", "修正截图脚本", 0),
		{EventID: "old-tool", Type: "ToolInvocation", ToolCallID: "old-call", ToolName: "bash"},
		event("old-result", "TextChunk", "截图脚本已修正", 0),
	}
	s := initialState(events)
	s.Board.Enabled, s.Board.ProviderID = true, 7
	s.Board.Goal = "修正截图脚本"
	s.Board.Current = Current{Action: "截图脚本已修正", SourceRefs: []Ref{Reference(events[2])}}
	s.Board.Stages = []Stage{{ID: "screenshot", Title: "截图脚本修正", Status: "completed", SourceRefs: []Ref{Reference(events[2])}}}
	s.Board.Updates = []Update{{ID: "old", Text: "截图脚本已修正", SourceRefs: []Ref{Reference(events[2])}}}
	// A previous request still has unsummarized data when the user asks a new question.
	s.Cursor, s.Digest = 1, PrefixDigests(events)[1]
	events = append(events, event("new-goal", "UserPrompt", "这个仓库是你创建的吗？", 1), event("new-response", "TextChunk", "我正在核查仓库归属", 1))
	if !Reconcile(&s, events, PrefixDigests(events), time.Now()) || s.Cursor != 3 {
		t.Fatal("new request did not replace the obsolete backlog", s.Cursor)
	}
	if !s.Board.Enabled || s.Board.ProviderID != 7 || len(s.Board.Stages) != 0 || len(s.Board.Updates) != 0 || s.Board.Current.Action != "" {
		t.Fatal("new request inherited old interpretations or lost monitor settings", s.Board)
	}
	w := BuildWindow(events, s)
	raw, _ := json.Marshal(w)
	if w.Goal != "这个仓库是你创建的吗？" || len(w.Events) != 2 || len(w.Context) != 0 || strings.Contains(string(raw), "截图") {
		t.Fatal("unrelated work entered the new request window", string(raw))
	}
	if _, err := ValidateOutput(outputFor(Reference(events[2])), w); err == nil {
		t.Fatal("a previous request's stage citation was accepted")
	}
}

func TestImageOnlyRequestDoesNotRetainPreviousGoal(t *testing.T) {
	events := []model.RenderEvent{event("old", "UserPrompt", "Check repository ownership", 0)}
	s := initialState(events)
	s.Board.Goal = "Check repository ownership"
	events = append(events, event("image", "UserPrompt", "", 1), event("response", "TextChunk", "I will adjust the board according to the screenshot", 1))
	Reconcile(&s, events, PrefixDigests(events), time.Now())
	w := BuildWindow(events, s)
	if s.TaskID != "image" || s.Cursor != 1 || s.Board.Goal != "" || w.Goal != "" || w.Events[0].Ref.EventID != "image" {
		t.Fatal("image-only request was mistaken for the previous task", s, w)
	}
}

func TestOnlyCurrentRequestStagesPersistAcrossWindows(t *testing.T) {
	events := []model.RenderEvent{event("old", "UserPrompt", "old task", 0), event("new", "UserPrompt", "current task", 1), event("step", "TextChunk", "inspecting source", 1), event("delta", "TextChunk", "found a relevant file", 1)}
	s := initialState(events)
	s.Cursor, s.Digest = 3, PrefixDigests(events)[3]
	s.Board.Current = Current{Action: "old task", SourceRefs: []Ref{Reference(events[0])}}
	s.Board.Stages = []Stage{
		{ID: "old", Title: "Unrelated old task", SourceRefs: []Ref{Reference(events[0])}},
		{ID: "current", Title: "Inspect source", SourceRefs: []Ref{Reference(events[2])}},
	}
	w := BuildWindow(events, s)
	if len(w.Stages) != 1 || w.Stages[0].ID != "current" || w.Previous.Action != "" {
		t.Fatal("stage carry-over was not scoped to the latest request", w)
	}
	if _, err := ValidateOutput(outputFor(Reference(events[2])), w); err != nil {
		t.Fatal("valid stage from this request was lost", err)
	}
}

func TestPausedReadClearsOldRequestWithoutCallingModel(t *testing.T) {
	key := Key{"pi", "paused"}
	src := &mutableSource{}
	old := event("old", "UserPrompt", "old task", 0)
	src.set(old)
	s := initialState([]model.RenderEvent{old})
	s.Board.Status = "paused"
	s.Board.Goal = "old task"
	s.Board.Stages = []Stage{{ID: "old", Title: "Old stage", SourceRefs: []Ref{Reference(old)}}}
	store := newMemoryStore()
	_ = store.SaveExecution(key, s)
	var calls atomic.Int32
	m := NewManager(store, src.read, func(context.Context, int64, string, string) (string, error) {
		calls.Add(1)
		return "", nil
	}, testOpts())
	defer m.Close()
	if b, err := m.Get(key); err != nil || len(b.Stages) != 1 {
		t.Fatal("paused interpretation was not retained before the request changed", b, err)
	}
	src.set(old, event("new", "UserPrompt", "new task", 1))
	b, err := m.Get(key)
	if err != nil || b.Enabled || b.Status != "paused" || len(b.Stages) != 0 || b.Goal != "" || calls.Load() != 0 {
		t.Fatal("paused board reused the old request or spent model tokens", b, err, calls.Load())
	}
}

func TestLegacyPausedBoardDoesNotExposeMixedTaskStages(t *testing.T) {
	key := Key{"pi", "legacy"}
	src := &mutableSource{}
	src.set(event("goal", "UserPrompt", "current request", 1))
	store := newMemoryStore()
	legacy := State{Board: Empty(), Locale: "en", Initialized: true}
	legacy.Board.Status = "paused"
	legacy.Board.Stages = []Stage{{ID: "unrelated", Title: "Unrelated old task"}}
	_ = store.SaveExecution(key, legacy)
	m := NewManager(store, src.read, func(context.Context, int64, string, string) (string, error) {
		t.Error("reading a paused legacy board must not call the model")
		return "", nil
	}, testOpts())
	defer m.Close()
	b, err := m.Get(key)
	if err != nil || len(b.Stages) != 0 || b.Enabled || b.Status != "paused" {
		t.Fatal("legacy mixed-request cards remained visible", b, err)
	}
}

func TestNewRequestCancelsOldInterpretationInFlight(t *testing.T) {
	key := Key{"pi", "request-change"}
	src := &mutableSource{}
	old := event("old", "UserPrompt", "old task", 0)
	src.set(old)
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	m := NewManager(newMemoryStore(), src.read, func(ctx context.Context, _ int64, _, prompt string) (string, error) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return outputFor(Reference(old)), nil
		}
		return outputFor(promptRef(prompt)), nil
	}, testOpts())
	defer m.Close()
	_, _ = m.Configure(key, "en", true, 7)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old request generation did not start")
	}
	src.set(old, event("new", "UserPrompt", "new task", 1))
	until(t, func() bool {
		b := board(m, key)
		return b.UpdatedAt != nil && len(b.Current.SourceRefs) == 1 && b.Current.SourceRefs[0].EventID == "new"
	})
	if calls.Load() != 2 {
		t.Fatal("new request did not replace the canceled interpretation", calls.Load())
	}
}
