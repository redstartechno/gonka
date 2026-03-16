package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"subnet/state"
	"subnet/user"
)

// streamRelay dispatches SSE lines to the current request's writer.
// The proxy swaps the writer before each request. Since Session.SendInference
// serializes calls (holds mutex), only one request writes at a time.
type streamRelay struct {
	mu sync.Mutex
	w  io.Writer
}

func (r *streamRelay) callback(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		fmt.Fprintf(r.w, "%s\n\n", line)
		if f, ok := r.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (r *streamRelay) setWriter(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w = w
}

// Proxy is the OpenAI-compatible HTTP proxy backed by a subnet session.
type Proxy struct {
	session  *user.Session
	sm       *state.StateMachine
	escrowID string
	model    string
	relay    *streamRelay
	inflight atomic.Bool
}

type chatRequest struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens uint64 `json:"max_tokens"`
}

func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !p.inflight.CompareAndSwap(false, true) {
		http.Error(w, `{"error":{"message":"another inference is in flight","type":"rate_limit"}}`, http.StatusTooManyRequests)
		return
	}
	defer p.inflight.Store(false)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse request: "+err.Error(), http.StatusBadRequest)
		return
	}

	model := req.Model
	if model == "" {
		model = p.model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}

	params := user.InferenceParams{
		Model:       model,
		Prompt:      body,
		InputLength: uint64(len(body)),
		MaxTokens:   maxTokens,
		StartedAt:   time.Now().Unix(),
	}

	if req.Stream {
		p.handleStreaming(w, r, params)
	} else {
		p.handleNonStreaming(w, r, params)
	}
}

func (p *Proxy) handleStreaming(w http.ResponseWriter, r *http.Request, params user.InferenceParams) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	p.relay.setWriter(w)
	defer p.relay.setWriter(nil)

	_, err := p.session.SendInference(r.Context(), params)
	if err != nil {
		log.Printf("inference error: %v", err)
		fmt.Fprintf(w, "data: {\"error\":{\"message\":%q}}\n\n", err.Error())
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *Proxy) handleNonStreaming(w http.ResponseWriter, r *http.Request, params user.InferenceParams) {
	var buf bytes.Buffer
	p.relay.setWriter(&buf)
	defer p.relay.setWriter(nil)

	_, err := p.session.SendInference(r.Context(), params)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadGateway)
		return
	}

	assembled := assembleSSEChunks(buf.String())
	w.Header().Set("Content-Type", "application/json")
	w.Write(assembled)
}

// assembleSSEChunks extracts the last data line from SSE output as the response.
func assembleSSEChunks(raw string) []byte {
	var lastData string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		lastData = data
	}
	if lastData != "" {
		return []byte(lastData)
	}
	return []byte(`{"error":{"message":"no response data"}}`)
}

func (p *Proxy) handleFinalize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := p.session.Finalize(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}

	st := p.sm.SnapshotState()
	finalNonce := p.session.Nonce()
	payload, err := state.BuildSettlement(p.escrowID, st, p.session.Signatures()[finalNonce], finalNonce)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}

	data, err := marshalSettlement(payload)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

type statusResponse struct {
	EscrowID string `json:"escrow_id"`
	Nonce    uint64 `json:"nonce"`
	Phase    string `json:"phase"`
	Balance  uint64 `json:"balance"`
}

func (p *Proxy) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	phase := p.sm.Phase()
	var phaseStr string
	switch phase {
	case 0:
		phaseStr = "active"
	case 1:
		phaseStr = "finalizing"
	case 2:
		phaseStr = "settlement"
	default:
		phaseStr = fmt.Sprintf("unknown(%d)", phase)
	}

	st := p.sm.SnapshotState()
	resp := statusResponse{
		EscrowID: p.escrowID,
		Nonce:    p.session.Nonce(),
		Phase:    phaseStr,
		Balance:  st.Balance,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
