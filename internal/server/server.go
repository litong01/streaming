package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"streaming/internal/config"
	"streaming/internal/preview"
	"streaming/internal/smp"
)

type Server struct {
	store   *config.Store
	client  *smp.Client
	preview *preview.Manager
	pages   Pages

	mu     sync.Mutex
	state  smp.State
	http   *http.Server
	cancel context.CancelFunc
}

type Pages struct {
	Control []byte
	Config  []byte
}

func New(store *config.Store, client *smp.Client, pages Pages) *Server {
	return &Server{
		store:   store,
		client:  client,
		preview: preview.New(),
		pages:   pages,
		state: smp.State{
			ActiveStream:  smp.ActiveNone,
			StatusMessage: "Idle",
		},
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	for {
		cfg := s.store.Get()
		addr := fmt.Sprintf(":%d", cfg.HTTPPort)
		handler := s.routes()
		httpServer := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		}

		s.mu.Lock()
		s.http = httpServer
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.mu.Unlock()

		s.startPoller(runCtx)
		s.preview.Sync(cfg)
		log.Printf("listening on http://0.0.0.0:%d/", cfg.HTTPPort)

		errCh := make(chan error, 1)
		go func() {
			errCh <- httpServer.ListenAndServe()
		}()

		select {
		case <-ctx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer shutdownCancel()
			_ = httpServer.Shutdown(shutdownCtx)
			s.preview.Stop()
			return ctx.Err()
		case <-runCtx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = httpServer.Shutdown(shutdownCtx)
			shutdownCancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		}
	}
}

func (s *Server) restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Server) startPoller(ctx context.Context) {
	go func() {
		for {
			cfg := s.store.Get()
			interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
			if interval <= 0 {
				interval = 3 * time.Second
			}
			state := s.client.QueryState(cfg)
			s.setState(state)

			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleControl)
	mux.HandleFunc("/config", s.handleConfigPage)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfigAPI)
	mux.HandleFunc("/api/config/test", s.handleConfigTest)
	mux.HandleFunc("/api/stream/english", s.handleEnglish)
	mux.HandleFunc("/api/stream/mandarin", s.handleMandarin)
	mux.HandleFunc("/api/stream/stop", s.handleStop)
	return mux
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeHTML(w, s.pages.Control)
}

func (s *Server) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	writeHTML(w, s.pages.Config)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.statusPayload(s.getState()))
}

func (s *Server) handleEnglish(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(cfg config.Config) smp.State {
		return s.client.StartEnglish(cfg)
	})
}

func (s *Server) handleMandarin(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(cfg config.Config) smp.State {
		return s.client.StartMandarin(cfg)
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(cfg config.Config) smp.State {
		current := s.getState()
		if !current.StreamEnabled {
			return current
		}
		return s.client.Stop(cfg)
	})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, action func(config.Config) smp.State) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := action(s.store.Get())
	s.setState(state)
	writeJSON(w, http.StatusOK, s.statusPayload(state))
}

func (s *Server) handleConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.Public())
	case http.MethodPost:
		s.saveConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type configPayload struct {
	SmpHost        string `json:"smpHost"`
	SmpSSHPort     int    `json:"smpSshPort"`
	SmpUsername    string `json:"smpUsername"`
	SmpPassword    string `json:"smpPassword"`
	HTTPPort       int    `json:"httpPort"`
	EnglishPreset  int    `json:"englishPreset"`
	MandarinPreset int    `json:"mandarinPreset"`
	PreviewURL     string `json:"previewUrl"`
	Go2rtcPort     int    `json:"go2rtcPort"`
}

// handleConfigTest probes the SMP with the values currently in the form so a
// connection can be checked before it is saved. It stores nothing.
func (s *Server) handleConfigTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	payload, err := readConfigPayload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg := s.store.Get()
	host, sshPort, err := config.ParseHostPort(payload.SmpHost, payload.SmpSSHPort)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg.SmpHost = host
	cfg.SmpSSHPort = sshPort
	if username := strings.TrimSpace(payload.SmpUsername); username != "" {
		cfg.SmpUsername = username
	}
	// An empty box means "keep the saved password", matching the save path.
	if payload.SmpPassword != "" {
		cfg.SmpPassword = payload.SmpPassword
	}
	writeJSON(w, http.StatusOK, s.client.Diagnose(cfg))
}

func readConfigPayload(r *http.Request) (configPayload, error) {
	var payload configPayload
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return payload, fmt.Errorf("invalid body")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return payload, fmt.Errorf("invalid JSON")
	}
	return payload, nil
}

func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	payload, err := readConfigPayload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	current := s.store.Get()
	next := current
	host, sshPort, err := config.ParseHostPort(payload.SmpHost, payload.SmpSSHPort)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	next.SmpHost = host
	next.SmpSSHPort = sshPort
	next.SmpUsername = strings.TrimSpace(payload.SmpUsername)
	if payload.SmpPassword != "" {
		next.SmpPassword = payload.SmpPassword
	}
	next.HTTPPort = payload.HTTPPort
	if next.HTTPPort == 0 {
		next.HTTPPort = config.DefaultHTTPPort
	}
	next.StreamIndex = config.DefaultStreamIndex
	next.EnglishPreset = payload.EnglishPreset
	next.MandarinPreset = payload.MandarinPreset
	next.PreviewURL = strings.TrimSpace(payload.PreviewURL)
	next.Go2rtcPort = payload.Go2rtcPort
	if next.Go2rtcPort == 0 {
		next.Go2rtcPort = config.DefaultGo2rtcPort
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := s.store.Save(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "httpPort": next.HTTPPort})
	previewChanged := next.PreviewURL != current.PreviewURL || next.Go2rtcPort != current.Go2rtcPort
	if next.HTTPPort != current.HTTPPort {
		go s.restart()
	} else if previewChanged {
		go s.preview.Sync(next)
	}
}

func (s *Server) getState() smp.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Server) setState(state smp.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

type statusPayload struct {
	smp.State
	PreviewReady      bool `json:"previewReady"`
	PreviewConfigured bool `json:"previewConfigured"`
	Go2rtcPort        int  `json:"go2rtcPort"`
	Go2rtcAvailable   bool `json:"go2rtcAvailable"`
}

func (s *Server) statusPayload(state smp.State) statusPayload {
	cfg := s.store.Get()
	return statusPayload{
		State:             state,
		PreviewReady:      s.preview.Ready(),
		PreviewConfigured: strings.TrimSpace(cfg.PreviewURL) != "",
		Go2rtcPort:        cfg.Go2rtcPort,
		Go2rtcAvailable:   s.preview.Available(),
	}
}

func writeHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
