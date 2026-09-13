package smp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"streaming/internal/config"
)

var channels = []int{archiveChannel, confidenceChannel}

func TestStartMandarinStartsTheArchivePush(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)

	state := New().StartMandarin(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if state.ActiveStream != ActiveMandarin || !state.Streaming {
		t.Fatalf("unexpected state: %+v", state)
	}
	want := []string{streamEnableURI(archiveChannel), publishURI(archiveChannel)}
	if got := fake.writtenURIs(); !slices.Equal(got, want) {
		t.Fatalf("wrote %v, want %v", got, want)
	}
}

func TestStartEnglishStartsTheConfidencePush(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)

	state := New().StartEnglish(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if state.ActiveStream != ActiveEnglish || !state.Streaming {
		t.Fatalf("unexpected state: %+v", state)
	}
	want := []string{streamEnableURI(confidenceChannel), publishURI(confidenceChannel)}
	if got := fake.writtenURIs(); !slices.Equal(got, want) {
		t.Fatalf("wrote %v, want %v", got, want)
	}
}

// The unit answers E13 to a publish sent as true or as "1", so the value has
// to go out as a JSON number.
func TestThePublishValueIsWrittenAsANumber(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)

	if state := New().StartMandarin(fake.config(t)); state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	for _, written := range fake.written() {
		if written.uri != publishURI(archiveChannel) {
			continue
		}
		if !strings.Contains(written.body, `"value":1`) {
			t.Fatalf("publish was sent as %s", written.body)
		}
		return
	}
	t.Fatal("no publish was written")
}

// Only one language goes out at a time, so the other one is stopped first.
func TestStartStopsTheOtherLanguageFirst(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.setPublishing(confidenceChannel, true)

	state := New().StartMandarin(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if state.ActiveStream != ActiveMandarin {
		t.Fatalf("unexpected state: %+v", state)
	}
	want := []string{
		publishURI(confidenceChannel),
		streamEnableURI(archiveChannel),
		publishURI(archiveChannel),
	}
	if got := fake.writtenURIs(); !slices.Equal(got, want) {
		t.Fatalf("wrote %v, want %v", got, want)
	}
}

// Both encoders are read before either is touched, so one that is already
// idle has nothing sent to it at all.
func TestStartLeavesAnIdleOtherLanguageAlone(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)

	if state := New().StartMandarin(fake.config(t)); state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	untouched := []string{publishURI(confidenceChannel), streamEnableURI(confidenceChannel)}
	for _, written := range fake.written() {
		if slices.Contains(untouched, written.uri) {
			t.Fatalf("wrote to the idle Confidence channel: %s", written.uri)
		}
	}
}

// A language that is already live is not interrupted and restarted.
func TestStartDoesNothingWhenThatLanguageIsAlreadyLive(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.setPublishing(archiveChannel, true)

	state := New().StartMandarin(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if state.ActiveStream != ActiveMandarin {
		t.Fatalf("unexpected state: %+v", state)
	}
	if written := fake.writtenURIs(); len(written) != 0 {
		t.Fatalf("sent %v to a stream that was already live", written)
	}
}

// The unit refuses a publish on an encoder that is not running, and somebody
// unticking Streaming on its own page is all it takes to get there.
func TestStartEnablesAnEncoderThatWasSwitchedOff(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.setEnabled(archiveChannel, false)

	state := New().StartMandarin(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if !fake.isEnabled(archiveChannel) {
		t.Fatal("the Archive encoder was left switched off")
	}
}

// The publish flag is set as soon as the command is accepted, so a start is
// only reported as working once the unit names a destination it has reached.
func TestStartWaitsForTheDestinationToBeReached(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.connectAfterReads(archiveChannel, 3)

	state := New().StartMandarin(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if got := fake.rtmpReads(archiveChannel); got < 4 {
		t.Fatalf("checked the destination %d times, expected it to keep looking", got)
	}
}

func TestStartFailsWhenTheDestinationIsNeverReached(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.connectAfterReads(archiveChannel, 1000)

	state := New().StartMandarin(fake.config(t))

	if state.LastError == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(*state.LastError, "did not reach its destination") {
		t.Fatalf("unhelpful error: %s", *state.LastError)
	}
	if state.ActiveStream != ActiveNone {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestStartFailsWhenThePushDropsStraightAway(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.dropPublishOnRead(archiveChannel)

	state := New().StartMandarin(fake.config(t))

	if state.LastError == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(*state.LastError, "stopped as soon as it started") {
		t.Fatalf("unhelpful error: %s", *state.LastError)
	}
}

// Stop asks first, so an encoder that is already idle has nothing sent to it.
func TestStopOnlyStopsWhatIsStreaming(t *testing.T) {
	fake := newFakeSMP(t)
	fake.setPublishing(archiveChannel, true)

	state := New().Stop(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("stop failed: %s", *state.LastError)
	}
	if state.ActiveStream != ActiveNone || state.Streaming {
		t.Fatalf("unexpected state: %+v", state)
	}
	want := []string{publishURI(archiveChannel)}
	if got := fake.writtenURIs(); !slices.Equal(got, want) {
		t.Fatalf("wrote %v, want %v", got, want)
	}
}

func TestStopSendsNothingWhenNothingIsStreaming(t *testing.T) {
	fake := newFakeSMP(t)

	state := New().Stop(fake.config(t))

	if state.LastError != nil {
		t.Fatalf("stop failed: %s", *state.LastError)
	}
	if written := fake.writtenURIs(); len(written) != 0 {
		t.Fatalf("sent %v with nothing streaming", written)
	}
}

// Switching an encoder off is what makes the unit report its configuration as
// modified and grey out half of its own streaming page, so Stop leaves the
// encoders running and only ends the push.
func TestStopLeavesTheEncodersRunning(t *testing.T) {
	fake := newFakeSMP(t)
	fake.setPublishing(archiveChannel, true)
	fake.setPublishing(confidenceChannel, true)

	if state := New().Stop(fake.config(t)); state.LastError != nil {
		t.Fatalf("stop failed: %s", *state.LastError)
	}
	for _, channel := range channels {
		if !fake.isEnabled(channel) {
			t.Fatalf("the %s encoder was switched off", channelName(channel))
		}
	}
}

func TestARefusedCommandNamesWhatWasRefused(t *testing.T) {
	quickConfirm(t)
	fake := newFakeSMP(t)
	fake.refuseResource(streamEnableURI(archiveChannel), "E13")

	state := New().StartMandarin(fake.config(t))

	if state.LastError == nil {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"enable the Archive encoder", streamEnableURI(archiveChannel), "E13"} {
		if !strings.Contains(*state.LastError, want) {
			t.Fatalf("error %q does not mention %q", *state.LastError, want)
		}
	}
}

func TestQueryStateNamesTheLanguageThatIsStreaming(t *testing.T) {
	tests := []struct {
		archive    bool
		confidence bool
		stream     ActiveStream
		message    string
	}{
		{false, false, ActiveNone, "Idle"},
		{true, false, ActiveMandarin, "Streaming Mandarin"},
		{false, true, ActiveEnglish, "Streaming English"},
		// Not something the buttons can produce, but the unit's own page can.
		{true, true, ActiveNone, "Streaming Mandarin and English"},
	}
	for _, test := range tests {
		fake := newFakeSMP(t)
		fake.setPublishing(archiveChannel, test.archive)
		fake.setPublishing(confidenceChannel, test.confidence)

		state, polled := New().QueryState(fake.config(t))
		if !polled {
			t.Fatal("the poll was skipped")
		}
		if state.ActiveStream != test.stream || state.StatusMessage != test.message {
			t.Fatalf("archive=%t confidence=%t gave %+v", test.archive, test.confidence, state)
		}
		if state.Streaming != (test.archive || test.confidence) {
			t.Fatalf("archive=%t confidence=%t gave streaming=%t", test.archive, test.confidence, state.Streaming)
		}
	}
}

// A poll is one request: both channels are asked for together.
func TestAPollIsASingleRequest(t *testing.T) {
	fake := newFakeSMP(t)

	if _, polled := New().QueryState(fake.config(t)); !polled {
		t.Fatal("the poll was skipped")
	}
	if got := fake.requestCount(); got != 1 {
		t.Fatalf("made %d requests, want 1", got)
	}
}

func TestQueryStateStepsAsideWhileAnActionRuns(t *testing.T) {
	client := New()
	client.mu.Lock()
	defer client.mu.Unlock()

	cfg := config.Default()
	cfg.SmpHost = "192.0.2.1"
	cfg.SmpUsername = "admin"
	if _, polled := client.QueryState(cfg); polled {
		t.Fatal("the poll should have stepped aside for the action")
	}
}

func TestQueryStateReportsARefusedRead(t *testing.T) {
	fake := newFakeSMP(t)
	fake.refuseResource(publishURI(confidenceChannel), "E10")

	state, polled := New().QueryState(fake.config(t))
	if !polled {
		t.Fatal("the poll was skipped")
	}
	if state.LastError == nil {
		t.Fatal("expected a failure")
	}
	if state.SmpReachable {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestActionsRefuseAnUnconfiguredSMP(t *testing.T) {
	client := New()
	cfg := config.Default()
	for name, state := range map[string]State{
		"start Mandarin": client.StartMandarin(cfg),
		"start English":  client.StartEnglish(cfg),
		"stop":           client.Stop(cfg),
	} {
		if state.LastError == nil {
			t.Fatalf("%s: expected a failure", name)
		}
		if state.StatusMessage != "SMP not configured" {
			t.Fatalf("%s: unexpected state: %+v", name, state)
		}
	}
}

func TestPreviewCarriesTheUnitsStream(t *testing.T) {
	fake := newFakeSMP(t)

	body, err := New().Preview(context.Background(), fake.config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != previewFragment {
		t.Fatalf("relayed %q", data)
	}
}

func TestDiagnoseChecksBothTheAPIAndThePreview(t *testing.T) {
	fake := newFakeSMP(t)
	fake.setPublishing(confidenceChannel, true)

	result := New().Diagnose(fake.config(t))

	if !result.OK || result.Stage != StageOK {
		t.Fatalf("unexpected diagnosis: %+v", result)
	}
	for _, want := range []string{"Mandarin (Archive) is idle", "English (Confidence) is streaming"} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("detail %q does not mention %q", result.Detail, want)
		}
	}
}

func TestDiagnoseBlamesThePreviewWhenOnlyItFails(t *testing.T) {
	fake := newFakeSMP(t)
	fake.failPreview()

	result := New().Diagnose(fake.config(t))

	if result.OK || result.Stage != StagePreview {
		t.Fatalf("unexpected diagnosis: %+v", result)
	}
}

func TestDiagnoseBlamesTheCredentials(t *testing.T) {
	fake := newFakeSMP(t)
	cfg := fake.config(t)
	cfg.SmpPassword = "wrong"

	result := New().Diagnose(cfg)

	if result.OK || result.Stage != StageAuth {
		t.Fatalf("unexpected diagnosis: %+v", result)
	}
}

func TestDiagnoseBlamesTheNetworkWhenNothingAnswers(t *testing.T) {
	cfg := config.Default()
	// A port nothing listens on, on the loopback address, so the connection is
	// refused rather than left to time out.
	cfg.SmpHost = "127.0.0.1"
	cfg.SmpPort = closedPort(t)
	cfg.SmpUsername = "admin"

	result := New().Diagnose(cfg)

	if result.OK || result.Stage != StageNetwork {
		t.Fatalf("unexpected diagnosis: %+v", result)
	}
}

// Every port but 80 is spoken to over TLS, so a plain HTTP service on an
// unusual port needs saying so rather than being blamed on the unit's answer.
func TestDiagnoseSpotsAPlainHTTPPort(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "[]")
	}))
	defer plain.Close()

	parsed, err := url.Parse(plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SmpHost = parsed.Hostname()
	cfg.SmpPort = port
	cfg.SmpUsername = "admin"

	result := New().Diagnose(cfg)

	if result.OK || result.Stage != StageNetwork {
		t.Fatalf("unexpected diagnosis: %+v", result)
	}
	if !strings.Contains(result.Hint, "port 80") {
		t.Fatalf("hint does not mention the plain HTTP port: %q", result.Hint)
	}
}

func TestEndpointSchemeFollowsThePort(t *testing.T) {
	cfg := config.Default()
	cfg.SmpHost = "192.0.2.1"
	if got := endpoint(cfg, "/x"); got != "https://192.0.2.1:443/x" {
		t.Fatalf("endpoint = %s", got)
	}
	cfg.SmpPort = 80
	if got := endpoint(cfg, "/x"); got != "http://192.0.2.1:80/x" {
		t.Fatalf("endpoint = %s", got)
	}
	cfg.SmpHost = "2001:db8::1"
	cfg.SmpPort = config.DefaultSmpPort
	if got := endpoint(cfg, "/x"); got != "https://[2001:db8::1]:443/x" {
		t.Fatalf("endpoint = %s", got)
	}
}

// quickConfirm keeps a test from waiting out the real confirmation window.
func quickConfirm(t *testing.T) {
	t.Helper()
	window, interval := publishConfirmWait, publishPollInterval
	publishConfirmWait, publishPollInterval = 500*time.Millisecond, time.Millisecond
	t.Cleanup(func() { publishConfirmWait, publishPollInterval = window, interval })
}

func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

const (
	fakeUser        = "admin"
	fakePassword    = "secret"
	previewFragment = "\x00\x00\x00\x18ftypiso5"
)

// fakeSMP answers the resources the client uses the way the unit does,
// including its habits of reporting a refusal as HTTP 200 with an Extron code
// and of accepting a publish only on an encoder that is already running.
type fakeSMP struct {
	server *httptest.Server

	mu            sync.Mutex
	enabled       map[int]bool
	publishing    map[int]bool
	connectAfter  map[int]int
	dropOnRead    map[int]bool
	refusals      map[string]string
	rtmpReadCount map[int]int
	writes        []writtenResource
	requests      int
	previewBroken bool
}

type writtenResource struct {
	uri  string
	body string
}

func newFakeSMP(t *testing.T) *fakeSMP {
	t.Helper()
	fake := &fakeSMP{
		enabled:       map[int]bool{archiveChannel: true, confidenceChannel: true},
		publishing:    map[int]bool{},
		connectAfter:  map[int]int{},
		dropOnRead:    map[int]bool{},
		refusals:      map[string]string{},
		rtmpReadCount: map[int]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(resourcePath, fake.handleResources)
	mux.HandleFunc("/mp4stream", fake.handlePreview)
	// A TLS server with its own throwaway certificate, which is what the unit
	// presents as well.
	fake.server = httptest.NewTLSServer(requireBasicAuth(mux))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeSMP) config(t *testing.T) config.Config {
	t.Helper()
	parsed, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SmpHost = parsed.Hostname()
	cfg.SmpPort = port
	cfg.SmpUsername = fakeUser
	cfg.SmpPassword = fakePassword
	return cfg
}

func requireBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != fakeUser || password != fakePassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="SMP 351"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fakeSMP) handleResources(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++

	var entries []string
	switch r.Method {
	case http.MethodGet:
		for _, uri := range r.URL.Query()["uri"] {
			entries = append(entries, f.readLocked(uri))
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		var updates []struct {
			URI   string          `json:"uri"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(body, &updates); err != nil {
			http.Error(w, "malformed body", http.StatusBadRequest)
			return
		}
		for _, update := range updates {
			f.writes = append(f.writes, writtenResource{uri: update.URI, body: string(body)})
			entries = append(entries, f.writeLocked(update.URI, update.Value))
		}
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		return
	}

	// A refusal comes back as 200 as well, as it does on the unit.
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "["+strings.Join(entries, ",")+"]")
}

func (f *fakeSMP) handlePreview(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	broken := f.previewBroken
	f.mu.Unlock()
	if broken {
		http.Error(w, "preview off", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	_, _ = io.WriteString(w, previewFragment)
}

func (f *fakeSMP) readLocked(uri string) string {
	if status, ok := f.refusals[uri]; ok {
		return refusalEntry(uri, status)
	}
	for _, channel := range channels {
		switch uri {
		case publishURI(channel):
			return resultEntry(uri, boolValue(f.publishing[channel]))
		case streamEnableURI(channel):
			return resultEntry(uri, boolValue(f.enabled[channel]))
		case rtmpURI(channel):
			return resultEntry(uri, f.rtmpLocked(channel))
		}
	}
	return refusalEntry(uri, "E10")
}

// rtmpLocked answers as the unit does: the session row only names a
// destination once the push has resolved one and connected to it, and the
// resource carries the destination URL and its key alongside.
func (f *fakeSMP) rtmpLocked(channel int) map[string]any {
	f.rtmpReadCount[channel]++
	if f.dropOnRead[channel] {
		f.publishing[channel] = false
	}
	idle := map[string]any{"pub_control": 0, "resolved_ip": "0.0.0.0", "resolved_port": 0}
	session := idle
	if f.publishing[channel] && f.rtmpReadCount[channel] > f.connectAfter[channel] {
		session = map[string]any{
			"pub_control":   1,
			"resolved_ip":   "172.253.124.134",
			"resolved_port": 1935,
		}
	}
	return map[string]any{
		"pub_control":  boolValue(f.publishing[channel]),
		"pub_url":      "rtmp://a.example/live/stream-key",
		"password":     "destination-password",
		"session_info": []any{session, idle},
	}
}

func (f *fakeSMP) writeLocked(uri string, raw json.RawMessage) string {
	if status, ok := f.refusals[uri]; ok {
		return refusalEntry(uri, status)
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return refusalEntry(uri, "E13")
	}
	for _, channel := range channels {
		switch uri {
		case streamEnableURI(channel):
			f.enabled[channel] = value == 1
			return resultEntry(uri, value)
		case publishURI(channel):
			// A publish on an encoder that is not running is refused outright.
			if value == 1 && !f.enabled[channel] {
				return refusalEntry(uri, "E13")
			}
			f.publishing[channel] = value == 1
			f.rtmpReadCount[channel] = 0
			return resultEntry(uri, value)
		}
	}
	return refusalEntry(uri, "E10")
}

func resultEntry(uri string, value any) string {
	encoded, err := json.Marshal(map[string]any{
		"meta":   map[string]any{"uri": uri},
		"result": value,
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func refusalEntry(uri, status string) string {
	encoded, err := json.Marshal(map[string]any{
		"meta": map[string]any{"uri": uri, "status": status},
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (f *fakeSMP) setPublishing(channel int, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishing[channel] = on
}

func (f *fakeSMP) setEnabled(channel int, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled[channel] = on
}

func (f *fakeSMP) isEnabled(channel int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled[channel]
}

// connectAfterReads makes a push take that many status reads before it names a
// destination, standing in for the seconds the unit spends reaching YouTube.
func (f *fakeSMP) connectAfterReads(channel, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectAfter[channel] = reads
}

// dropPublishOnRead stands in for a destination that refuses the push: the
// unit accepts the command and then gives up on it.
func (f *fakeSMP) dropPublishOnRead(channel int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropOnRead[channel] = true
}

func (f *fakeSMP) refuseResource(uri, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals[uri] = status
}

func (f *fakeSMP) failPreview() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previewBroken = true
}

func (f *fakeSMP) written() []writtenResource {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeSMP) writtenURIs() []string {
	writes := f.written()
	uris := make([]string, 0, len(writes))
	for _, written := range writes {
		uris = append(uris, written.uri)
	}
	return uris
}

func (f *fakeSMP) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeSMP) rtmpReads(channel int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rtmpReadCount[channel]
}
