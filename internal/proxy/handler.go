package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trimbar/trimbar/internal/anthropic"
	"github.com/trimbar/trimbar/internal/config"
	"github.com/trimbar/trimbar/internal/pricing"
)

const (
	maxErrorCaptureBytes = 4096
)

// ProxyEvent carries structured telemetry emitted after each proxied HTTP request completes.
type ProxyEvent struct {
	Timestamp     time.Time
	Method        string
	Path          string
	StatusCode    int
	Latency       time.Duration
	TTFB          time.Duration
	IsStreaming   bool
	ResponseBytes int64
	Inspection    *anthropic.InspectionResult
	StreamResult  *anthropic.StreamResult
	Cost          pricing.CostBreakdown
	ShadowSavings pricing.ShadowSavings
	ErrorBody     string
	UpstreamErr   error
}

// Handler is the transparent Anthropic reverse-proxy http.Handler.
type Handler struct {
	upstream   *url.URL
	client     *http.Client
	calculator *pricing.Calculator
	onComplete func(ProxyEvent)
}

// HandlerOption configures optional Handler behavior.
type HandlerOption func(*Handler)

// WithHTTPClient overrides the HTTP client used to call the upstream API.
func WithHTTPClient(client *http.Client) HandlerOption {
	return func(h *Handler) {
		if client != nil {
			h.client = client
		}
	}
}

// WithOnComplete registers a telemetry callback invoked when a request finishes.
func WithOnComplete(fn func(ProxyEvent)) HandlerOption {
	return func(h *Handler) {
		h.onComplete = fn
	}
}

// WithCalculator overrides the pricing calculator used to enrich telemetry events.
func WithCalculator(calc *pricing.Calculator) HandlerOption {
	return func(h *Handler) {
		if calc != nil {
			h.calculator = calc
		}
	}
}

// NewHandler constructs a transparent reverse-proxy Handler targeting cfg.UpstreamURL.
func NewHandler(cfg *config.Config, opts ...HandlerOption) (*Handler, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	u, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL %q: %w", cfg.UpstreamURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream URL %q must include scheme and host", cfg.UpstreamURL)
	}

	h := &Handler{
		upstream:   u,
		client:     defaultHTTPClient(),
		calculator: pricing.NewCalculator(cfg),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP transparently proxies the request to Anthropic, cloning and inspecting
// POST /v1/messages payloads while preserving all headers, streaming with zero buffering,
// and passing upstream errors (including 429 and 529) through verbatim.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	isMessages := r.Method == http.MethodPost && isMessagesPath(r.URL.Path)

	event := ProxyEvent{
		Timestamp: start,
		Method:    r.Method,
		Path:      r.URL.Path,
	}

	var outBody io.Reader = r.Body
	var contentLength = r.ContentLength

	var inspectDone chan struct{}
	var inspectRes *anthropic.InspectionResult

	if isMessages && r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			event.StatusCode = http.StatusBadRequest
			event.Latency = time.Since(start)
			event.UpstreamErr = err
			h.emit(event)
			return
		}
		_ = r.Body.Close()

		outBody = bytes.NewReader(bodyBytes)
		contentLength = int64(len(bodyBytes))

		// Inspect the cloned request payload concurrently so it adds 0ms to upstream TTFB.
		inspectDone = make(chan struct{})
		go func() {
			defer close(inspectDone)
			if res, err := anthropic.InspectRequest(bodyBytes); err == nil {
				inspectRes = res
			}
		}()
	}

	targetURL := buildUpstreamURL(h.upstream, r.URL)
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), outBody)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "proxy_error", "failed to build upstream request")
		event.StatusCode = http.StatusBadGateway
		event.Latency = time.Since(start)
		event.UpstreamErr = err
		if inspectDone != nil {
			<-inspectDone
			event.Inspection = inspectRes
		}
		h.emit(event)
		return
	}

	copyHeaders(outReq.Header, r.Header)
	outReq.Host = h.upstream.Host
	outReq.ContentLength = contentLength

	resp, err := h.client.Do(outReq)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream_unreachable", fmt.Sprintf("upstream request failed: %v", err))
		event.StatusCode = http.StatusBadGateway
		event.Latency = time.Since(start)
		event.UpstreamErr = err
		if inspectDone != nil {
			<-inspectDone
			event.Inspection = inspectRes
		}
		h.emit(event)
		return
	}
	defer resp.Body.Close()

	event.TTFB = time.Since(start)
	event.StatusCode = resp.StatusCode

	// Preserve all upstream response headers verbatim (including Retry-After, anthropic-ratelimit-*, request-id).
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	contentType := resp.Header.Get("Content-Type")
	contentEncoding := resp.Header.Get("Content-Encoding")

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && isSSEContentType(contentType):
		event.IsStreaming = true
		streamRes, n, streamErr := StreamSSE(w, resp.Body, contentEncoding)
		event.ResponseBytes = n
		event.StreamResult = &streamRes
		if streamErr != nil {
			event.UpstreamErr = streamErr
		}

	case resp.StatusCode >= 200 && resp.StatusCode < 300 && isMessages:
		// Non-streaming /v1/messages JSON response: tee body directly to client while capturing for usage parsing.
		var captured bytes.Buffer
		n, copyErr := io.Copy(w, io.TeeReader(resp.Body, &captured))
		event.ResponseBytes = n
		if copyErr != nil {
			event.UpstreamErr = copyErr
		}
		rawJSON := captured.Bytes()
		if strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") {
			if gz, err := gzip.NewReader(bytes.NewReader(rawJSON)); err == nil {
				if plain, err := io.ReadAll(gz); err == nil {
					rawJSON = plain
				}
				_ = gz.Close()
			}
		}
		if res, err := anthropic.ParseJSONResponse(rawJSON); err == nil {
			event.StreamResult = &res
		}

	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// Transparent error pass-through (429 Too Many Requests, 529 Overloaded, 4xx, 5xx).
		// Forward exact status, headers, and body bytes untouched while capturing a bounded snippet for telemetry.
		capBuf := &limitedBuffer{limit: maxErrorCaptureBytes}
		n, copyErr := io.Copy(w, io.TeeReader(resp.Body, capBuf))
		event.ResponseBytes = n
		event.ErrorBody = capBuf.String()
		if copyErr != nil {
			event.UpstreamErr = copyErr
		}

	default:
		// Auxiliary Claude Code endpoints (/v1/messages/count_tokens, /v1/models, etc.)
		n, copyErr := io.Copy(w, resp.Body)
		event.ResponseBytes = n
		if copyErr != nil {
			event.UpstreamErr = copyErr
		}
	}

	if inspectDone != nil {
		<-inspectDone
		event.Inspection = inspectRes
	}

	// Enrich telemetry with exact USD cost and honest shadow-savings calculation.
	if event.StreamResult != nil && h.calculator != nil {
		model := event.StreamResult.Model
		if model == "" && event.Inspection != nil {
			model = event.Inspection.Model
		}
		u := pricingUsageFromAnthropic(event.StreamResult.Usage)
		event.Cost = h.calculator.CalculateCost(model, u)
		if event.Inspection != nil && event.Inspection.TotalWastedEstTokens > 0 {
			event.ShadowSavings = h.calculator.CalculateShadowSavings(
				model,
				u,
				int64(event.Inspection.TotalWastedEstTokens),
			)
		}
	}

	event.Latency = time.Since(start)
	h.emit(event)
}

func (h *Handler) emit(ev ProxyEvent) {
	if h.onComplete != nil {
		h.onComplete(ev)
	}
}

func pricingUsageFromAnthropic(u anthropic.Usage) pricing.Usage {
	pu := pricing.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
	if u.CacheCreation != nil {
		pu.CacheCreation5mTokens = u.CacheCreation.Ephemeral5mInputTokens
		pu.CacheCreation1hTokens = u.CacheCreation.Ephemeral1hInputTokens
	}
	return pu
}

func isMessagesPath(p string) bool {
	return p == "/v1/messages" || strings.HasSuffix(p, "/v1/messages")
}

func isSSEContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
}

func buildUpstreamURL(base, reqURL *url.URL) *url.URL {
	out := *base
	out.Path = singleJoiningSlash(base.Path, reqURL.Path)
	out.RawPath = ""
	if base.RawPath != "" || reqURL.RawPath != "" {
		out.RawPath = singleJoiningSlash(base.EscapedPath(), reqURL.EscapedPath())
	}
	out.RawQuery = reqURL.RawQuery
	return &out
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		if a == "" {
			return "/" + b
		}
		return a + "/" + b
	}
	return a + b
}

// hopByHopHeaders lists RFC 7230 hop-by-hop headers stripped during proxying.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	// Identify any additional headers named in the Connection header.
	connHeaders := make(map[string]bool)
	for _, h := range src.Values("Connection") {
		for _, f := range strings.Split(h, ",") {
			if name := http.CanonicalHeaderKey(strings.TrimSpace(f)); name != "" {
				connHeaders[name] = true
			}
		}
	}

	for k, vv := range src {
		canon := http.CanonicalHeaderKey(k)
		if hopByHopHeaders[canon] || connHeaders[canon] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func writeProxyError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = l.buf.Write(p[:remaining])
		} else {
			_, _ = l.buf.Write(p)
		}
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	return l.buf.String()
}
