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

	"streaming/internal/autovolume"
	"streaming/internal/avcontrol"
	"streaming/internal/config"
	"streaming/internal/smp"
)

// The audio is read on this one schedule however many control pages are open:
// they are all answered from the last reading. The automatic gain is decided
// from the same readings, and writes at most once per autovolume.MinInterval.
const (
	audioPollInterval = 500 * time.Millisecond
	audioErrorBackoff = 3 * time.Second
)

type Server struct {
	store  *config.Store
	client *smp.Client
	pages  Pages

	mu     sync.Mutex
	state  smp.State
	http   *http.Server
	cancel context.CancelFunc

	audioMu  sync.Mutex
	audio    smp.AudioState
	audioAt  time.Time
	audioErr error
	auto     *autovolume.Controller
	autoNote string
	av       *avcontrol.Controller
}

type Pages struct {
	Control     []byte
	ConfigIndex []byte
	Config      []byte
	AVConfig    []byte
	AV          []byte
}

func New(store *config.Store, client *smp.Client, pages Pages) *Server {
	return &Server{
		store:  store,
		client: client,
		pages:  pages,
		state: smp.State{
			ActiveStream:  smp.ActiveNone,
			StatusMessage: "Idle",
		},
		auto:     autovolume.New(autovolume.DefaultSettings()),
		autoNote: autovolume.NoteOff,
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	for {
		cfg := s.store.Get()
		addr := fmt.Sprintf(":%d", cfg.HTTPPort)
		handler := s.routes()
		// No WriteTimeout: the preview is an endless response.
		httpServer := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		}

		s.mu.Lock()
		s.http = httpServer
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.mu.Unlock()

		s.startPoller(runCtx)
		s.startAudioPoller(runCtx)
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
			survive("status poll", func() {
				if state, polled := s.client.QueryState(cfg); polled {
					s.setState(state)
				}
			})

			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// survive keeps one bad poll from taking the whole server down. Nobody is
// watching the tablet, and the HTTP server already recovers its own handlers.
func survive(what string, poll func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s panicked: %v", what, r)
		}
	}()
	poll()
}

// The control API has no login, so without this any web page open on a
// device that can reach this server could start or stop a stream, or rewrite
// the configuration, by posting to it in the background. Reads stay open.
func (s *Server) routes() http.Handler {
	return http.NewCrossOriginProtection().Handler(s.mux())
}

func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleControl)
	mux.HandleFunc("/config", s.handleConfigIndex)
	mux.HandleFunc("/config/streaming", s.handleConfigPage)
	mux.HandleFunc("/config/av", s.handleAVConfigPage)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfigAPI)
	mux.HandleFunc("/api/config/test", s.handleConfigTest)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/audio", s.handleAudio)
	mux.HandleFunc("/api/audio/gain", s.handleAudioGain)
	mux.HandleFunc("/api/audio/mute", s.handleAudioMute)
	mux.HandleFunc("/api/audio/auto", s.handleAudioAuto)
	mux.HandleFunc("/api/stream/english", s.handleEnglish)
	mux.HandleFunc("/api/stream/mandarin", s.handleMandarin)
	mux.HandleFunc("/api/stream/stop", s.handleStop)
	mux.HandleFunc("/avcontrol", s.handleAVPage)
	mux.HandleFunc("/api/avcontrol/config", s.handleAVConfigAPI)
	mux.HandleFunc("/api/avcontrol/config/test", s.handleAVConfigTest)
	mux.HandleFunc("/api/avcontrol/status", s.handleAVStatus)
	mux.HandleFunc("/api/avcontrol/on", s.handleAVOn)
	mux.HandleFunc("/api/avcontrol/off", s.handleAVOff)
	mux.HandleFunc("/api/avcontrol/delay", s.handleAVDelay)
	return mux
}

func (s *Server) AttachAV(av *avcontrol.Controller) {
	s.av = av
}

func (s *Server) StreamActive() bool {
	return s.getState().Streaming
}

// handlePreview relays the SMP's own live preview to the browser. The unit
// serves it as a fragmented MP4 and keeps producing it whether or not anything
// is being streamed, so the picture does not depend on which language is live.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.store.Get()
	if !cfg.IsSmpConfigured() {
		http.Error(w, "SMP is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := s.client.Preview(r.Context(), cfg)
	if err != nil {
		http.Error(w, "preview unavailable", http.StatusBadGateway)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	copyStreaming(w, body)
}

// copyStreaming forwards the body a fragment at a time and flushes each one,
// so the picture starts as soon as the SMP sends it rather than when a buffer
// happens to fill.
func copyStreaming(w http.ResponseWriter, body io.Reader) {
	controller := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeHTML(w, s.pages.Control)
}

func (s *Server) handleConfigIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.pages.ConfigIndex) == 0 {
		http.NotFound(w, r)
		return
	}
	writeHTML(w, s.pages.ConfigIndex)
}

func (s *Server) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeHTML(w, s.pages.Config)
}

func (s *Server) handleAVConfigPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.pages.AVConfig) == 0 {
		http.NotFound(w, r)
		return
	}
	writeHTML(w, s.pages.AVConfig)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.statusPayload(s.getState()))
}

func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.writeAudioState(w)
}

type audioGainPayload struct {
	GainTenths int `json:"gainTenths"`
}

func (s *Server) handleAudioGain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload audioGainPayload
	if err := readJSON(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if payload.GainTenths < -180 || payload.GainTenths > 240 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "gain must be between -18.0 and +24.0 dB",
		})
		return
	}
	cfg := s.store.Get()
	if cfg.AutoVolume {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "turn off Auto Volume to set the volume by hand",
		})
		return
	}
	if err := s.client.SetAudioGain(cfg, payload.GainTenths); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.audioMu.Lock()
	s.audio.LeftGainTenths = payload.GainTenths
	s.audio.RightGainTenths = payload.GainTenths
	s.audioMu.Unlock()
	s.writeAudioState(w)
}

type audioMutePayload struct {
	Muted bool `json:"muted"`
}

func (s *Server) handleAudioMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload audioMutePayload
	if err := readJSON(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.client.SetAudioMute(s.store.Get(), payload.Muted); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.audioMu.Lock()
	s.audio.Muted = payload.Muted
	s.audioMu.Unlock()
	s.writeAudioState(w)
}

type audioAutoPayload struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleAudioAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload audioAutoPayload
	if err := readJSON(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	next := s.store.Get()
	next.AutoVolume = payload.Enabled
	if err := s.store.Save(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audioMu.Lock()
	s.auto.Reset()
	s.autoNote = autovolume.NoteOff
	if payload.Enabled {
		s.autoNote = autovolume.NoteWaiting
	}
	s.audioMu.Unlock()
	s.writeAudioState(w)
}

type audioPayload struct {
	smp.AudioState
	AutoVolume bool   `json:"autoVolume"`
	AutoNote   string `json:"autoNote"`
}

// writeAudioState answers from the last reading rather than asking the SMP,
// so a page polling twice a second adds nothing to the unit's load. A reading
// is never too old to show: polls are only skipped while a stream start holds
// the unit, and a read that hangs ends in an error rather than a gap.
func (s *Server) writeAudioState(w http.ResponseWriter) {
	autoVolume := s.store.Get().AutoVolume
	s.audioMu.Lock()
	state, at, err, note := s.audio, s.audioAt, s.audioErr, s.autoNote
	s.audioMu.Unlock()

	switch {
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	case at.IsZero():
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, audioPayload{AudioState: state, AutoVolume: autoVolume, AutoNote: note})
	}
}

func (s *Server) startAudioPoller(ctx context.Context) {
	go func() {
		for {
			wait := audioErrorBackoff
			survive("audio poll", func() {
				if s.pollAudio() {
					wait = audioPollInterval
				}
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

// pollAudio reads the SMP's audio once and, when Auto Volume is on, adjusts
// the gain from it. It reports false when the SMP could not be read or
// written, so the next attempt backs off rather than hammering the unit.
func (s *Server) pollAudio() bool {
	cfg := s.store.Get()
	state, polled, err := s.client.QueryAudio(cfg)
	if !polled {
		return true
	}
	if err != nil {
		s.audioMu.Lock()
		s.audioErr = err
		s.audioMu.Unlock()
		return false
	}
	current := (state.LeftGainTenths + state.RightGainTenths) / 2
	decision := s.recordAudio(cfg, state, current)

	if !decision.Change || !s.store.Get().AutoVolume {
		return true
	}
	applied, err := s.client.TrySetAudioGain(cfg, decision.GainTenths)
	if err != nil {
		log.Printf("auto volume: set gain to %d tenths: %v", decision.GainTenths, err)
		return false
	}
	if applied {
		meter := max(state.LeftLevelTenths, state.RightLevelTenths)
		log.Printf("auto volume: gain %d -> %d tenths, meter %d, %s",
			current, decision.GainTenths, meter, decision.Note)
		s.recordAutoGain(current, decision.GainTenths)
	}
	return true
}

func (s *Server) recordAudio(cfg config.Config, state smp.AudioState, current int) autovolume.Decision {
	now := time.Now()
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	s.audio, s.audioAt, s.audioErr = state, now, nil
	if !cfg.AutoVolume {
		return autovolume.Decision{}
	}
	decision := s.auto.Observe(autovolume.Reading{
		At:               now,
		LeftLevelTenths:  state.LeftLevelTenths,
		RightLevelTenths: state.RightLevelTenths,
		GainTenths:       current,
		Muted:            state.Muted,
	})
	s.autoNote = decision.Note
	return decision
}

func (s *Server) recordAutoGain(from, to int) {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	s.auto.Applied(time.Now(), from, to)
	s.audio.LeftGainTenths = to
	s.audio.RightGainTenths = to
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
	SmpHost     string `json:"smpHost"`
	SmpPort     int    `json:"smpPort"`
	SmpUsername string `json:"smpUsername"`
	SmpPassword string `json:"smpPassword"`
	HTTPPort    int    `json:"httpPort"`
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
	host, port, err := config.ParseHostPort(payload.SmpHost, payload.SmpPort)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	password, err := passwordFor(cfg, host, port, payload.SmpPassword)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg.SmpHost = host
	cfg.SmpPort = port
	cfg.SmpPassword = password
	if username := strings.TrimSpace(payload.SmpUsername); username != "" {
		cfg.SmpUsername = username
	}
	writeJSON(w, http.StatusOK, s.client.Diagnose(cfg))
}

// passwordFor reads an empty password box as "keep the saved password", but
// only while the address is the one it was saved for. Otherwise anyone who can
// reach this page could point the address at a machine of their own and
// collect the password from the next request sent there.
func passwordFor(saved config.Config, host string, port int, typed string) (string, error) {
	if typed != "" {
		return typed, nil
	}
	if saved.SmpPassword == "" {
		return "", nil
	}
	if !strings.EqualFold(saved.SmpHost, host) || saved.SmpPort != port {
		return "", errors.New("enter the SMP password again for the new address")
	}
	return saved.SmpPassword, nil
}

func readConfigPayload(r *http.Request) (configPayload, error) {
	var payload configPayload
	if err := readJSON(r, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func readJSON(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("invalid body")
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	payload, err := readConfigPayload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	current := s.store.Get()
	next := current
	host, port, err := config.ParseHostPort(payload.SmpHost, payload.SmpPort)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	password, err := passwordFor(current, host, port, payload.SmpPassword)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	next.SmpHost = host
	next.SmpPort = port
	next.SmpPassword = password
	next.SmpUsername = strings.TrimSpace(payload.SmpUsername)
	next.HTTPPort = payload.HTTPPort
	if next.HTTPPort == 0 {
		next.HTTPPort = config.DefaultHTTPPort
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
	if next.HTTPPort != current.HTTPPort {
		go s.restart()
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
	PreviewAvailable bool `json:"previewAvailable"`
}

func (s *Server) statusPayload(state smp.State) statusPayload {
	return statusPayload{
		State:            state,
		PreviewAvailable: s.store.Get().IsSmpConfigured(),
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
