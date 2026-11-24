package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trimbar/trimbar/internal/anthropic"
	"github.com/trimbar/trimbar/internal/config"
	"github.com/trimbar/trimbar/internal/pricing"
	"github.com/trimbar/trimbar/internal/trimmer"
)

const (
	maxErrorCaptureBytes = 4096
)

// RetryEvent describes a retry or failover triggered before SSE streaming starts.
type RetryEvent struct {
	Attempt    int
	StatusCode int
	Delay      time.Duration
	Failover   bool
	TargetURL  string
}

// ProxyEvent carries structured telemetry emitted after each proxied HTTP request completes.
type ProxyEvent struct {
	Timestamp         time.Time
	Method            string
	Path              string
	StatusCode        int
	Latency           time.Duration
	TTFB              time.Duration
	IsStreaming       bool
	ResponseBytes     int64
	Inspection        *anthropic.InspectionResult
	StreamResult      *anthropic.StreamResult
	TrimResult        *trimmer.TrimResult
	Cost              pricing.CostBreakdown
	ShadowSavings     pricing.ShadowSavings
	TrimmedSavingsUSD float64
	RetryCount        int
	FailoverUsed      bool
	ErrorBody         string
	UpstreamErr       error
}

// Handler is the transparent Anthropic reverse-proxy http.Handler.
type Handler struct {
	upstream         *url.URL
	fallbackUpstream *url.URL
	fallbackAPIKey   string
	maxRetries       int
	retryBaseDelay   time.Duration
	maxRetryAfter    time.Duration
	client           *http.Client
	calculator       *pricing.Calculator
	trimEnabled      bool
	onComplete       func(ProxyEvent)
	onRetry          func(RetryEvent)
	uiHandler        http.Handler
}

// HandlerOption configures optional Handler behavior.
type HandlerOption func(*Handler)

// WithOnRetry registers a callback invoked whenever a 429/529 retry or failover fires.
func WithOnRetry(fn func(RetryEvent)) HandlerOption {
	return func(h *Handler) {
		h.onRetry = fn
	}
}

// WithWebUI attaches the local offline Web UI handler (/ui, /api/stats, /api/inspect) to Server.
func WithWebUI(ui http.Handler) HandlerOption {
	return func(h *Handler) {
		h.uiHandler = ui
	}
}

// WithTrimEnabled enables or disables cache-safe active request trimming.
func WithTrimEnabled(enabled bool) HandlerOption {
	return func(h *Handler) {
		h.trimEnabled = enabled
	}
}

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

	var fbURL *url.URL
	if strings.TrimSpace(cfg.FallbackUpstream) != "" {
		parsedFB, err := url.Parse(cfg.FallbackUpstream)
		if err != nil {
			return nil, fmt.Errorf("parse fallback upstream URL %q: %w", cfg.FallbackUpstream, err)
		}
		if parsedFB.Scheme == "" || parsedFB.Host == "" {
			return nil, fmt.Errorf("fallback upstream URL %q must include scheme and host", cfg.FallbackUpstream)
		}
		fbURL = parsedFB
	}

	retryBase := cfg.RetryBaseDelay
	if retryBase <= 0 {
		retryBase = config.DefaultRetryBaseDelay
	}
	maxRetryAfter := cfg.MaxRetryAfter
	if maxRetryAfter <= 0 {
		maxRetryAfter = config.DefaultMaxRetryAfter
	}

	h := &Handler{
		upstream:         u,
		fallbackUpstream: fbURL,
		fallbackAPIKey:   cfg.FallbackAPIKey,
		maxRetries:       cfg.MaxRetries,
		retryBaseDelay:   retryBase,
		maxRetryAfter:    maxRetryAfter,
		client:           defaultHTTPClient(),
		calculator:       pricing.NewCalculator(cfg),
		trimEnabled:      cfg.TrimEnabled,
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

	var (
		outboundBytes []byte
		replayable    bool
		outBody       io.Reader = r.Body
		contentLength           = r.ContentLength
	)

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

		outboundBytes = bodyBytes
		if h.trimEnabled {
			if tr, err := trimmer.TrimRequest(bodyBytes); err == nil {
				trCopy := tr
				event.TrimResult = &trCopy
				if tr.Modified {
					outboundBytes = tr.Body
				}
			}
		}

		replayable = true
		outBody = bytes.NewReader(outboundBytes)
		contentLength = int64(len(outboundBytes))

		// Inspect the original cloned request payload concurrently so it adds 0ms to upstream TTFB.
		inspectDone = make(chan struct{})
		go func() {
			defer close(inspectDone)
			if res, err := anthropic.InspectRequest(bodyBytes); err == nil {
				inspectRes = res
			}
		}()
	} else if r.Body == nil || r.ContentLength == 0 {
		replayable = true
	}

	resp, retryCount, failoverUsed, err := h.doUpstreamWithRetry(r, isMessages, replayable, outboundBytes, outBody, contentLength)
	event.RetryCount = retryCount
	event.FailoverUsed = failoverUsed
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

	// Enrich telemetry with exact USD cost, honest shadow savings, and active trim savings.
	if h.calculator != nil {
		var model string
		if event.StreamResult != nil && event.StreamResult.Model != "" {
			model = event.StreamResult.Model
		} else if event.Inspection != nil {
			model = event.Inspection.Model
		}

		if event.StreamResult != nil {
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
		if event.TrimResult != nil && event.TrimResult.TrimmedTokens > 0 {
			event.TrimmedSavingsUSD = h.calculator.CalculateTrimmedSavings(model, int64(event.TrimResult.TrimmedTokens))
		}
	}

	event.Latency = time.Since(start)
	h.emit(event)
}

func (h *Handler) emit(ev ProxyEvent) {
	if h.onComplete == nil || !shouldLogTelemetryPath(ev.Path) {
		return
	}
	// Defensive guard: a failing telemetry sink (e.g. read-only SQLite) must never
	// crash the proxy handler after the user's response has been delivered.
	defer func() {
		_ = recover()
	}()
	h.onComplete(ev)
}

// shouldLogTelemetryPath ensures only real Anthropic API calls (/v1/*) are logged
// and persisted to SQLite, filtering out browser noise (/, /favicon.ico, /ui/*).
func shouldLogTelemetryPath(p string) bool {
	return strings.HasPrefix(p, "/v1/")
}

func (h *Handler) hasFallback() bool {
	return h.fallbackUpstream != nil || strings.TrimSpace(h.fallbackAPIKey) != ""
}

// doUpstreamWithRetry executes the upstream request and safely retries up to maxRetries
// times (before SSE streaming begins) on HTTP 429 or 529, respecting Retry-After or
// exponential backoff (1s -> 2s), or failing over to fallback_upstream / fallback_api_key.
func (h *Handler) doUpstreamWithRetry(
	r *http.Request,
	isMessages bool,
	replayable bool,
	outboundBytes []byte,
	initialBody io.Reader,
	contentLength int64,
) (*http.Response, int, bool, error) {
	maxAttempts := 1
	if replayable && h.maxRetries > 0 {
		maxAttempts += h.maxRetries
	}

	var (
		retryCount   int
		failoverUsed bool
		useFallback  bool
	)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		base := h.upstream
		if useFallback && h.fallbackUpstream != nil {
			base = h.fallbackUpstream
		}

		var bodyReader io.Reader
		if attempt == 0 && !replayable {
			bodyReader = initialBody
		} else if len(outboundBytes) > 0 {
			bodyReader = bytes.NewReader(outboundBytes)
		}

		targetURL := buildUpstreamURL(base, r.URL)
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bodyReader)
		if err != nil {
			return nil, retryCount, failoverUsed, err
		}

		copyHeaders(outReq.Header, r.Header)
		if isMessages {
			outReq.Header.Del("Content-Length")
		}
		if useFallback && strings.TrimSpace(h.fallbackAPIKey) != "" {
			outReq.Header.Set("x-api-key", h.fallbackAPIKey)
		}
		outReq.Host = base.Host
		outReq.ContentLength = contentLength

		resp, err := h.client.Do(outReq)
		if err != nil {
			// On transport error, fail over immediately if fallback is configured and not yet attempted.
			if replayable && h.hasFallback() && !useFallback && attempt+1 < maxAttempts {
				useFallback = true
				failoverUsed = true
				retryCount++
				h.emitRetry(RetryEvent{
					Attempt:    retryCount,
					StatusCode: 0,
					Failover:   true,
					TargetURL:  h.fallbackTargetString(),
				})
				continue
			}
			return nil, retryCount, failoverUsed, err
		}

		// Non-retryable status or final attempt reached: return upstream response verbatim.
		if !isRetryableStatus(resp.StatusCode) || !replayable || attempt+1 >= maxAttempts {
			return resp, retryCount, failoverUsed, nil
		}

		// If a fallback upstream/key is configured and hasn't been tried yet, fail over immediately.
		if h.hasFallback() && !useFallback {
			status := resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			useFallback = true
			failoverUsed = true
			retryCount++
			h.emitRetry(RetryEvent{
				Attempt:    retryCount,
				StatusCode: status,
				Failover:   true,
				TargetURL:  h.fallbackTargetString(),
			})
			continue
		}

		// Determine pause duration from Retry-After header or exponential backoff (1s -> 2s).
		delay, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		if hasRetryAfter {
			if delay > h.maxRetryAfter {
				// Long cooldown requested by upstream: pass 429/529 through immediately instead of stalling.
				return resp, retryCount, failoverUsed, nil
			}
		} else {
			delay = h.retryBaseDelay << attempt
		}

		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		retryCount++
		h.emitRetry(RetryEvent{
			Attempt:    retryCount,
			StatusCode: status,
			Delay:      delay,
			Failover:   useFallback,
			TargetURL:  base.String(),
		})

		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return nil, retryCount, failoverUsed, r.Context().Err()
			case <-timer.C:
			}
		}
	}

	return nil, retryCount, failoverUsed, fmt.Errorf("exhausted retry attempts")
}

func (h *Handler) emitRetry(ev RetryEvent) {
	if h.onRetry != nil {
		h.onRetry(ev)
	}
}

func (h *Handler) fallbackTargetString() string {
	if h.fallbackUpstream != nil {
		return h.fallbackUpstream.String()
	}
	return h.upstream.String() + " (fallback api key)"
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == 529
}

// parseRetryAfter parses an HTTP Retry-After header (seconds or HTTP-date).
func parseRetryAfter(val string) (time.Duration, bool) {
	v := strings.TrimSpace(val)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if ts, err := http.ParseTime(v); err == nil {
		d := time.Until(ts)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
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
