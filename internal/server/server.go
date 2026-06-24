// Package server exposes the gateway over HTTP with an OpenAI-compatible API.
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"frostgate/internal/gateway"
	"frostgate/internal/governance"
	"frostgate/internal/schema"
)

// Server holds the gateway and routes HTTP requests to it.
type Server struct {
	gw *gateway.Gateway
}

// New builds a Server.
func New(gw *gateway.Gateway) *Server { return &Server{gw: gw} }

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	// Provider-prefixed drop-in paths mirror Bifrost's /openai, /anthropic.
	mux.HandleFunc("/openai/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/anthropic/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/cluster", s.handleCluster)
	mux.HandleFunc("/", s.handleDashboard)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "frostgate"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(s.gw.Metrics.Render()))
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Decode into a map first so we can preserve unknown fields as
	// passthrough, then into the typed struct.
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req, err := decodeRequest(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// If the path carries a provider prefix and the model has none, apply it.
	if p := providerFromPath(r.URL.Path); p != "" && !strings.Contains(req.Model, "/") {
		req.Model = p + "/" + req.Model
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "missing 'model'")
		return
	}

	// Governance: authorize the virtual key, enforce rate limits / budgets /
	// model allow-lists before doing any upstream work.
	secret := bearerToken(r)
	gov := s.gw.Governor.Authorize(secret, req.Model)
	if gov.Decision != governance.Allow {
		s.gw.Metrics.Errors.Add(1)
		writeErr(w, statusForDecision(gov.Decision), "request denied: "+gov.Decision.String())
		return
	}

	if req.Stream {
		s.streamChat(w, r, req, gov.BillingID)
		return
	}
	resp, err := s.gw.Complete(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.gw.Governor.RecordSpend(gov.BillingID, resp.Usage.TotalTokens)
	writeJSON(w, http.StatusOK, resp)
}

// statusForDecision maps a governance denial to an HTTP status.
func statusForDecision(d governance.Decision) int {
	switch d {
	case governance.DenyUnauthorized:
		return http.StatusUnauthorized
	case governance.DenyRateLimited:
		return http.StatusTooManyRequests
	case governance.DenyBudget, governance.DenyModel:
		return http.StatusForbidden
	}
	return http.StatusForbidden
}

// bearerToken extracts the credential from "Authorization: Bearer <x>".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *schema.ChatRequest, secret string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	attempts, err := s.gw.PrepareStream(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flush := func() { flusher.Flush() }
	won, _, err := s.gw.Router.Stream(r.Context(), attempts, req, w, flush)
	if err != nil {
		// Best-effort error frame; headers are already sent.
		_ = writeSSEError(w, err.Error())
		flush()
		s.gw.Metrics.Errors.Add(1)
		return
	}
	s.gw.Metrics.ObserveProvider(won.Provider.Name())
}

// decodeRequest splits known fields into the typed struct and keeps the rest
// as passthrough so provider-specific params survive.
func decodeRequest(raw map[string]json.RawMessage) (*schema.ChatRequest, error) {
	req := &schema.ChatRequest{Passthrough: map[string]json.RawMessage{}}
	known := map[string]bool{
		"model": true, "messages": true, "stream": true, "temperature": true,
		"max_tokens": true, "top_p": true, "stop": true, "tools": true, "tool_choice": true,
	}
	// Re-marshal the known subset and unmarshal into the struct.
	knownObj := map[string]json.RawMessage{}
	for k, v := range raw {
		if known[k] {
			knownObj[k] = v
		} else {
			req.Passthrough[k] = v
		}
	}
	b, _ := json.Marshal(knownObj)
	if err := json.Unmarshal(b, req); err != nil {
		return nil, err
	}
	return req, nil
}

// providerFromPath extracts "openai" from "/openai/v1/chat/completions".
func providerFromPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/openai/"):
		return "openai"
	case strings.HasPrefix(path, "/anthropic/"):
		return "anthropic"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "frostgate_error"},
	})
}

func writeSSEError(w http.ResponseWriter, msg string) error {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg}})
	_, err := w.Write([]byte("data: " + string(b) + "\n\n"))
	return err
}
