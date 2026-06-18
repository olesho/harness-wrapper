package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

type convEntry struct {
	id   string
	conv *chat.Conversation
	fan  *fanout

	mu      sync.Mutex
	tokens  map[string]func() // control token -> release()
	harness string
}

func (e *convEntry) acquireToken(release func()) string {
	tok := newToken()
	e.mu.Lock()
	e.tokens[tok] = release
	e.mu.Unlock()
	return tok
}

func (e *convEntry) releaseToken(tok string) bool {
	e.mu.Lock()
	rel, ok := e.tokens[tok]
	if ok {
		delete(e.tokens, tok)
	}
	e.mu.Unlock()
	if ok {
		rel()
	}
	return ok
}

func (e *convEntry) hasToken(tok string) bool {
	e.mu.Lock()
	_, ok := e.tokens[tok]
	e.mu.Unlock()
	return ok
}

func (e *convEntry) releaseAll() {
	e.mu.Lock()
	rels := make([]func(), 0, len(e.tokens))
	for k, r := range e.tokens {
		rels = append(rels, r)
		delete(e.tokens, k)
	}
	e.mu.Unlock()
	for _, r := range rels {
		r()
	}
}

type Server struct {
	mu    sync.RWMutex
	convs map[string]*convEntry
}

func NewServer() *Server {
	return &Server{convs: make(map[string]*convEntry)}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/turns", s.runTurn)
	mux.HandleFunc("POST /v1/conversations", s.openConv)
	mux.HandleFunc("GET /v1/conversations", s.listConvs)
	mux.HandleFunc("DELETE /v1/conversations/{id}", s.closeConv)
	mux.HandleFunc("POST /v1/conversations/{id}/control", s.acquireControl)
	mux.HandleFunc("DELETE /v1/conversations/{id}/control/{token}", s.releaseControl)
	mux.HandleFunc("POST /v1/conversations/{id}/messages", s.sendMessage)
	mux.HandleFunc("POST /v1/conversations/{id}/input", s.answerInput)
	mux.HandleFunc("GET /v1/conversations/{id}/events", s.streamEvents)
	mux.HandleFunc("GET /v1/conversations/{id}/history", s.history)
	return mux
}

// Shutdown closes every conversation.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	convs := make([]*convEntry, 0, len(s.convs))
	for _, e := range s.convs {
		convs = append(convs, e)
	}
	s.convs = make(map[string]*convEntry)
	s.mu.Unlock()
	for _, e := range convs {
		e.releaseAll()
		_ = e.conv.Close(ctx)
	}
}

func (s *Server) runTurn(w http.ResponseWriter, r *http.Request) {
	var req runTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	exitAfterTurn := true
	if req.ExitAfterTurn != nil {
		exitAfterTurn = *req.ExitAfterTurn
	}
	if !exitAfterTurn {
		writeError(w, http.StatusBadRequest, "unsupported", "POST /v1/turns is one-shot and requires exit_after_turn=true")
		return
	}

	ctx := r.Context()
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       req.Harness,
		TurnHarness:   req.TurnHarness,
		BinaryPath:    req.BinaryPath,
		Args:          req.Args,
		WorkingDir:    req.WorkingDir,
		Env:           req.Env,
		Prompt:        req.Prompt,
		ExitAfterTurn: true,
		Cols:          req.Cols,
		Rows:          req.Rows,
		InputPolicy:   req.InputPolicy,
	})
	if err != nil && !errors.Is(err, harness.ErrTurnErrored) {
		writeRunTurnError(w, err)
		return
	}

	out := runTurnResponse{
		Turn:                    toTurnDTO(res.Turn),
		Session:                 toSessionDTO(res.Session),
		History:                 make([]turnDTO, 0, len(res.History)),
		ProcessStoppedAfterTurn: res.ProcessStoppedAfterTurn,
		WrapperStatus:           string(res.WrapperResult.Status),
		WrapperReason:           res.WrapperResult.Reason,
	}
	for _, t := range res.History {
		out.History = append(out.History, toTurnDTO(t))
	}
	if err != nil {
		out.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) openConv(w http.ResponseWriter, r *http.Request) {
	var req openRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	// Use a background context: chat.Open hands this to wrapper.Start,
	// which keeps it for the lifetime of the harness process. r.Context()
	// would cancel as soon as this handler returns.
	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:     req.Harness,
		BinaryPath:  req.BinaryPath,
		Args:        req.Args,
		WorkingDir:  req.WorkingDir,
		Env:         req.Env,
		Cols:        req.Cols,
		Rows:        req.Rows,
		Store:       memstore.New(),
		InputPolicy: req.InputPolicy,
	})
	if err != nil {
		writeChatError(w, err)
		return
	}
	entry := &convEntry{
		id:      conv.SessionID(),
		conv:    conv,
		fan:     newFanout(conv.Events()),
		tokens:  make(map[string]func()),
		harness: req.Harness,
	}
	s.mu.Lock()
	s.convs[entry.id] = entry
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, openResponse{ID: entry.id})
}

func (s *Server) listConvs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	out := make([]conversationSummary, 0, len(s.convs))
	for _, e := range s.convs {
		out = append(out, conversationSummary{
			ID:        e.id,
			Harness:   e.harness,
			SessionID: e.conv.SessionID(),
		})
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) closeConv(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	entry, ok := s.convs[id]
	if ok {
		delete(s.convs, id)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	entry.releaseAll()
	_ = entry.conv.Close(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) acquireControl(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.lookup(w, r)
	if !ok {
		return
	}
	release, err := entry.conv.AcquireControl(r.Context())
	if err != nil {
		writeChatError(w, err)
		return
	}
	tok := entry.acquireToken(release)
	writeJSON(w, http.StatusOK, controlResponse{Token: tok})
}

func (s *Server) releaseControl(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.lookup(w, r)
	if !ok {
		return
	}
	tok := r.PathValue("token")
	if !entry.releaseToken(tok) {
		writeError(w, http.StatusNotFound, "unknown_token", "token not held")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !entry.hasToken(req.Token) {
		writeError(w, http.StatusConflict, "no_control", "caller does not hold the control token")
		return
	}
	turnID, err := entry.conv.Send(r.Context(), req.Text)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sendResponse{TurnID: turnID})
}

func (s *Server) answerInput(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req answerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !entry.hasToken(req.Token) {
		writeError(w, http.StatusConflict, "no_control", "caller does not hold the control token")
		return
	}
	err := entry.conv.Answer(r.Context(), req.RequestID, chat.InputAnswer{OptionID: req.OptionID, Text: req.Text})
	if err != nil {
		writeChatError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.lookup(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flush", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub, unsub := entry.fan.subscribe()
	defer unsub()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-sub:
			if !open {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := enc.Encode(toEventDTO(ev)); err != nil {
				return
			}
			// json.Encoder.Encode writes a trailing \n; SSE needs \n\n.
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.lookup(w, r)
	if !ok {
		return
	}
	turns, err := entry.conv.History(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "history_failed", err.Error())
		return
	}
	out := historyResponse{Turns: make([]turnDTO, 0, len(turns))}
	for _, t := range turns {
		out.Turns = append(out.Turns, toTurnDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*convEntry, bool) {
	id := r.PathValue("id")
	s.mu.RLock()
	entry, ok := s.convs[id]
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return nil, false
	}
	return entry, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, Code: code})
}

func writeRunTurnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "canceled", err.Error())
	default:
		writeChatError(w, err)
	}
}

func writeChatError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chat.ErrInvalidOptions):
		writeError(w, http.StatusBadRequest, "invalid_options", err.Error())
	case errors.Is(err, chat.ErrUnknownHarness):
		writeError(w, http.StatusBadRequest, "unknown_harness", err.Error())
	case errors.Is(err, chat.ErrNoControl):
		writeError(w, http.StatusConflict, "no_control", err.Error())
	case errors.Is(err, chat.ErrTurnInFlight):
		writeError(w, http.StatusConflict, "turn_in_flight", err.Error())
	case errors.Is(err, chat.ErrInputPending):
		writeError(w, http.StatusConflict, "input_pending", err.Error())
	case errors.Is(err, chat.ErrNoInputPending):
		writeError(w, http.StatusConflict, "no_input_pending", err.Error())
	case errors.Is(err, chat.ErrStaleInputRequest):
		writeError(w, http.StatusConflict, "stale_input_request", err.Error())
	case errors.Is(err, chat.ErrUnknownOption):
		writeError(w, http.StatusBadRequest, "unknown_option", err.Error())
	case errors.Is(err, chat.ErrClosed):
		writeError(w, http.StatusGone, "closed", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
