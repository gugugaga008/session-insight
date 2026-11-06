package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bbsteel/session-insight/internal/insight"
	"github.com/bbsteel/session-insight/internal/model"
)

const (
	TailEvents      = 80
	MaxEvents       = 100
	MaxContextTools = 12
	MaxEventRunes   = 1200
	MaxWindowRunes  = 18000
	MaxOutputBytes  = 24000
	MaxStages       = 8
	MaxUpdates      = 20
	TokenThreshold  = 2048
)

// PrefixDigests detects in-place edits, reordered events, truncation and
// replaced source files even when event counts and stat revisions coincide.
// CursorEvents excludes ephemeral renderer status markers from the durable
// source cursor. They remain available to ObserveActivity but can disappear or
// change timestamp on every read without invalidating successful AI work.
func CursorEvents(events []model.RenderEvent) []model.RenderEvent {
	out := make([]model.RenderEvent, 0, len(events))
	for _, e := range events {
		if e.Type == "AgentSpecific" && e.Subtype == "in_progress" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func PrefixDigests(events []model.RenderEvent) []string {
	h := sha256.New()
	out := make([]string, len(events)+1)
	out[0] = hex.EncodeToString(h.Sum(nil))
	for i, e := range events {
		b, _ := json.Marshal(e)
		h.Write(b)
		h.Write([]byte{'\n'})
		out[i+1] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}
func Reference(e model.RenderEvent) Ref {
	return Ref{EventID: e.EventID, TurnIndex: e.TurnIndex, ToolCallID: e.ToolCallID}
}
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Scrub reuses the insight redactor for every string crossing the model/data
// boundary. Tool arguments are serialized as untrusted text, never instructions.
func Scrub(s string) string {
	b, _ := insight.Redact(insight.Bundle{Facts: []insight.EvidenceFact{{Statement: s}}})
	return b.Facts[0].Statement
}
func toolAliases(e model.RenderEvent) []string {
	keys := []string{}
	if e.ToolCallID != "" {
		keys = append(keys, fmt.Sprintf("call:%d:%s", e.TurnIndex, e.ToolCallID))
	}
	id := e.EventID
	if e.Type == "ToolResult" {
		id = e.ParentEventID
	}
	if id != "" {
		keys = append(keys, "event:"+id)
	}
	return keys
}
func openTools(events []model.RenderEvent, end int) map[int]bool {
	open := map[int]bool{}
	aliases := map[string]int{}
	for i := 0; i < end; i++ {
		e := events[i]
		if e.Type == "ToolInvocation" {
			open[i] = true
			for _, k := range toolAliases(e) {
				aliases[k] = i
			}
		}
		if e.Type == "ToolResult" {
			for _, k := range toolAliases(e) {
				if index, ok := aliases[k]; ok {
					delete(open, index)
					for _, a := range toolAliases(events[index]) {
						delete(aliases, a)
					}
				}
			}
		}
	}
	return open
}

// ObserveActivity preserves unmatched calls across window boundaries. A stale
// open tool is still an observed open pair, not proof its process is alive.
func ObserveActivity(events []model.RenderEvent) Activity {
	a := Activity{Kind: "idle", SourceRefs: []Ref{}}
	open := openTools(events, len(events))
	last := -1
	for i, e := range events {
		if e.Type == "UserPrompt" || e.Type == "TextChunk" || e.Type == "ToolResult" || e.Subtype == "in_progress" {
			last = i
		}
	}
	latest := -1
	for i := range open {
		if i > latest {
			latest = i
		}
	}
	if latest >= 0 {
		e := events[latest]
		a.Kind = "tool"
		a.ToolName = clip(Scrub(e.ToolName), 120)
		a.SourceRefs = []Ref{Reference(e)}
		if !e.Timestamp.IsZero() {
			t := e.Timestamp.UTC()
			a.StartedAt = &t
		}
	} else if last >= 0 && events[last].Subtype == "in_progress" {
		a.Kind = "responding"
		a.SourceRefs = []Ref{Reference(events[last])}
		if !events[last].Timestamp.IsZero() {
			t := events[last].Timestamp.UTC()
			a.StartedAt = &t
		}
	}
	return a
}

type InputEvent struct {
	Ref      Ref    `json:"ref"`
	Type     string `json:"type"`
	ToolName string `json:"tool_name,omitempty"`
	Text     string `json:"text,omitempty"`
	Failed   bool   `json:"failed,omitempty"`
}
type Window struct {
	Goal          string       `json:"goal_source"`
	Previous      Current      `json:"previous_interpretation"`
	Stages        []Stage      `json:"previous_stages"`
	Events        []InputEvent `json:"events"`
	Context       []InputEvent `json:"tool_context"`
	SourceLimited bool         `json:"source_limited"`
	End           int          `json:"-"`
	Urgent        bool         `json:"-"`
	Runes         int          `json:"-"`
}

func inputEvent(e model.RenderEvent) InputEvent {
	x := InputEvent{Ref: Reference(e), Type: e.Type, ToolName: clip(Scrub(e.ToolName), 120), Failed: e.ExitCode != 0 || e.ErrorKind != "" || e.TimedOut || e.Rejected}
	// Reasoning, metadata, payload and environment are deliberately excluded.
	text := e.Text
	if e.Type == "ThinkingChunk" || e.Type == "ThinkingStart" || e.Type == "ThinkingEnd" {
		text = ""
	}
	if e.Type == "ToolInvocation" {
		b, _ := json.Marshal(e.ToolInput)
		text = string(b)
	}
	if e.Type == "ToolResult" {
		text = e.Stdout + "\n" + e.Stderr
	}
	x.Text = clip(Scrub(text), MaxEventRunes)
	return x
}

// BuildWindow consumes deltas from the committed cursor in order; backlogs
// become multiple bounded requests. Bootstrap selection is done only once by
// Reconcile. Earlier tool invocations/results are context, not cursor movement.
func BuildWindow(events []model.RenderEvent, state State) Window {
	w := Window{Previous: state.Board.Current, Stages: state.Board.Stages, Events: []InputEvent{}, Context: []InputEvent{}, SourceLimited: state.Board.SourceLimited, End: state.Cursor}
	// The latest explicit user request is the current goal anchor; old
	// requests must not override a later change of direction.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type == "UserPrompt" && strings.TrimSpace(e.Text) != "" {
			w.Goal = clip(Scrub(e.Text), 1000)
			break
		}
	}
	// Carry only previously validated references still present in this source.
	// This lets a completed stage survive delta windows without inventing
	// fresh evidence or permitting arbitrary historical model citations.
	available := map[string]bool{}
	for _, e := range events {
		available[refKey(Reference(e))] = true
	}
	validRefs := func(refs []Ref) []Ref {
		out := []Ref{}
		for _, r := range refs {
			if available[refKey(r)] {
				out = append(out, r)
			}
		}
		return out
	}
	w.Previous.SourceRefs = validRefs(w.Previous.SourceRefs)
	w.Stages = append([]Stage{}, w.Stages...)
	for i := range w.Stages {
		w.Stages[i].SourceRefs = validRefs(w.Stages[i].SourceRefs)
	}
	budget := 0
	var outputTokens int64
	for i := state.Cursor; i < len(events) && len(w.Events) < MaxEvents; i++ {
		x := inputEvent(events[i])
		b, _ := json.Marshal(x)
		n := len([]rune(string(b)))
		if n > MaxWindowRunes-4000 {
			w.SourceLimited = true
			w.End = i + 1
			continue
		}
		if budget+n > MaxWindowRunes-4000 && len(w.Events) > 0 {
			break
		}
		w.Events = append(w.Events, x)
		budget += n
		w.End = i + 1
		w.Urgent = w.Urgent || x.Failed
		if usage := events[i].TokenUsage; usage != nil {
			outputTokens += max(int64(0), usage.OutputTokens)
		}
		w.Urgent = w.Urgent || outputTokens >= TokenThreshold
		if events[i].Truncated || len([]rune(events[i].Text)) > MaxEventRunes || len([]rune(x.Text)) >= MaxEventRunes {
			w.SourceLimited = true
		}
	}
	// Preserve still-open invocations and the invocation of results in this
	// batch, even if the invocation fell before bootstrap's recent tail.
	open := openTools(events, state.Cursor)
	wanted := map[string]bool{}
	for i := state.Cursor; i < w.End; i++ {
		if events[i].Type == "ToolResult" {
			for _, k := range toolAliases(events[i]) {
				wanted[k] = true
			}
		}
	}
	count := 0
	for i := state.Cursor - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != "ToolInvocation" {
			continue
		}
		needed := open[i]
		for _, k := range toolAliases(e) {
			needed = needed || wanted[k]
		}
		if !needed {
			continue
		}
		if count >= MaxContextTools {
			w.SourceLimited = true
			break
		}
		x := inputEvent(e)
		b, _ := json.Marshal(x)
		n := len([]rune(string(b)))
		if budget+n > MaxWindowRunes {
			w.SourceLimited = true
			continue
		}
		w.Context = append(w.Context, x)
		budget += n
		count++
	}
	w.Runes = budget
	return w
}

// Reconcile resets stale interpretations on any committed-prefix rewrite. A
// first observation skips only bounded historical bootstrap, never live deltas.
func Reconcile(s *State, events []model.RenderEvent, digests []string, now time.Time) bool {
	reset := !s.Initialized || s.Cursor > len(events) || s.Cursor < 0 || s.Digest != digests[s.Cursor]
	if reset {
		enabled, provider := s.Board.Enabled, s.Board.ProviderID
		s.Board = Empty()
		s.Board.Enabled = enabled
		s.Board.ProviderID = provider
		s.Cursor = 0
		if len(events) > TailEvents {
			s.Cursor = len(events) - TailEvents
		}
		s.Digest = digests[s.Cursor]
		s.Initialized = true
		s.Failures = 0
		s.RetryAt = time.Time{}
		s.Board.SourceLimited = s.Cursor > 0
	}
	s.Board.PendingEvents = len(events) - s.Cursor
	s.Board.Activity = ObserveActivity(events)
	t := now.UTC()
	s.Board.ObservedAt = &t
	return reset
}

func Due(now, pendingSince, lastAttempt, retryAt time.Time, pending int, urgent bool, runes int, interval, cooldown time.Duration) bool {
	if pending == 0 || now.Before(retryAt) || (!lastAttempt.IsZero() && now.Sub(lastAttempt) < cooldown) {
		return false
	}
	return urgent || runes >= 8000 || (!pendingSince.IsZero() && now.Sub(pendingSince) >= interval)
}
