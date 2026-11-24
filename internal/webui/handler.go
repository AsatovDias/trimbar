package webui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AsatovDias/trimbar/internal/storage"
)

//go:embed static/index.html
var indexHTML []byte

// Handler serves the embedded offline Web UI (/ui) and JSON telemetry APIs
// (/api/stats, /api/inspect) backed by the local SQLite repository.
type Handler struct {
	repo        *storage.Repository
	allowedHost string
	mux         *http.ServeMux
}

// NewHandler creates a local Web UI http.Handler backed by repo.
// By default it enforces DNS-rebinding protection, accepting only localhost / 127.0.0.1 / ::1 Host headers.
func NewHandler(repo *storage.Repository) *Handler {
	h := &Handler{
		repo: repo,
		mux:  http.NewServeMux(),
	}
	h.mux.HandleFunc("/ui", h.handleUI)
	h.mux.HandleFunc("/ui/", h.handleUI)
	h.mux.HandleFunc("/api/stats", h.handleStats)
	h.mux.HandleFunc("/api/inspect", h.handleInspect)
	return h
}

// SetAllowedHost optionally registers an explicit non-loopback bind host (e.g. a LAN IP)
// that is permitted alongside localhost / 127.0.0.1 / ::1. Wildcards (0.0.0.0, ::) are ignored.
func (h *Handler) SetAllowedHost(host string) {
	cleaned := strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if cleaned == "" || cleaned == "0.0.0.0" || cleaned == "::" {
		return
	}
	h.allowedHost = cleaned
}

// ServeHTTP enforces DNS-rebinding protection via the HTTP Host header before
// dispatching /ui, /api/stats, and /api/inspect requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isAllowedWebUIHost(r.Host, h.allowedHost) {
		writeJSONError(w, http.StatusForbidden, "forbidden: invalid Host header (DNS rebinding protection)")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func isAllowedWebUIHost(hostHeader, extraAllowed string) bool {
	raw := strings.TrimSpace(hostHeader)
	if raw == "" {
		return false
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if extraAllowed != "" && host == extraAllowed {
		return true
	}
	return false
}

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(indexHTML)
	}
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.repo == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "storage repository unavailable")
		return
	}

	filter, err := parseFilterFromQuery(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	stats, err := h.repo.GetStats(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("query stats: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.repo == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "storage repository unavailable")
		return
	}

	filter, err := parseFilterFromQuery(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if filter.Limit <= 0 {
		filter.Limit = 25
	}

	report, err := h.repo.GetInspect(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("query inspect: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func parseFilterFromQuery(r *http.Request) (storage.StatsFilter, error) {
	q := r.URL.Query()
	filter := storage.StatsFilter{
		Repo:      strings.TrimSpace(q.Get("repo")),
		Branch:    strings.TrimSpace(q.Get("branch")),
		SessionID: strings.TrimSpace(q.Get("session")),
	}

	if sinceStr := strings.TrimSpace(q.Get("since")); sinceStr != "" && !strings.EqualFold(sinceStr, "all") {
		since, err := parseSinceParam(sinceStr)
		if err != nil {
			return filter, err
		}
		filter.Since = since
	}

	if limitStr := strings.TrimSpace(q.Get("limit")); limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n <= 0 || n > 200 {
			return filter, fmt.Errorf("invalid limit %q (expected 1..200)", limitStr)
		}
		filter.Limit = n
	}
	return filter, nil
}

func parseSinceParam(s string) (time.Time, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if strings.HasSuffix(v, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || days <= 0 {
			return time.Time{}, fmt.Errorf("invalid since %q", s)
		}
		return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid since %q", s)
	}
	return time.Now().UTC().Add(-d), nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
