package execution

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactCitationsRestoreExactLongToolIdentity(t *testing.T) {
	canonical := Ref{EventID: "source-1", TurnIndex: 2, ToolCallID: "call-" + strings.Repeat("a", 160)}
	window := Window{Goal: "Inspect authentication", Events: []InputEvent{{Ref: canonical}}}
	raw := `{"current":{"action":"Reading authentication logic","source_refs":[{"event_id":"source-1","turn_index":2}]},"stages":[{"id":"inspect","title":"Inspect","status":"active","source_refs":[{"event_id":"source-1","turn_index":2}]}],"updates":[{"text":"Opened the authentication file","source_refs":[{"event_id":"source-1","turn_index":2}]}]}`
	out, err := ValidateOutput(raw, window)
	if err != nil {
		t.Fatal(err)
	}
	for _, refs := range [][]Ref{out.Current.SourceRefs, out.Stages[0].SourceRefs, out.Updates[0].SourceRefs} {
		if len(refs) != 1 || refs[0] != canonical {
			t.Fatalf("compact citation lost its exact source link: %+v", refs)
		}
	}
	encoded, _ := json.Marshal(out)
	if !strings.Contains(string(encoded), canonical.ToolCallID) {
		t.Fatal("board response must retain the complete native tool identity")
	}
	for name, forged := range map[string]string{
		"unknown event": strings.ReplaceAll(raw, "source-1", "unknown"),
		"wrong turn":    strings.ReplaceAll(raw, `"turn_index":2`, `"turn_index":3`),
		"wrong tool":    strings.ReplaceAll(raw, `"turn_index":2`, `"turn_index":2,"tool_call_id":"forged"`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateOutput(forged, window); err == nil {
				t.Fatal("forged source identity was accepted")
			}
		})
	}
}
