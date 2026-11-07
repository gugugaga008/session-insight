// Package execution maintains a bounded, evidence-backed AI interpretation of a
// single session. Activity is observed source data; current/stages/updates are
// model interpretations, never a session-completion signal.
package execution

import "time"

type Ref struct {
	EventID    string `json:"event_id"`
	TurnIndex  int    `json:"turn_index"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}
type Current struct {
	Action     string `json:"action"`
	Purpose    string `json:"purpose"`
	Blocker    string `json:"blocker"`
	NextStep   string `json:"next_step"`
	SourceRefs []Ref  `json:"source_refs"`
}
type Activity struct {
	Kind       string     `json:"kind"`
	ToolName   string     `json:"tool_name"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	SourceRefs []Ref      `json:"source_refs"`
}
type Stage struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Summary    string `json:"summary"`
	SourceRefs []Ref  `json:"source_refs"`
}
type Update struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
	SourceRefs []Ref     `json:"source_refs"`
}
type Board struct {
	Enabled       bool       `json:"enabled"`
	Status        string     `json:"status"`
	Goal          string     `json:"goal"`
	Current       Current    `json:"current"`
	Activity      Activity   `json:"activity"`
	Stages        []Stage    `json:"stages"`
	Updates       []Update   `json:"updates"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	PendingEvents int        `json:"pending_events"`
	ErrorCode     string     `json:"error_code,omitempty"`
	ProviderID    int64      `json:"provider_id,omitempty"`
	SourceLimited bool       `json:"source_limited"`
}
type Key struct {
	Agent   string
	Session string
}

// State contains only bounded board data and a verified prefix cursor, never
// credentials or a persisted transcript. Cursor advances only after success.
// RequestScopeVersion invalidates older interpretations that mixed separate
// user requests into one apparent phase route.
const RequestScopeVersion = 2

type State struct {
	Board        Board     `json:"board"`
	Locale       string    `json:"locale"`
	TaskID       string    `json:"task_id,omitempty"`
	ScopeVersion int       `json:"scope_version"`
	Cursor       int       `json:"cursor"`
	Digest       string    `json:"digest"`
	Initialized  bool      `json:"initialized"`
	RetryAt      time.Time `json:"retry_at"`
	Failures     int       `json:"failures"`
}

func Empty() Board {
	return Board{Status: "idle", Current: Current{SourceRefs: []Ref{}}, Activity: Activity{Kind: "idle", SourceRefs: []Ref{}}, Stages: []Stage{}, Updates: []Update{}}
}
func normalize(b *Board) {
	if b.Current.SourceRefs == nil {
		b.Current.SourceRefs = []Ref{}
	}
	if b.Activity.SourceRefs == nil {
		b.Activity.SourceRefs = []Ref{}
	}
	if b.Stages == nil {
		b.Stages = []Stage{}
	}
	if b.Updates == nil {
		b.Updates = []Update{}
	}
	for i := range b.Stages {
		if b.Stages[i].SourceRefs == nil {
			b.Stages[i].SourceRefs = []Ref{}
		}
	}
	for i := range b.Updates {
		if b.Updates[i].SourceRefs == nil {
			b.Updates[i].SourceRefs = []Ref{}
		}
	}
}
