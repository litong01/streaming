package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streaming/internal/avcontrol"
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

func TestAVConfigurationRoundTrip(t *testing.T) {
	s := newTestServer(t)
	s.pages.ConfigIndex = []byte("<h1>Configuration</h1><a href=\"/config/streaming\">Streaming</a>")
	s.pages.Config = []byte("<h1>Streaming</h1>")
	s.pages.AVConfig = []byte("<h1>AV system control</h1>")
	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	s.AttachAV(avcontrol.New(path, avcontrol.Options{Seed: []byte(avConfigSeed)}))

	if rec := get(t, s, "/config"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Streaming") {
		t.Fatalf("index %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, s, "/config/streaming"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Streaming") {
		t.Fatalf("streaming %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, s, "/config/av"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AV system control") {
		t.Fatalf("av page %d %s", rec.Code, rec.Body)
	}

	rec := get(t, s, "/api/avcontrol/config")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "secret-value") || strings.Contains(rec.Body.String(), "kept-key") {
		t.Fatalf("get %d %s", rec.Code, rec.Body)
	}

	body := `{"projectCode":"project-2","accessId":"access-1","accessSecret":"","items":[{"id":"a","label":"Alpha first","deviceId":"dev-a","ip":"192.0.2.10","protocol":"3.4","delaySec":9},{"label":"Gamma","deviceId":"dev-c","localKey":"gamma-key","protocol":"3.5","delaySec":4}]}`
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/avcontrol/config", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Alpha first") || strings.Contains(rec.Body.String(), "kept-key") {
		t.Fatalf("save %d %s", rec.Code, rec.Body)
	}

	saved, err := avcontrol.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Developer.AccessSecret != "secret-value" || saved.Developer.AccessID != "access-1" || saved.Developer.ProjectCode != "project-2" {
		t.Fatalf("tuya %+v", saved.Developer)
	}
	if len(saved.Switches) != 2 || saved.Switches[0].Label != "Alpha first" || saved.Switches[0].LocalKey != "kept-key" {
		t.Fatalf("switches %+v", saved.Switches)
	}
	if saved.Switches[1].ID != "gamma" || saved.Startup[0].SettleSec != 9 || saved.Startup[1].SettleSec != 4 {
		t.Fatalf("startup %#v second %+v", saved.Startup, saved.Switches[1])
	}
	if saved.Schedule.EnsureOffDaily != "17:00" {
		t.Fatalf("schedule lost: %+v", saved.Schedule)
	}
}

func TestAVConnectionTestDoesNotSave(t *testing.T) {
	s := newTestServer(t)
	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	cmd := &stubAV{ok: map[string]bool{"a": true}}
	s.AttachAV(avcontrol.New(path, avcontrol.Options{Seed: []byte(avConfigSeed), Commands: cmd}))

	body := `{"projectCode":"project-1","accessId":"access-1","items":[{"id":"a","label":"Alpha","deviceId":"dev-a","ip":"192.0.2.10","protocol":"3.4","delaySec":2}]}`
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/avcontrol/config/test", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Reachable") || strings.Contains(rec.Body.String(), "kept-key") {
		t.Fatalf("test %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("test wrote a file: %v", err)
	}
	if cmd.sets != 0 {
		t.Fatal("test turned a switch")
	}
}

func TestAVDiscoverDoesNotReturnKeys(t *testing.T) {
	const key = "0123456789abcdef"
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/token":
			_, _ = io.WriteString(w, `{"success":true,"result":{"access_token":"tok","expire_time":7200}}`)
		case "/v1.0/iot-01/associated-users/devices":
			fmt.Fprintf(w, `{"success":true,"result":{"has_more":false,"devices":[{"id":"dev-1","name":"Amp","local_key":"%s"}]}}`, key)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloud.Close()

	s := newTestServer(t)
	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	seed := strings.Replace(avConfigSeed, "access_secret: secret-value", "access_secret: secret-value\n  endpoint: \""+cloud.URL+"\"", 1)
	s.AttachAV(avcontrol.New(path, avcontrol.Options{Seed: []byte(seed)}))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/avcontrol/config/discover", strings.NewReader(`{"projectCode":"project-1","accessId":"access-1"}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Amp") || strings.Contains(rec.Body.String(), key) {
		t.Fatalf("discover %d %s", rec.Code, rec.Body)
	}
}

type stubAV struct {
	ok   map[string]bool
	sets int
}

func (s *stubAV) Set(context.Context, avcontrol.Switch, bool) error {
	s.sets++
	return nil
}

func (s *stubAV) States(_ context.Context, switches []avcontrol.Switch) map[string]avcontrol.Reading {
	out := make(map[string]avcontrol.Reading, len(switches))
	for _, sw := range switches {
		if s.ok[sw.ID] {
			out[sw.ID] = avcontrol.Reading{On: true}
			continue
		}
		out[sw.ID] = avcontrol.Reading{Err: errors.New("no reply")}
	}
	return out
}

func (s *stubAV) Use(*avcontrol.File) {}

func TestAVControlPageAndActions(t *testing.T) {
	s := newTestServer(t)
	if rec := get(t, s, "/avcontrol"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing page status %d", rec.Code)
	}
	s.pages.AV = []byte("<h1>AV System Control</h1>")
	if rec := get(t, s, "/avcontrol"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AV System Control") {
		t.Fatalf("page status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, s, "/api/avcontrol/status"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/avcontrol/on", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("on without a controller: %d", rec.Code)
	}
}

const avConfigSeed = `
server:
  timezone: "America/New_York"
control:
  prefer_local: false
  cloud_fallback: true
  command_mode: "absolute"
switches:
  - id: a
    label: Alpha
    device_id: dev-a
    local_key: kept-key
startup:
  - { switch: a, settle_sec: 2 }
shutdown: "reverse_of_startup"
schedule:
  ensure_off_daily: "17:00"
off_delay:
  step_minutes: 60
developer.tuya.com:
  project_code: project-1
  access_id: access-1
  access_secret: secret-value
`
