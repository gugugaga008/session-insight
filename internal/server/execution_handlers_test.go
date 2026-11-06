package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbsteel/session-insight/internal/db"
	"github.com/bbsteel/session-insight/internal/execution"
	"github.com/bbsteel/session-insight/internal/llm"
	"github.com/bbsteel/session-insight/internal/model"
	"github.com/bbsteel/session-insight/internal/reader"
)

type executionAPIReader struct {
	mu     sync.Mutex
	agent  string
	events []model.RenderEvent
}

func (rd *executionAPIReader) AgentType() string   { return rd.agent }
func (rd *executionAPIReader) DisplayName() string { return rd.agent }
func (rd *executionAPIReader) ListSessions() ([]model.Session, error) {
	return []model.Session{{ID: "same", AgentType: rd.agent}}, nil
}
func (rd *executionAPIReader) GetSession(id string) (*model.SessionDetail, error) {
	if id != "same" {
		return nil, nil
	}
	return &model.SessionDetail{Session: model.Session{ID: id, AgentType: rd.agent}}, nil
}
func (rd *executionAPIReader) RenderANSI(string, int) (string, error) { return "", nil }
func (rd *executionAPIReader) GetRenderEvents(string) ([]model.RenderEvent, error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return append([]model.RenderEvent{}, rd.events...), nil
}

type executionAPIClient struct {
	calls  atomic.Int32
	seenMu sync.Mutex
	prompt string
}

func (c *executionAPIClient) ListModels(context.Context) ([]llm.Model, error) { return nil, nil }
func (c *executionAPIClient) Generate(ctx context.Context, prompt string, _ llm.StatusFunc) (string, error) {
	return c.GenerateWithSystem(ctx, "", prompt, nil)
}
func (c *executionAPIClient) GenerateWithSystem(_ context.Context, _, prompt string, _ llm.StatusFunc) (string, error) {
	c.calls.Add(1)
	c.seenMu.Lock()
	c.prompt = prompt
	c.seenMu.Unlock()
	var w execution.Window
	_ = json.Unmarshal([]byte(prompt), &w)
	r := w.Events[len(w.Events)-1].Ref
	b, _ := json.Marshal(execution.Output{Goal: "Fix tests", Current: execution.Current{Action: "Inspecting test failure", Purpose: "Find the cause", SourceRefs: []execution.Ref{r}}, Stages: []execution.Stage{}, Updates: []execution.Update{}})
	return string(b), nil
}
func executionHTTP(s *Server, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "localhost"
	if method == "POST" {
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Mux.ServeHTTP(w, r)
	return w
}
func executionTestServer(t *testing.T) (*Server, *executionAPIReader, *executionAPIClient) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rd := &executionAPIReader{agent: "pi", events: []model.RenderEvent{{EventID: "pi-only", Type: "ToolResult", TurnIndex: 2, ToolCallID: "call", ExitCode: 1, Stderr: "failure token=supersecret12345"}}}
	other := &executionAPIReader{agent: "claude", events: []model.RenderEvent{{EventID: "wrong-agent", Type: "UserPrompt", Text: "private other agent"}}}
	s := New(database, []reader.BaseSessionReader{other, rd})
	client := &executionAPIClient{}
	s.newGenerationClient = func(cfg llm.Config) (llm.Client, error) {
		if cfg.MaxOutputTokens != 3000 {
			t.Errorf("execution output budget missing: %d", cfg.MaxOutputTokens)
		}
		return client, nil
	}
	t.Cleanup(func() { s.CloseExecutionMonitors(); database.Close() })
	return s, rd, client
}
func decodeExecutionBoard(t *testing.T, w *httptest.ResponseRecorder) execution.Board {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var b execution.Board
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}
func TestExecutionHTTPRequiresExactAgentAndProtectedWrites(t *testing.T) {
	s, _, _ := executionTestServer(t)
	cases := []struct {
		method, url, body string
		status            int
	}{
		{"GET", "/api/sessions/same/execution", "", 400},
		{"GET", "/api/sessions/same/execution?agent=missing", "", 404},
		{"GET", "/api/sessions/missing/execution?agent=pi", "", 404},
		{"GET", "/api/sessions/same/execution?agent=pi&locale=xx", "", 400},
		{"POST", "/api/sessions/same/execution", `{"agent":"pi","locale":"en","enabled":true}`, 412},
		{"POST", "/api/sessions/same/execution", `{"agent":"pi","locale":"en"}`, 400},
		{"POST", "/api/sessions/same/execution", `{"agent":"pi","locale":"en","enabled":false} {}`, 400},
		{"POST", "/api/sessions/same/execution?agent=claude", `{"agent":"pi","locale":"en","enabled":false}`, 400},
		{"POST", "/api/sessions/same/execution", `{"agent":"pi","locale":"en","enabled":false,"provider_id":-1}`, 400},
	}
	for _, tt := range cases {
		w := executionHTTP(s, tt.method, tt.url, tt.body)
		if w.Code != tt.status {
			t.Errorf("%s %s got %d want %d: %s", tt.method, tt.url, w.Code, tt.status, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/api/sessions/same/execution", strings.NewReader(`{"agent":"pi","locale":"en","enabled":false}`))
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unsafe write %d", w.Code)
	}
}
func TestExecutionHTTPNeverEnabledNoSpendBackgroundAndPause(t *testing.T) {
	s, _, client := executionTestServer(t)
	w := executionHTTP(s, "GET", "/api/sessions/same/execution?agent=pi&locale=zh-CN", "")
	b := decodeExecutionBoard(t, w)
	if b.Enabled || b.Status != "idle" || b.Current.SourceRefs == nil || b.Activity.SourceRefs == nil || b.Stages == nil || b.Updates == nil {
		t.Fatalf("empty contract %s", w.Body.String())
	}
	if client.calls.Load() != 0 {
		t.Fatal("GET spent")
	}
	id, err := s.DB.AddLLMProvider(db.LLMProvider{Name: "stub", Kind: "api", BaseURL: "http://secret-endpoint", APIKey: "provider-secret", Headers: `{"Authorization":"secret-header"}`, ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SetDefaultLLMProviderID(id); err != nil {
		t.Fatal(err)
	}
	b = decodeExecutionBoard(t, executionHTTP(s, "POST", "/api/sessions/same/execution", `{"agent":"pi","locale":"zh-CN","enabled":true}`))
	if !b.Enabled || b.ProviderID != id {
		t.Fatal(b)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		b = decodeExecutionBoard(t, executionHTTP(s, "GET", "/api/sessions/same/execution?agent=pi", ""))
		if b.UpdatedAt != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b.UpdatedAt == nil || b.PendingEvents != 0 || b.Current.SourceRefs[0].EventID != "pi-only" || b.Current.SourceRefs[0].TurnIndex != 2 || b.Current.SourceRefs[0].ToolCallID != "call" {
		t.Fatalf("background result %+v", b)
	}
	client.seenMu.Lock()
	prompt := client.prompt
	client.seenMu.Unlock()
	if strings.Contains(prompt, "wrong-agent") || strings.Contains(prompt, "supersecret12345") || strings.Contains(prompt, "provider-secret") {
		t.Fatal("bad source selection/redaction")
	}
	if client.calls.Load() != 1 {
		t.Fatal("duplicate calls")
	}
	// GET locale does not silently reconfigure the saved monitor.
	state, _, err := s.DB.LoadExecution(execution.Key{Agent: "pi", Session: "same"})
	if err != nil || state.Locale != "zh-CN" {
		t.Fatal(state, err)
	}
	b = decodeExecutionBoard(t, executionHTTP(s, "POST", "/api/sessions/same/execution", `{"agent":"pi","locale":"zh-CN","enabled":false}`))
	if b.Enabled || b.Status != "paused" {
		t.Fatal(b)
	}
	paused := decodeExecutionBoard(t, executionHTTP(s, "POST", "/api/sessions/same/execution/refresh", `{"agent":"pi","locale":"zh-CN","enabled":true}`))
	if paused.Enabled {
		t.Fatal("refresh enabled paused board")
	}
	w = executionHTTP(s, "GET", "/api/sessions/same/execution?agent=pi", "")
	for _, secret := range []string{"provider-secret", "secret-header", "secret-endpoint"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("provider secret exposed")
		}
	}
}
