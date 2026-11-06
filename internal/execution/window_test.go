package execution

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bbsteel/session-insight/internal/model"
)

func event(id, kind, text string, turn int) model.RenderEvent {
	return model.RenderEvent{EventID: id, Type: kind, Text: text, TurnIndex: turn}
}
func initialState(events []model.RenderEvent) State {
	s := State{Board: Empty(), Locale: "en"}
	Reconcile(&s, events, PrefixDigests(events), time.Now())
	return s
}
func outputFor(ref Ref) string {
	b, _ := json.Marshal(Output{Goal: "Implement the requested change", Current: Current{Action: "Inspecting source", Purpose: "Understand behavior", SourceRefs: []Ref{ref}}, Stages: []Stage{{ID: "inspect", Title: "Inspect", Status: "active", SourceRefs: []Ref{ref}}}, Updates: []Update{{Text: "Inspected source", SourceRefs: []Ref{ref}}}})
	return string(b)
}
func TestWindowBootstrapDeltaBoundsAndToolContext(t *testing.T) {
	events := []model.RenderEvent{event("goal", "UserPrompt", "old goal", 0), {EventID: "open", Type: "ToolInvocation", TurnIndex: 0, ToolCallID: "call", ToolName: "bash", ToolInput: map[string]any{"command": "long running"}}}
	for i := 0; i < 350; i++ {
		events = append(events, event(fmt.Sprint(i), "TextChunk", strings.Repeat("字", 3000), i+1))
	}
	events = append(events, event("goal2", "UserPrompt", "new goal", 352))
	s := initialState(events)
	if s.Cursor != len(events)-TailEvents || !s.Board.SourceLimited {
		t.Fatalf("bootstrap %+v", s)
	}
	w := BuildWindow(events, s)
	if w.Goal != "new goal" || len(w.Events) > MaxEvents || w.Runes > MaxWindowRunes || len(w.Context) != 1 || w.Context[0].Ref.ToolCallID != "call" {
		t.Fatalf("bad window %+v", w)
	}
	// Live backlogs consume in source order, not a sliding tail that skips data.
	s.Cursor = w.End
	s.Digest = PrefixDigests(events)[s.Cursor]
	oldEnd := s.Cursor
	for i := 0; i < 200; i++ {
		events = append(events, event(fmt.Sprintf("delta-%d", i), "TextChunk", "delta", 400+i))
	}
	Reconcile(&s, events, PrefixDigests(events), time.Now())
	w = BuildWindow(events, s)
	if w.End <= oldEnd || w.End > oldEnd+MaxEvents || w.Events[0].Ref != Reference(events[oldEnd]) {
		t.Fatal("delta skipped or exceeded count bound")
	}
}
func TestReconcileSameLengthRewriteAndSyntheticActivity(t *testing.T) {
	events := []model.RenderEvent{event("a", "UserPrompt", "first", 0), event("b", "TextChunk", "second", 0)}
	s := initialState(events)
	s.Cursor = 2
	s.Digest = PrefixDigests(events)[2]
	s.Board.Goal = "old interpretation"
	events[0].Text = "rewritten"
	if !Reconcile(&s, events, PrefixDigests(events), time.Now()) || s.Cursor != 0 || s.Board.Goal != "" {
		t.Fatal("same length rewrite did not reset")
	}
	base := CursorEvents(events)
	events = append(events, model.RenderEvent{Type: "AgentSpecific", Subtype: "in_progress", Timestamp: time.Now()})
	if PrefixDigests(base)[len(base)] != PrefixDigests(CursorEvents(events))[len(base)] {
		t.Fatal("synthetic activity changed cursor")
	}
	if ObserveActivity(events).Kind != "responding" {
		t.Fatal("lost responding status")
	}
}
func TestToolPairingAcrossWindowsAndNoCompletionInference(t *testing.T) {
	events := []model.RenderEvent{{EventID: "inv", Type: "ToolInvocation", ToolCallID: "call", TurnIndex: 3, ToolName: "read", Timestamp: time.Now()}}
	if a := ObserveActivity(events); a.Kind != "tool" || a.SourceRefs[0].ToolCallID != "call" {
		t.Fatal(a)
	}
	events = append(events, model.RenderEvent{EventID: "result", Type: "ToolResult", ParentEventID: "inv", TurnIndex: 3})
	if a := ObserveActivity(events); a.Kind != "idle" {
		t.Fatal(a)
	}
	if Empty().Status != "idle" {
		t.Fatal("unexpected initial status")
	}
	// Results that only expose a parent event still pair with an invocation
	// whose preferred identity is tool_call_id.
	s := initialState(events)
	s.Cursor = 1
	w := BuildWindow(events, s)
	if len(w.Context) != 1 || w.Context[0].Ref.EventID != "inv" {
		t.Fatal("lost invocation context", w)
	}
	usage := []model.RenderEvent{{EventID: "usage", Type: "AgentSpecific", TokenUsage: &model.RenderTokenUsage{OutputTokens: TokenThreshold}}}
	if !BuildWindow(usage, initialState(usage)).Urgent {
		t.Fatal("token threshold not accelerated")
	}
}
func TestWindowRedactionAndExcludesReasoning(t *testing.T) {
	events := []model.RenderEvent{event("goal", "UserPrompt", "token=abcdefghijklmnop and a@b.com", 0), event("think", "ThinkingChunk", "private reasoning", 0), {EventID: "tool", Type: "ToolInvocation", ToolInput: map[string]any{"api_key": "abcdefghijklmnop"}}}
	w := BuildWindow(events, initialState(events))
	raw, _ := json.Marshal(w)
	if strings.Contains(string(raw), "abcdefghijklmnop") || strings.Contains(string(raw), "a@b.com") || strings.Contains(string(raw), "private reasoning") {
		t.Fatalf("sensitive input leaked %s", raw)
	}
}
func TestOutputCitationsBoundsAndPreservedStages(t *testing.T) {
	events := []model.RenderEvent{event("old", "TextChunk", "old", 0), event("new", "TextChunk", "new", 1)}
	s := initialState(events)
	s.Cursor = 1
	s.Digest = PrefixDigests(events)[1]
	s.Board.Stages = []Stage{{ID: "done", Title: "Done", Status: "completed", SourceRefs: []Ref{Reference(events[0])}}}
	w := BuildWindow(events, s)
	for _, raw := range []string{`{}`, `{"current":{}}`, `{"current":{"action":"   ","source_refs":[]}}`} {
		if _, err := ValidateOutput(raw, w); err == nil {
			t.Fatalf("accepted blank output %s", raw)
		}
	}
	if _, err := ValidateOutput(outputFor(Reference(events[0])), w); err != nil {
		t.Fatalf("valid carried stage rejected: %v", err)
	}
	bad := Reference(events[1])
	bad.TurnIndex = 99
	if _, err := ValidateOutput(outputFor(bad), w); err == nil {
		t.Fatal("accepted forged jump location")
	}
	bad = Reference(events[1])
	bad.ToolCallID = "invented"
	if _, err := ValidateOutput(outputFor(bad), w); err == nil {
		t.Fatal("accepted forged tool ID")
	}
	if _, err := ValidateOutput(strings.Repeat("x", MaxOutputBytes+1), w); err == nil {
		t.Fatal("unbounded output")
	}
	out, err := ValidateOutput(outputFor(Reference(events[1])), w)
	if err != nil {
		t.Fatal(err)
	}
	b := Empty()
	for i := 0; i < 30; i++ {
		out.Updates[0].Text = fmt.Sprintf("progress %d", i)
		ApplyOutput(&b, out, time.Now())
	}
	if len(b.Updates) != MaxUpdates {
		t.Fatal(len(b.Updates))
	}
	ApplyOutput(&b, out, time.Now())
	if len(b.Updates) != MaxUpdates {
		t.Fatal("duplicate update")
	}
	// A previous citation not in current source cannot be laundered into output.
	events = events[1:]
	w = BuildWindow(events, State{Board: s.Board})
	if _, err = ValidateOutput(outputFor(Ref{EventID: "old"}), w); err == nil {
		t.Fatal("accepted stale prior reference")
	}
}
func TestSchedulingCooldownRetryAndUrgency(t *testing.T) {
	now := time.Now()
	old := now.Add(-20 * time.Second)
	if Due(now, old, time.Time{}, time.Time{}, 0, true, 9000, 10*time.Second, 3*time.Second) {
		t.Fatal("idle spend")
	}
	if Due(now, old, now.Add(-time.Second), time.Time{}, 1, true, 9000, 10*time.Second, 3*time.Second) {
		t.Fatal("cooldown bypass")
	}
	if Due(now, old, old, now.Add(time.Minute), 1, true, 9000, 10*time.Second, 3*time.Second) {
		t.Fatal("retry bypass")
	}
	if !Due(now, now, old, time.Time{}, 1, true, 1, 10*time.Second, 3*time.Second) {
		t.Fatal("error not accelerated")
	}
	if !Due(now, old, old, time.Time{}, 1, false, 1, 10*time.Second, 3*time.Second) {
		t.Fatal("normal interval")
	}
	if Due(now, now, old, time.Time{}, 1, false, 1, 10*time.Second, 3*time.Second) {
		t.Fatal("called before interval")
	}
	if !strings.Contains(SystemInstruction("zh-CN"), "Simplified Chinese") || strings.Contains(SystemInstruction("en"), "likely next step") {
		t.Fatal("locale/declared plan contract")
	}
}
