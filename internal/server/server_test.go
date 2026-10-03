package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streaming/internal/config"
	"streaming/internal/smp"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return New(store, smp.New(), Pages{})
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func put(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)))
	return rec
}

func TestCrossSitePostsAreRefused(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/stream/stop", nil)
	req.Host = "192.0.2.5:8080"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/audio/auto", strings.NewReader(`{"enabled":true}`))
	req.Host = "192.0.2.5:8080"
	req.Header.Set("Origin", "http://192.0.2.5:8080")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("refused the control page's own request")
	}
}

func TestSavedPasswordIsOnlyReusedForTheSameSMP(t *testing.T) {
	s := newTestServer(t)
	cfg := s.store.Get()
	cfg.SmpHost, cfg.SmpPort, cfg.SmpUsername, cfg.SmpPassword = "192.0.2.10", 443, "admin", "secret"
	if err := s.store.Save(cfg); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"smpHost":"192.0.2.99","smpPort":443,"smpUsername":"admin","httpPort":8080}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("moved the saved password to a new address: status %d", rec.Code)
	}
	if s.store.Get().SmpHost != "192.0.2.10" {
		t.Fatal("configuration changed anyway")
	}

	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"smpHost":"192.0.2.10","smpPort":443,"smpUsername":"operator","httpPort":8080}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("same address: status %d: %s", rec.Code, rec.Body)
	}
	if got := s.store.Get(); got.SmpPassword != "secret" || got.SmpUsername != "operator" {
		t.Fatalf("saved password not kept: %+v", got)
	}
}

func TestAudioReadingStaysShownWhilePollsAreSkipped(t *testing.T) {
	s := newTestServer(t)
	s.audio = smp.AudioState{LeftGainTenths: 60, RightGainTenths: 60}
	s.audioAt = time.Now().Add(-20 * time.Second)

	rec := get(t, s, "/api/audio")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestAudioReportsAFailedRead(t *testing.T) {
	s := newTestServer(t)
	s.audioAt = time.Now()
	s.audioErr = errors.New("no reply")

	if rec := get(t, s, "/api/audio"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAudioBeforeTheFirstReadingIsNoContent(t *testing.T) {
	if rec := get(t, newTestServer(t), "/api/audio"); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAutoVolumeIsSavedAndBlocksTheFaders(t *testing.T) {
	s := newTestServer(t)
	s.audioAt = time.Now()

	rec := put(t, s, "/api/audio/auto", `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var payload audioPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.AutoVolume || !s.store.Get().AutoVolume {
		t.Fatalf("auto volume not saved: %+v", payload)
	}

	if rec := put(t, s, "/api/audio/gain", `{"gainTenths":30}`); rec.Code != http.StatusConflict {
		t.Fatalf("manual gain while auto: status %d", rec.Code)
	}

	put(t, s, "/api/audio/auto", `{"enabled":false}`)
	if s.store.Get().AutoVolume {
		t.Fatal("auto volume still saved as on")
	}
}
