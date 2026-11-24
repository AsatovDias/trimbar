package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/AsatovDias/trimbar/internal/config"
)

// Server wraps the local Trimbar HTTP reverse-proxy server and routing.
type Server struct {
	cfg        *config.Config
	handler    *Handler
	httpServer *http.Server
}

// NewServer creates a configured Trimbar proxy Server.
func NewServer(cfg *config.Config, opts ...HandlerOption) (*Server, error) {
	if cfg == nil {
		cfg = config.Default()
	}

	h, err := NewHandler(cfg, opts...)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_trimbar/health", handleHealth)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if h.uiHandler != nil {
		mux.Handle("/ui", h.uiHandler)
		mux.Handle("/ui/", h.uiHandler)
		mux.Handle("/api/stats", h.uiHandler)
		mux.Handle("/api/inspect", h.uiHandler)
	}
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.uiHandler != nil && r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, "/ui", http.StatusFound)
			return
		}
		h.ServeHTTP(w, r)
	}))

	srv := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		// WriteTimeout MUST remain 0 so long-running Anthropic SSE streams are never terminated.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	return &Server{
		cfg:        cfg,
		handler:    h,
		httpServer: srv,
	}, nil
}

// Handler returns the underlying reverse-proxy Handler.
func (s *Server) Handler() *Handler {
	return s.handler
}

// ServeMux returns the root http.Handler (including /_trimbar/health).
func (s *Server) ServeMux() http.Handler {
	return s.httpServer.Handler
}

// Start binds to cfg.ListenAddr() and serves requests until ctx is cancelled or an error occurs.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr())
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve accepts incoming connections on ln and gracefully shuts down when ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
			return
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func defaultHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Preserve client Accept-Encoding and upstream wire bytes untouched.
		DisableCompression: true,
		// Do not time out slow extended-thinking responses before headers arrive.
		ResponseHeaderTimeout: 0,
	}
	return &http.Client{
		Transport: transport,
		// Never follow redirects automatically for an API reverse proxy.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
