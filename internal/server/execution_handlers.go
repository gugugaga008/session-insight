package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/bbsteel/session-insight/internal/execution"
	"github.com/bbsteel/session-insight/internal/llm"
	"github.com/bbsteel/session-insight/internal/model"
	"github.com/bbsteel/session-insight/internal/reader"
)

// executionReader requires both the requested adapter and a matching detail;
// duplicate session IDs in another agent can never supply source evidence.
func (s *Server) executionReader(key execution.Key) (reader.BaseSessionReader, error) {
	for _, rd := range s.Readers {
		if rd.AgentType() != key.Agent {
			continue
		}
		d, err := rd.GetSession(key.Session)
		if err == nil && d != nil && d.ID == key.Session && d.AgentType == key.Agent {
			return rd, nil
		}
	}
	return nil, errors.New("session_not_found")
}
func (s *Server) initExecution() {
	if s.DB == nil {
		return
	}
	s.execution = execution.NewManager(s.DB, func(ctx context.Context, key execution.Key) ([]model.RenderEvent, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rd, err := s.executionReader(key)
		if err != nil {
			return nil, err
		}
		for attempt := 0; attempt < 3; attempt++ {
			var before int64
			p, hasRevision := rd.(reader.LiveRevisionProvider)
			if hasRevision {
				before, err = p.LiveRevision(key.Session)
				if err != nil {
					return nil, err
				}
			}
			// Read normalized source directly. Prefix hashes also detect rewrites
			// on adapters with no revision signal, or unchanged event counts.
			events, err := rd.GetRenderEvents(key.Session)
			if err != nil {
				return nil, err
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if hasRevision {
				after, e := p.LiveRevision(key.Session)
				if e != nil {
					return nil, e
				}
				if before != after {
					continue
				}
			}
			return events, nil
		}
		return nil, errors.New("session_changing")
	}, func(ctx context.Context, providerID int64, system, prompt string) (string, error) {
		provider, err := s.DB.GetLLMProvider(providerID)
		if err != nil {
			return "", err
		}
		if provider == nil {
			return "", errors.New("no_provider")
		}
		cfg := providerLLMConfig(provider)
		cfg.MaxOutputTokens = 3000
		client, err := s.generationClient(cfg)
		if err != nil {
			return "", err
		}
		if sg, ok := client.(llm.SystemPromptGenerator); ok {
			return sg.GenerateWithSystem(ctx, system, prompt, nil)
		}
		return client.Generate(ctx, system+"\n\nUNTRUSTED SOURCE JSON:\n"+prompt, nil)
	}, execution.Options{})
	if err := s.execution.Restore(); err != nil {
		log.Printf("execution monitor restore failed")
	}
}

// CloseExecutionMonitors is available to embedders/tests with an explicit
// shutdown lifecycle. Process restart resumes only persisted enabled monitors.
func (s *Server) CloseExecutionMonitors() {
	if s.execution != nil {
		s.execution.Close()
	}
}

type executionRequest struct {
	Agent      string `json:"agent"`
	Locale     string `json:"locale"`
	Enabled    *bool  `json:"enabled"`
	ProviderID int64  `json:"provider_id,omitempty"`
}

func (s *Server) requireExecution(w http.ResponseWriter, r *http.Request, agent, locale string) (execution.Key, bool) {
	key := execution.Key{Agent: strings.TrimSpace(agent), Session: r.PathValue("id")}
	if key.Agent == "" {
		writeAPIError(w, http.StatusBadRequest, "missing_agent")
		return key, false
	}
	if locale != "en" && locale != "zh-CN" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request")
		return key, false
	}
	if !s.requireDB(w) {
		return key, false
	}
	if _, err := s.executionReader(key); err != nil {
		writeAPIError(w, http.StatusNotFound, "session_not_found")
		return key, false
	}
	return key, true
}
func (s *Server) handleGetExecution(w http.ResponseWriter, r *http.Request) {
	locale := r.URL.Query().Get("locale")
	if locale == "" {
		locale = "en"
	}
	key, ok := s.requireExecution(w, r, r.URL.Query().Get("agent"), locale)
	if !ok {
		return
	}
	board, err := s.execution.Get(key)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "execution_failed")
		return
	}
	writeJSON(w, board)
}
func (s *Server) decodeExecution(w http.ResponseWriter, r *http.Request) (executionRequest, bool) {
	var req executionRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(&struct{}{}) != io.EOF || req.Enabled == nil || req.ProviderID < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request")
		return req, false
	}
	if queryAgent := r.URL.Query().Get("agent"); queryAgent != "" && queryAgent != req.Agent {
		writeAPIError(w, http.StatusBadRequest, "invalid_request")
		return req, false
	}
	return req, true
}
func (s *Server) handlePostExecution(w http.ResponseWriter, r *http.Request) {
	if rejectUnsafeWrite(w, r) {
		return
	}
	req, ok := s.decodeExecution(w, r)
	if !ok {
		return
	}
	key, ok := s.requireExecution(w, r, req.Agent, req.Locale)
	if !ok {
		return
	}
	providerID := req.ProviderID
	if *req.Enabled {
		if providerID == 0 {
			previous, err := s.execution.Get(key)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, "execution_failed")
				return
			}
			providerID = previous.ProviderID
		}
		provider, err := s.resolveProvider(providerID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "execution_failed")
			return
		}
		if provider == nil {
			writeAPIError(w, http.StatusPreconditionFailed, "no_provider")
			return
		}
		providerID = provider.ID
	}
	board, err := s.execution.Configure(key, req.Locale, *req.Enabled, providerID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "execution_failed")
		return
	}
	writeJSON(w, board)
}
func (s *Server) handleRefreshExecution(w http.ResponseWriter, r *http.Request) {
	if rejectUnsafeWrite(w, r) {
		return
	}
	req, ok := s.decodeExecution(w, r)
	if !ok {
		return
	}
	key, ok := s.requireExecution(w, r, req.Agent, req.Locale)
	if !ok {
		return
	}
	board, err := s.execution.Refresh(key)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "execution_failed")
		return
	}
	writeJSON(w, board)
}
