package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trimbar/trimbar/internal/config"
)

// TestStreamingZeroBufferingLatency verifies that SSE chunks are forwarded immediately
// with <50ms proxy latency per chunk (no full-response buffering) and that async usage
// parsing captures exact Anthropic token & cache metrics.
func TestStreamingZeroBufferingLatency(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":50,\"cache_creation_input_tokens\":1000,\"cache_read_input_tokens\":12000,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"chunk-1\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"chunk-2\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":77}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	const interChunkPause = 80 * time.Millisecond
	sentAt := make([]time.Time, len(events))
	var sentMu sync.Mutex

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Upstream-Trace", "trace-sse-1")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter does not implement http.Flusher")
			return
		}
		flusher.Flush()

		for i, ev := range events {
			if i > 0 {
				time.Sleep(interChunkPause)
			}
			sentMu.Lock()
			sentAt[i] = time.Now()
			sentMu.Unlock()
			_, _ = io.WriteString(w, ev)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	var (
		gotEvent ProxyEvent
		eventCh  = make(chan ProxyEvent, 1)
	)
	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL

	srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
		eventCh <- ev
	}))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	reqBody := `{"model":"claude-sonnet-4-20250514","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream-Trace") != "trace-sse-1" {
		t.Errorf("X-Upstream-Trace = %q, want trace-sse-1", resp.Header.Get("X-Upstream-Trace"))
	}

	reader := bufio.NewReader(resp.Body)
	receivedAt := make([]time.Time, 0, len(events))
	var fullBody strings.Builder

	for len(receivedAt) < len(events) {
		var eventBuf strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				eventBuf.WriteString(line)
				fullBody.WriteString(line)
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("ReadString error: %v", err)
			}
			if line == "\n" {
				break
			}
		}
		if eventBuf.Len() == 0 {
			break
		}
		receivedAt = append(receivedAt, time.Now())
	}

	if len(receivedAt) != len(events) {
		t.Fatalf("received %d SSE events, want %d", len(receivedAt), len(events))
	}
	if fullBody.String() != strings.Join(events, "") {
		t.Errorf("forwarded SSE payload mismatch")
	}

	sentMu.Lock()
	defer sentMu.Unlock()

	// Verify per-chunk forwarding latency < 50ms AND zero full-response buffering.
	const maxAllowedLatency = 50 * time.Millisecond
	for i := range events {
		latency := receivedAt[i].Sub(sentAt[i])
		if latency >= maxAllowedLatency {
			t.Errorf("chunk %d proxy latency = %v, want < %v", i, latency, maxAllowedLatency)
		}
	}
	// Chunk 0 must arrive strictly before upstream even emits chunk 1 (proves no buffering).
	if !receivedAt[0].Before(sentAt[1]) {
		t.Errorf("chunk 0 arrived at %v after chunk 1 was sent at %v (proxy buffered stream!)",
			receivedAt[0], sentAt[1])
	}

	// Verify async telemetry captured exact usage & cost.
	select {
	case gotEvent = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for ProxyEvent telemetry callback")
	}

	if !gotEvent.IsStreaming {
		t.Errorf("expected IsStreaming=true")
	}
	if gotEvent.StreamResult == nil {
		t.Fatalf("StreamResult is nil")
	}
	u := gotEvent.StreamResult.Usage
	if u.InputTokens != 50 || u.CacheCreationInputTokens != 1000 || u.CacheReadInputTokens != 12000 || u.OutputTokens != 77 {
		t.Errorf("parsed SSE usage = %+v, want {in:50, cw:1000, cr:12000, out:77}", u)
	}
	if !gotEvent.Cost.PricingKnown || gotEvent.Cost.TotalCostUSD <= 0 {
		t.Errorf("expected valid calculated Cost, got %+v", gotEvent.Cost)
	}
}

// TestErrorPassThrough429And529 verifies that Anthropic 429 (Rate Limit) and 529 (Overloaded)
// errors are passed through verbatim: exact status code, Retry-After / ratelimit headers, and body.
func TestErrorPassThrough429And529(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		headers    map[string]string
		body       string
	}{
		{
			name:       "429 Too Many Requests",
			statusCode: http.StatusTooManyRequests,
			headers: map[string]string{
				"Content-Type":                        "application/json",
				"Retry-After":                         "42",
				"anthropic-ratelimit-requests-limit":  "50",
				"anthropic-ratelimit-tokens-remaining": "0",
				"request-id":                          "req_01RateLimit429",
			},
			body: `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit."}}`,
		},
		{
			name:       "529 Overloaded",
			statusCode: 529,
			headers: map[string]string{
				"Content-Type": "application/json",
				"Retry-After":  "10",
				"request-id":   "req_01Overloaded529",
			},
			body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			eventCh := make(chan ProxyEvent, 1)
			cfg := config.Default()
			cfg.UpstreamURL = upstream.URL
			srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
				eventCh <- ev
			}))
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			proxySrv := httptest.NewServer(srv.ServeMux())
			defer proxySrv.Close()

			resp, err := http.Post(
				proxySrv.URL+"/v1/messages",
				"application/json",
				strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
			)
			if err != nil {
				t.Fatalf("POST failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.statusCode {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tc.statusCode)
			}
			for k, want := range tc.headers {
				if got := resp.Header.Get(k); got != want {
					t.Errorf("header %q = %q, want %q", k, got, want)
				}
			}
			gotBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(gotBody) != tc.body {
				t.Errorf("body = %q, want %q", string(gotBody), tc.body)
			}

			select {
			case ev := <-eventCh:
				if ev.StatusCode != tc.statusCode {
					t.Errorf("telemetry StatusCode = %d, want %d", ev.StatusCode, tc.statusCode)
				}
				if ev.ErrorBody != tc.body {
					t.Errorf("telemetry ErrorBody = %q, want %q", ev.ErrorBody, tc.body)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for error telemetry")
			}
		})
	}
}

func TestHeaderPreservationAndAuxiliaryEndpoints(t *testing.T) {
	var (
		gotHeaders http.Header
		gotPath    string
		gotQuery   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"input_tokens":321}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		proxySrv.URL+"/v1/messages/count_tokens?beta=true",
		strings.NewReader(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}]}`),
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer oauth-max-token-xyz")
	req.Header.Set("x-api-key", "sk-ant-preserved")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31,extended-cache-ttl-2025-04-11")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/messages/count_tokens" || gotQuery != "beta=true" {
		t.Errorf("upstream target = %s?%s, want /v1/messages/count_tokens?beta=true", gotPath, gotQuery)
	}
	for k, want := range map[string]string{
		"Authorization":     "Bearer oauth-max-token-xyz",
		"X-Api-Key":         "sk-ant-preserved",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "prompt-caching-2024-07-31,extended-cache-ttl-2025-04-11",
	} {
		if got := gotHeaders.Get(k); got != want {
			t.Errorf("upstream header %q = %q, want %q", k, got, want)
		}
	}

	// Verify /_trimbar/health endpoint responds locally without hitting upstream.
	healthResp, err := http.Get(proxySrv.URL + "/_trimbar/health")
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	defer healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("health StatusCode = %d, want 200", healthResp.StatusCode)
	}
}

func TestGzipSSETransparencyAndParsing(t *testing.T) {
	ssePayload := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_gz","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":15,"cache_read_input_tokens":800}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(gzw, ssePayload)
	_ = gzw.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed.Bytes())
	}))
	defer upstream.Close()

	eventCh := make(chan ProxyEvent, 1)
	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
		eventCh <- ev
	}))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	// Verify client received valid gzip bytes matching upstream payload.
	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gzr.Close()
	gotPlain, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatalf("ReadAll gzip: %v", err)
	}
	if string(gotPlain) != ssePayload {
		t.Errorf("decompressed body mismatch")
	}

	select {
	case ev := <-eventCh:
		if ev.StreamResult == nil {
			t.Fatalf("StreamResult is nil")
		}
		if ev.StreamResult.Usage.CacheReadInputTokens != 800 || ev.StreamResult.Usage.OutputTokens != 20 {
			t.Errorf("gzip SSE parsed usage = %+v, want {cr:800, out:20}", ev.StreamResult.Usage)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for gzip SSE telemetry")
	}
}

func TestNonStreamingMessagesResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_nonstream_1",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-haiku-4-5-20251001",
			"stop_reason": "end_turn",
			"content":     []map[string]string{{"type": "text", "text": "Summary"}},
			"usage": map[string]int64{
				"input_tokens":               100,
				"cache_read_input_tokens":    400,
				"output_tokens":              30,
			},
		})
	}))
	defer upstream.Close()

	eventCh := make(chan ProxyEvent, 1)
	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
		eventCh <- ev
	}))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	resp, err := http.Post(
		proxySrv.URL+"/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"claude-haiku-4-5-20251001","max_tokens":256,"stream":false,"messages":[{"role":"user","content":"summarize"}]}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	select {
	case ev := <-eventCh:
		if ev.IsStreaming {
			t.Errorf("expected IsStreaming=false")
		}
		if ev.StreamResult == nil || ev.StreamResult.Usage.InputTokens != 100 || ev.StreamResult.Usage.OutputTokens != 30 {
			t.Errorf("unexpected non-streaming StreamResult: %+v", ev.StreamResult)
		}
		if ev.Inspection == nil || ev.Inspection.AgentRole != "subagent" {
			t.Errorf("expected AgentRole=subagent, got %+v", ev.Inspection)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for non-streaming telemetry")
	}
}

func TestActiveTrimmingUpdatesOutboundBodyAndContentLength(t *testing.T) {
	fileBody := strings.Repeat("package main // repeated file body\n", 40)
	reqPayload := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 2048,
		"stream":     false,
		"messages": []map[string]any{
			{"role": "user", "content": "check main.go"},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r1", "name": "Read", "input": map[string]string{"file_path": "main.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r1", "content": fileBody, "cache_control": map[string]string{"type": "ephemeral"}},
				},
			},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r2", "name": "Read", "input": map[string]string{"file_path": "main.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r2", "content": fileBody},
				},
			},
		},
	}
	origBytes, _ := json.Marshal(reqPayload)

	var (
		gotUpstreamBody []byte
		gotContentLen   int64
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLen = r.ContentLength
		gotUpstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_trim_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"cache_read_input_tokens":500,"output_tokens":20}}`)
	}))
	defer upstream.Close()

	eventCh := make(chan ProxyEvent, 1)
	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	cfg.TrimEnabled = true

	srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
		eventCh <- ev
	}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", bytes.NewReader(origBytes))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if len(gotUpstreamBody) >= len(origBytes) {
		t.Errorf("expected trimmed upstream body (%d bytes) < original (%d bytes)", len(gotUpstreamBody), len(origBytes))
	}
	if gotContentLen != int64(len(gotUpstreamBody)) {
		t.Errorf("upstream Content-Length = %d, want %d", gotContentLen, len(gotUpstreamBody))
	}
	if !strings.Contains(string(gotUpstreamBody), "[Trimbar: identical content of main.go already loaded earlier in context]") {
		t.Errorf("expected duplicate Read placeholder in trimmed upstream payload")
	}

	select {
	case ev := <-eventCh:
		if ev.TrimResult == nil || !ev.TrimResult.Modified || ev.TrimResult.TrimmedTokens <= 0 {
			t.Errorf("expected populated TrimResult, got %+v", ev.TrimResult)
		}
		if ev.TrimmedSavingsUSD <= 0 {
			t.Errorf("expected TrimmedSavingsUSD > 0, got %v", ev.TrimmedSavingsUSD)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for telemetry event")
	}
}

func TestAutoRetryOn429And529WithBackoff(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		switch n {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"try again"}}`)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}
	}))
	defer upstream.Close()

	var (
		retryMu sync.Mutex
		retries []RetryEvent
	)
	eventCh := make(chan ProxyEvent, 1)

	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	cfg.MaxRetries = 2
	cfg.RetryBaseDelay = 15 * time.Millisecond

	srv, err := NewServer(cfg,
		WithOnRetry(func(re RetryEvent) {
			retryMu.Lock()
			retries = append(retries, re)
			retryMu.Unlock()
		}),
		WithOnComplete(func(ev ProxyEvent) {
			eventCh <- ev
		}),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	resp, err := http.Post(
		proxySrv.URL+"/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final StatusCode = %d, want 200 after retries", resp.StatusCode)
	}
	if attempts.Load() != 3 {
		t.Errorf("upstream attempts = %d, want 3", attempts.Load())
	}

	retryMu.Lock()
	if len(retries) != 2 {
		t.Fatalf("len(retries) = %d, want 2", len(retries))
	}
	if retries[0].StatusCode != 429 || retries[0].Delay != 15*time.Millisecond {
		t.Errorf("retry[0] = %+v, want status=429 delay=15ms", retries[0])
	}
	if retries[1].StatusCode != 529 || retries[1].Delay != 30*time.Millisecond {
		t.Errorf("retry[1] = %+v, want status=529 delay=30ms", retries[1])
	}
	retryMu.Unlock()

	select {
	case ev := <-eventCh:
		if ev.RetryCount != 2 || ev.FailoverUsed {
			t.Errorf("telemetry retry/failover = (%d, %v), want (2, false)", ev.RetryCount, ev.FailoverUsed)
		}
		if ev.StreamResult == nil || ev.StreamResult.Usage.OutputTokens != 12 {
			t.Errorf("expected parsed SSE result after retry, got %+v", ev.StreamResult)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for telemetry")
	}
}

func TestFailoverToFallbackUpstreamAndAPIKey(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"primary overloaded"}}`)
	}))
	defer primary.Close()

	var gotFallbackKey string
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFallbackKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_fb_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","usage":{"input_tokens":30,"output_tokens":15}}`)
	}))
	defer fallback.Close()

	eventCh := make(chan ProxyEvent, 1)
	cfg := config.Default()
	cfg.UpstreamURL = primary.URL
	cfg.FallbackUpstream = fallback.URL
	cfg.FallbackAPIKey = "sk-ant-backup-key"
	cfg.MaxRetries = 2

	srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
		eventCh <- ev
	}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	req, _ := http.NewRequest(
		http.MethodPost,
		proxySrv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-primary-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200 from fallback", resp.StatusCode)
	}
	if gotFallbackKey != "sk-ant-backup-key" {
		t.Errorf("fallback x-api-key = %q, want sk-ant-backup-key", gotFallbackKey)
	}

	select {
	case ev := <-eventCh:
		if !ev.FailoverUsed || ev.RetryCount != 1 {
			t.Errorf("telemetry failover/retry = (%v, %d), want (true, 1)", ev.FailoverUsed, ev.RetryCount)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for failover telemetry")
	}
}

func TestRetryExhaustedReturns429Verbatim(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"still limited"}}`)
	}))
	defer upstream.Close()

	eventCh := make(chan ProxyEvent, 1)
	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	cfg.MaxRetries = 2
	cfg.RetryBaseDelay = 5 * time.Millisecond

	srv, err := NewServer(cfg, WithOnComplete(func(ev ProxyEvent) {
		eventCh <- ev
	}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	resp, err := http.Post(
		proxySrv.URL+"/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", resp.StatusCode)
	}
	if !strings.Contains(string(body), "still limited") {
		t.Errorf("body = %q, want verbatim 429 error", string(body))
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (1 initial + 2 retries)", attempts.Load())
	}

	select {
	case ev := <-eventCh:
		if ev.RetryCount != 2 || ev.StatusCode != 429 {
			t.Errorf("telemetry = (retries=%d, status=%d), want (2, 429)", ev.RetryCount, ev.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for exhausted retry telemetry")
	}
}
