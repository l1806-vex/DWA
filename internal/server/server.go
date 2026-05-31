package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"dwa/internal/client"
	"dwa/internal/convert"
)

const (
	modelID      = "deepseek-chat"
	modelDisplay = "deepseek-r1"
)

type sessionEntry struct {
	id    string
	msgID *int64
}

// Server holds runtime state for the proxy.
type Server struct {
	ds       *client.Client
	mu       sync.Mutex
	sessions map[string]sessionEntry
	mux      *http.ServeMux
}

func New(ds *client.Client) *Server {
	s := &Server{
		ds:       ds,
		sessions: map[string]sessionEntry{},
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("/v1/messages", s.handleMessages)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func readBody(r *http.Request) (map[string]any, error) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func convKey(messages []map[string]any) string {
	history := messages
	if len(messages) > 0 {
		history = messages[:len(messages)-1]
	}
	b, _ := json.Marshal(history)
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}

func (s *Server) getOrCreateSession(ctx context.Context, key string) (sessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.sessions[key]; ok {
		return e, nil
	}
	id, err := s.ds.CreateSession(ctx)
	if err != nil {
		return sessionEntry{}, err
	}
	e := sessionEntry{id: id}
	s.sessions[key] = e
	return e, nil
}

func (s *Server) updateSession(key, id string, msgID *int64) {
	s.mu.Lock()
	s.sessions[key] = sessionEntry{id: id, msgID: msgID}
	s.mu.Unlock()
}

// streamDeepSeek runs the DeepSeek stream and calls onChunk for each text delta.
func (s *Server) streamDeepSeek(
	ctx context.Context,
	messages []map[string]any,
	tools []map[string]any,
	thinking, search bool,
	key string,
	onChunk func(string),
) error {
	systemPrompt, userPrompt := convert.MessagesToPrompt(messages, tools)
	if userPrompt == "" {
		return fmt.Errorf("no user message found")
	}
	fullPrompt := userPrompt
	if systemPrompt != "" {
		fullPrompt = systemPrompt + "\n\n" + userPrompt
	}

	entry, err := s.getOrCreateSession(ctx, key)
	if err != nil {
		return err
	}

	chunks, errc := s.ds.Stream(ctx, entry.id, fullPrompt, entry.msgID, thinking, search)

	var lastMsgID *int64
	for chunk := range chunks {
		if chunk.MsgID >= 0 {
			id := chunk.MsgID
			lastMsgID = &id
		} else {
			onChunk(chunk.Text)
		}
	}

	if err := <-errc; err != nil {
		return err
	}

	s.updateSession(key, entry.id, lastMsgID)
	return nil
}

// ── /v1/models ────────────────────────────────────────────────────────────────

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	writeJSON(w, 200, map[string]any{
		"object": "list",
		"data": []any{
			map[string]any{"id": modelID, "object": "model", "created": now, "owned_by": "deepseek"},
			map[string]any{"id": modelDisplay, "object": "model", "created": now, "owned_by": "deepseek"},
		},
	})
}

// ── /v1/chat/completions ─────────────────────────────────────────────────────

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}

	messages := toMsgSlice(body["messages"])
	tools := toMsgSlice(body["tools"])
	stream, _ := body["stream"].(bool)
	thinking := strings.Contains(fmt.Sprint(body["model"]), "deepseek-reasoner")
	search, _ := body["search_enabled"].(bool)

	if len(messages) == 0 {
		writeJSON(w, 422, map[string]any{"error": "messages list is empty"})
		return
	}

	cid := fmt.Sprintf("chatcmpl-%x", time.Now().UnixNano())
	key := convKey(messages)

	if stream {
		s.openaiStream(w, r, messages, tools, thinking, search, cid, key)
		return
	}

	var fullText strings.Builder
	err = s.streamDeepSeek(r.Context(), messages, tools, thinking, search, key,
		func(t string) { fullText.WriteString(t) },
	)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	clean, calls := convert.ParseToolCalls(fullText.String())
	var callsArg []map[string]any
	if len(calls) > 0 {
		callsArg = calls
	}
	writeJSON(w, 200, convert.ResponseToOpenAI(clean, modelID, cid, callsArg))
}

func (s *Server) openaiStream(
	w http.ResponseWriter, r *http.Request,
	messages, tools []map[string]any,
	thinking, search bool,
	cid, key string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	var fullText strings.Builder
	err := s.streamDeepSeek(r.Context(), messages, tools, thinking, search, key,
		func(t string) {
			fullText.WriteString(t)
			ev, _ := json.Marshal(convert.ChunkToOpenAI(t, modelID, cid))
			fmt.Fprintf(w, "data: %s\n\n", ev)
			flusher.Flush()
		},
	)
	if err != nil {
		ev, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "server_error"}})
		fmt.Fprintf(w, "data: %s\n\n", ev)
		flusher.Flush()
		return
	}

	clean, calls := convert.ParseToolCalls(fullText.String())
	var callsArg []map[string]any
	if len(calls) > 0 {
		callsArg = calls
	}
	final, _ := json.Marshal(convert.FinishToOpenAI(clean, modelID, cid, callsArg))
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", final)
	flusher.Flush()
}

// ── /v1/messages ─────────────────────────────────────────────────────────────

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}

	rawMsgs := toMsgSlice(body["messages"])
	openaiMsgs := convert.AnthropicMessagesToOpenAI(rawMsgs)

	if sysContent, ok := body["system"].(string); ok && sysContent != "" {
		openaiMsgs = append([]map[string]any{{"role": "system", "content": sysContent}}, openaiMsgs...)
	}

	anthTools := toMsgSlice(body["tools"])
	openaiTools := make([]map[string]any, 0, len(anthTools))
	for _, t := range anthTools {
		openaiTools = append(openaiTools, convert.AnthropicToolToOpenAI(t))
	}

	stream, _ := body["stream"].(bool)
	thinkingMap, _ := body["thinking"].(map[string]any)
	thinking := fmt.Sprint(thinkingMap["type"]) == "enabled"
	msgID := fmt.Sprintf("msg_%x", time.Now().UnixNano())
	key := convKey(openaiMsgs)

	if len(openaiMsgs) == 0 {
		writeJSON(w, 422, map[string]any{"error": "messages list is empty"})
		return
	}

	if stream {
		s.anthropicStream(w, r, openaiMsgs, openaiTools, thinking, msgID, key)
		return
	}

	var fullText strings.Builder
	err = s.streamDeepSeek(r.Context(), openaiMsgs, openaiTools, thinking, false, key,
		func(t string) { fullText.WriteString(t) },
	)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	clean, calls := convert.ParseToolCalls(fullText.String())
	writeJSON(w, 200, convert.BuildAnthropicResponse(clean, calls, msgID))
}

func (s *Server) anthropicStream(
	w http.ResponseWriter, r *http.Request,
	messages, tools []map[string]any,
	thinking bool,
	msgID, key string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	sse := func(ev map[string]any) {
		etype, _ := ev["type"].(string)
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", etype, data)
		flusher.Flush()
	}

	for _, ev := range convert.StartToAnthropic(modelID, msgID) {
		sse(ev)
	}

	var fullText strings.Builder
	err := s.streamDeepSeek(r.Context(), messages, tools, thinking, false, key,
		func(t string) {
			fullText.WriteString(t)
			for _, ev := range convert.ChunkToAnthropic(t) {
				sse(ev)
			}
		},
	)
	if err != nil {
		sse(map[string]any{"type": "error", "error": map[string]any{"type": "server_error", "message": err.Error()}})
		return
	}

	clean, calls := convert.ParseToolCalls(fullText.String())
	_ = clean
	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}
	for _, ev := range convert.FinishToAnthropic(stopReason, calls) {
		sse(ev)
	}
}

// ── util ──────────────────────────────────────────────────────────────────────

func toMsgSlice(v any) []map[string]any {
	slice, _ := v.([]any)
	out := make([]map[string]any, 0, len(slice))
	for _, item := range slice {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
