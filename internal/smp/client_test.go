package smp

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"streaming/internal/config"
)

func TestSMP351StreamCommandsUseSISControlBytes(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"enable archive A", streamEnabledCommand(1, 1), "\x1b1*1STRC\r"},
		{"disable archive A", streamEnabledCommand(1, 0), "\x1b1*0STRC\r"},
		{"query archive A", streamEnabledQuery(1), "\x1b1STRC\r"},
		{"recall preset 2", streamingPresetRecallCommand(1, 2), "3*1*2."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("command = %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestStartRecallsLanguageOnArchiveOnly(t *testing.T) {
	tests := []struct {
		name           string
		start          func(*Client, config.Config) State
		languagePreset int
		active         ActiveStream
	}{
		{"English", (*Client).StartEnglish, 2, ActiveEnglish},
		{"Mandarin", (*Client).StartMandarin, 1, ActiveMandarin},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withoutStartConfirmWait(t)
			encoders := newFakeEncoders()
			cfg := configuredTestConfig()

			state := test.start(encoders.client(), cfg)
			want := []string{
				// The encoder is read before it is disturbed.
				"\x1b1STRC\r", "46I",
				"\x1b1*0STRC\r", fmt.Sprintf("3*1*%d.", test.languagePreset), "\x1b1*1STRC\r",
				// Then the result is confirmed rather than assumed.
				"\x1b1STRC\r", "46I",
			}
			if strings.Join(encoders.commands, "|") != strings.Join(want, "|") {
				t.Fatalf("commands = %q, want %q", encoders.commands, want)
			}
			if state.ActiveStream != test.active || !state.StreamEnabled {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

// The Confidence encoder belongs to whoever configured the unit. Nothing the
// app does may read it, recall onto it, or switch it, whatever state it is in.
func TestStartAndStopNeverTouchTheConfidenceEncoder(t *testing.T) {
	withoutStartConfirmWait(t)
	cfg := configuredTestConfig()
	encoders := newFakeEncoders()
	encoders.streaming(archiveStreamIndex, cfg.MandarinPreset)
	encoders.streaming(confidenceStreamIndex, 7)

	client := encoders.client()
	if state := client.StartEnglish(cfg); state.LastError != nil {
		t.Fatalf("start failed: %s", *state.LastError)
	}
	if err := client.stopArchive(cfg); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	for _, command := range encoders.commands {
		if strings.Contains(command, "3STRC") || strings.HasPrefix(command, "3*3*") || command == "48I" {
			t.Errorf("touched the Confidence encoder: %q", printable(command))
		}
	}
	if !encoders.enabled[confidenceStreamIndex] || encoders.recalled[confidenceStreamIndex] != 7 {
		t.Errorf("Confidence encoder changed: enabled=%v preset=%d",
			encoders.enabled[confidenceStreamIndex], encoders.recalled[confidenceStreamIndex])
	}
}

// Tapping the language that is already running must not interrupt the push,
// so nothing is sent beyond the two reads that establish it is already right.
func TestStartDoesNothingWhenThatLanguageIsAlreadyLive(t *testing.T) {
	cfg := configuredTestConfig()
	encoders := newFakeEncoders()
	encoders.streaming(archiveStreamIndex, cfg.EnglishPreset)

	started := time.Now()
	state := encoders.client().StartEnglish(cfg)
	// Deliberately not shortening startConfirmWait: there is nothing to
	// confirm when nothing was changed.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("took %s, want no settle delay", elapsed)
	}
	want := []string{"\x1b1STRC\r", "46I"}
	if strings.Join(encoders.commands, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", encoders.commands, want)
	}
	if state.ActiveStream != ActiveEnglish || !state.StreamEnabled {
		t.Fatalf("state = %#v", state)
	}
	if state.StatusMessage != "Streaming English (preset 2)" {
		t.Errorf("status = %q", state.StatusMessage)
	}
}

// The Confidence encoder is stream 3. Production code no longer names it,
// because nothing in the app may touch it; the fake still models it so that
// can be proved.
const confidenceStreamIndex = 3

// fakeEncoders models the pair of encoders closely enough to exercise the
// decisions a start makes. Both are modelled so a test can prove the
// Confidence encoder is never touched. Switching one off marks its configuration as
// drifted from the saved preset, which is what the unit itself reports.
type fakeEncoders struct {
	enabled  map[int]bool
	recalled map[int]int
	modified map[int]bool
	// dropped names an encoder that refuses to stay switched on, the way the
	// unit behaves when it cannot reach a destination.
	dropped  int
	commands []string
}

func newFakeEncoders() *fakeEncoders {
	return &fakeEncoders{
		enabled:  map[int]bool{},
		recalled: map[int]int{},
		modified: map[int]bool{},
	}
}

// streaming puts an encoder in the state of already running a preset.
func (f *fakeEncoders) streaming(streamIndex, preset int) {
	f.enabled[streamIndex] = true
	f.recalled[streamIndex] = preset
	f.modified[streamIndex] = false
}

func (f *fakeEncoders) client() *Client {
	return &Client{send: f.send}
}

func (f *fakeEncoders) send(
	_ config.Config,
	command string,
	matchesResponse func(string) bool,
) (string, error) {
	f.commands = append(f.commands, command)
	response, ok := f.respond(command)
	if !ok {
		return "", fmt.Errorf("unexpected command %q", printable(command))
	}
	if !matchesResponse(response) {
		return "", fmt.Errorf("response %q did not match command %q", response, printable(command))
	}
	return response, nil
}

func (f *fakeEncoders) respond(command string) (string, bool) {
	if match := fakeRecallRegex.FindStringSubmatch(command); match != nil {
		streamIndex, preset := atoiOrZero(match[1]), atoiOrZero(match[2])
		f.recalled[streamIndex] = preset
		// The configuration only matches the preset once the encoder is on,
		// because the preset carries the enabled flag too.
		f.modified[streamIndex] = !f.enabled[streamIndex]
		return fmt.Sprintf("3Rpr%02d*%02d", streamIndex, preset), true
	}
	for _, streamIndex := range []int{archiveStreamIndex, confidenceStreamIndex} {
		switch command {
		case streamEnabledQuery(streamIndex):
			if f.enabled[streamIndex] {
				return "1", true
			}
			return "0", true
		case streamingPresetQueryCommand(streamIndex):
			preset := f.recalled[streamIndex]
			if preset == 0 || f.modified[streamIndex] {
				return "0*modified, not saved", true
			}
			return fmt.Sprintf("%d*STREAMING PRESET %02d", preset, preset), true
		case streamEnabledCommand(streamIndex, 0):
			f.enabled[streamIndex] = false
			f.modified[streamIndex] = true
			return fmt.Sprintf("Strc%d*0", streamIndex), true
		case streamEnabledCommand(streamIndex, 1):
			f.enabled[streamIndex] = streamIndex != f.dropped
			f.modified[streamIndex] = !f.enabled[streamIndex]
			return fmt.Sprintf("Strc%d*1", streamIndex), true
		}
	}
	return "", false
}

var fakeRecallRegex = regexp.MustCompile(`^3\*(\d+)\*(\d+)\.$`)

func atoiOrZero(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

// A destination the unit cannot reach shows up as the encoder switching
// itself off again moments after being started, which has to be reported
// rather than announced as a live stream.
func TestStartFailsWhenTheEncoderDoesNotStayUp(t *testing.T) {
	withoutStartConfirmWait(t)
	encoders := newFakeEncoders()
	encoders.dropped = archiveStreamIndex

	state := encoders.client().StartMandarin(configuredTestConfig())
	if state.LastError == nil {
		t.Fatal("a start whose encoder stopped again should report an error")
	}
	if state.StatusMessage != "Start failed" {
		t.Errorf("status = %q, want Start failed", state.StatusMessage)
	}
	if state.ActiveStream != ActiveNone {
		t.Errorf("active stream = %q, want none", state.ActiveStream)
	}
}

// withoutStartConfirmWait removes the settle delay, which is there for a real
// unit rather than for a stubbed one.
func withoutStartConfirmWait(t *testing.T) {
	t.Helper()
	previous := startConfirmWait
	startConfirmWait = 0
	t.Cleanup(func() { startConfirmWait = previous })
}

func TestStopDisablesArchiveOnly(t *testing.T) {
	var commands []string
	client := &Client{send: func(
		_ config.Config,
		command string,
		matchesResponse func(string) bool,
	) (string, error) {
		commands = append(commands, command)
		response := responseForCommand(command)
		if command == "\x1b1STRC\r" {
			response = "0"
		}
		if !matchesResponse(response) {
			t.Fatalf("response %q did not match command %q", response, command)
		}
		return response, nil
	}}

	state := client.Stop(configuredTestConfig())
	want := []string{"\x1b1*0STRC\r", "\x1b1STRC\r", "46I"}
	if strings.Join(commands, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", commands, want)
	}
	if state.StreamEnabled {
		t.Fatalf("state = %#v", state)
	}
}

func configuredTestConfig() config.Config {
	cfg := config.Default()
	cfg.SmpHost = "192.0.2.1"
	cfg.SmpUsername = "admin"
	return cfg
}

func responseForCommand(command string) string {
	switch command {
	case "\x1b1*0STRC\r":
		return "Strc1*0"
	case "\x1b3*0STRC\r":
		return "Strc3*0"
	case "\x1b1*1STRC\r":
		return "Strc1*1"
	case "\x1b3*1STRC\r":
		return "Strc3*1"
	case "\x1b1STRC\r":
		return "1"
	case "46I":
		return "2*STREAMING PRESET 02"
	// The unit pads both numbers in a recall reply to two digits.
	case "3*1*1.":
		return "3Rpr01*01"
	case "3*1*2.":
		return "3Rpr01*02"
	case "3*3*3.":
		return "3Rpr03*03"
	default:
		return "E10"
	}
}

// Firmware 2.11 answers 3*3*3. with 3Rpr03*03. Matching that reply as literal
// text is what made every Start press wait out its timeout and report a
// failure for a recall the unit had already carried out.
func TestPresetRecallReplyPaddingIsAccepted(t *testing.T) {
	tests := []struct {
		reply  string
		stream int
		preset int
		want   bool
	}{
		{reply: "3Rpr03*03", stream: 3, preset: 3, want: true},
		{reply: "3Rpr01*01", stream: 1, preset: 1, want: true},
		{reply: "3Rpr01*11", stream: 1, preset: 11, want: true},
		{reply: "3Rpr1*1", stream: 1, preset: 1, want: true},
		{reply: "3rpr01*02", stream: 1, preset: 2, want: true},
		{reply: "3Rpr01*02", stream: 1, preset: 1},
		{reply: "3Rpr03*01", stream: 1, preset: 1},
		{reply: "Strc1*1", stream: 1, preset: 1},
		{reply: "E13", stream: 1, preset: 1},
	}
	for _, test := range tests {
		got := matchesPresetRecall(test.reply, test.stream, test.preset)
		if got != test.want {
			t.Errorf("%q for stream %d preset %d = %t, want %t",
				test.reply, test.stream, test.preset, got, test.want)
		}
	}
}

func TestSMP351ErrorResponses(t *testing.T) {
	for _, response := range []string{"E10", "E24", "E13"} {
		if !sisErrorRegex.MatchString(response) {
			t.Errorf("did not recognize %q as an SIS error", response)
		}
	}
	if sisErrorRegex.MatchString("Strc1*1") {
		t.Fatal("recognized a successful response as an SIS error")
	}
}

func TestSMP351StreamingPresetQueries(t *testing.T) {
	tests := []struct {
		stream int
		want   string
	}{
		{1, "46I"},
		{2, "47I"},
		{3, "48I"},
	}
	for _, test := range tests {
		if got := streamingPresetQueryCommand(test.stream); got != test.want {
			t.Errorf("stream %d query = %q, want %q", test.stream, got, test.want)
		}
	}
}

func TestReadUntilSISResponseSkipsSMP351Banner(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{
			name: "firmware 2.11 non-verbose response",
			response: "(c) Copyright 2014-2019, Extron Electronics, SMP 351, V2.11, 60-1324-01\r\n" +
				"Fri, 11 Sep 2026 23:59:11\r\n" +
				"1\r\n",
			want: "1",
		},
		{
			name:     "verbose response",
			response: "Strc1*1\r\n",
			want:     "Strc1*1",
		},
		{
			name:     "extra carriage return over SSH",
			response: "Strc1*1\r\r\n",
			want:     "Strc1*1",
		},
		{
			name:     "carriage returns without line feeds",
			response: "(c) Copyright 2014-2019, Extron Electronics, SMP 351\rStrc1*1\r",
			want:     "Strc1*1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := readUntilResponse(
				strings.NewReader(test.response),
				time.Second,
				matchesStreamStatus,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Errorf("response = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReadUntilSISResponseReturnsErrorAfterBanner(t *testing.T) {
	response := "(c) Copyright 2014-2019, Extron Electronics, SMP 351\r\nE24\r\n"
	got, err := readUntilResponse(
		strings.NewReader(response),
		time.Second,
		matchesStreamStatus,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "E24" {
		t.Fatalf("response = %q, want E24", got)
	}
}

// A session's last reply sometimes arrives with no terminator at all, which a
// line reader alone would wait out and report as silence.
func TestReadUntilSISResponseAcceptsReplyWithoutTerminator(t *testing.T) {
	reader := haltingReader(t, "(c) Copyright 2014-2019, Extron Electronics, SMP 351\r\n", "Strc1*1")

	got, err := readUntilResponse(reader, 2*time.Second, matchesStreamStatus)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Strc1*1" {
		t.Fatalf("response = %q, want Strc1*1", got)
	}
}

// Only a channel that produced nothing at all should send the client looking
// for a different channel type; a unit that greeted us and then ignored the
// command has a command problem, and retrying cannot help.
func TestMuteOnlyCoversChannelsThatSaidNothing(t *testing.T) {
	tests := []struct {
		name   string
		output []string
		want   bool
	}{
		{name: "silent channel", want: true},
		{name: "rejected channel", want: true},
		{name: "banner but no reply", output: []string{"(c) Copyright 2014-2019, Extron Electronics, SMP 351\r\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "rejected channel" {
				if !mute(fmt.Errorf("%w: shell request failed", errChannelRejected)) {
					t.Fatal("a channel that never opened should be retried elsewhere")
				}
				return
			}
			_, err := readUntilResponse(haltingReader(t, test.output...), 300*time.Millisecond, matchesStreamStatus)
			if err == nil {
				t.Fatal("expected the read to fail")
			}
			if mute(err) != test.want {
				t.Fatalf("mute(%v) = %t, want %t", err, mute(err), test.want)
			}
		})
	}
}

// The SMP puts its banner, and on some firmware its errors, on stderr. A
// stdout-only reader cannot tell that apart from a unit that never answered.
func TestMergeOutputCarriesStderr(t *testing.T) {
	stdout, stdoutWriter := io.Pipe()
	stderr, stderrWriter := io.Pipe()
	go func() {
		_, _ = io.WriteString(stderrWriter, "Strc1*1\r\n")
		_ = stderrWriter.Close()
		_ = stdoutWriter.Close()
	}()

	got, err := readUntilResponse(mergeOutput(stdout, stderr), time.Second, matchesStreamStatus)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Strc1*1" {
		t.Fatalf("response = %q, want Strc1*1", got)
	}
}

// haltingReader serves its chunks and then goes quiet without closing, the way
// an SSH channel stays open after the SMP stops talking.
func haltingReader(t *testing.T, chunks ...string) io.Reader {
	t.Helper()
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	return &quietAfter{chunks: chunks, blocked: blocked}
}

type quietAfter struct {
	chunks  []string
	blocked chan struct{}
}

func (r *quietAfter) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		<-r.blocked
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}

func TestParseSelectedStreamingPreset(t *testing.T) {
	tests := []struct {
		response string
		want     *int
	}{
		{"0*modified, not saved", nil},
		{"0*modified,not saved", nil},
		{"3*RTMPYouTube", intPtr(3)},
		{"1*", intPtr(1)},
	}
	for _, test := range tests {
		got := parseSelectedStreamingPreset(test.response)
		if !presetEqual(got, test.want) {
			t.Errorf("%q => %v, want %v", test.response, got, test.want)
		}
	}
}

func TestReadUntilPresetQuerySkipsBanner(t *testing.T) {
	response := "(c) Copyright 2014-2019, Extron Electronics, SMP 351, V2.11, 60-1324-01\r\n" +
		"Sat, 12 Sep 2026 00:05:40\r\n" +
		"0*modified, not saved\r\n"
	got, err := readUntilResponse(strings.NewReader(response), time.Second, func(line string) bool {
		return presetResponseRegex.MatchString(line)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "0*modified, not saved" {
		t.Fatalf("got %q", got)
	}
	if parseSelectedStreamingPreset(got) != nil {
		t.Fatal("unsaved stream should not report a selected preset")
	}
}

func intPtr(value int) *int { return &value }

func presetEqual(got, want *int) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func TestStreamStatusResponseForms(t *testing.T) {
	for _, response := range []string{
		"0",
		"1",
		"Strc1*0",
		"Strc1*1",
	} {
		if !matchesStreamStatus(response) {
			t.Errorf("did not recognize %q as stream status", response)
		}
	}
}

func TestDialClientAuthenticatesWithPassword(t *testing.T) {
	cfg, serverDone := startTestSSHServer(t, func(server *ssh.ServerConfig) {
		server.PasswordCallback = func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if metadata.User() != "admin" {
				return nil, fmt.Errorf("unexpected username %q", metadata.User())
			}
			if string(password) != " secret with spaces " {
				return nil, fmt.Errorf("unexpected password %q", password)
			}
			return nil, nil
		}
	})
	cfg.SmpUsername = " admin "
	cfg.SmpPassword = " secret with spaces "

	client, err := dialClient(cfg)
	if err != nil {
		t.Fatalf("dialClient: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("SSH server: %v", err)
	}
}

func TestDialClientFallsBackToKeyboardInteractivePassword(t *testing.T) {
	cfg, serverDone := startTestSSHServer(t, func(server *ssh.ServerConfig) {
		server.KeyboardInteractiveCallback = func(
			metadata ssh.ConnMetadata,
			challenge ssh.KeyboardInteractiveChallenge,
		) (*ssh.Permissions, error) {
			answers, err := challenge(
				"SMP 351",
				"Authentication required",
				[]string{"Password: "},
				[]bool{false},
			)
			if err != nil {
				return nil, err
			}
			if metadata.User() != "admin" || len(answers) != 1 || answers[0] != "secret" {
				return nil, fmt.Errorf("unexpected keyboard-interactive credentials")
			}
			return nil, nil
		}
	})
	cfg.SmpUsername = "admin"
	cfg.SmpPassword = "secret"

	client, err := dialClient(cfg)
	if err != nil {
		t.Fatalf("dialClient: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("SSH server: %v", err)
	}
}

func TestPasswordChallengeOnlyAnswersPasswordPrompts(t *testing.T) {
	answers, err := passwordChallenge("secret")(
		"",
		"",
		[]string{"Password: ", "Verification code: "},
		[]bool{false, false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0] != "secret" || answers[1] != "" {
		t.Fatalf("answers = %#v", answers)
	}
}

// The failure this was written for: firmware that authenticates, opens the
// channel, and then says nothing because its SIS session only starts once a
// terminal has been requested.
func TestSendCommandFindsTheChannelTypeTheSMPAnswersOn(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{answersOn: "pty-shell"})
	client, err := dialClient(smp.config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn := &connection{client: client}
	for attempt := range 2 {
		response, err := conn.sendCommand(streamEnabledQuery(1), matchesStreamStatus)
		if err != nil {
			t.Fatalf("command %d: %v", attempt+1, err)
		}
		if response != "1" {
			t.Fatalf("command %d: response = %q, want 1", attempt+1, response)
		}
	}
	if conn.transport != terminalTransport {
		t.Fatalf("transport = %v, want a shell with a terminal", conn.transport)
	}
	// The second command must reuse what worked instead of searching again,
	// so one button press does not open nine channels on a unit that allows
	// very few.
	want := []string{"shell", "pty-shell", "pty-shell"}
	if got := smp.channelsOpened(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("channels opened = %v, want %v", got, want)
	}
}

func TestSendCommandReportsSilenceOnEveryChannelType(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{closeChannels: true})
	client, err := dialClient(smp.config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_, err = (&connection{client: client}).sendCommand(streamEnabledQuery(1), matchesStreamStatus)
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	if !strings.Contains(err.Error(), "sent nothing back on any SSH channel type") {
		t.Fatalf("error = %v", err)
	}
	if got := smp.channelsOpened(); len(got) != len(sisTransports) {
		t.Fatalf("channels opened = %v, want one per channel type", got)
	}
}

// The poller logs in again every few seconds, so the search for a working
// channel type has to happen once rather than on every poll.
func TestTheWorkingChannelTypeIsRememberedBetweenLogins(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{answersOn: "pty-shell", closeChannels: true})
	client := New()

	for poll := range 2 {
		state, polled := client.QueryState(smp.config)
		if !polled || !state.SmpReachable {
			t.Fatalf("poll %d: state = %#v", poll+1, state)
		}
	}

	// Two commands per poll, and only the very first one had to search.
	want := []string{"shell", "pty-shell", "pty-shell", "pty-shell", "pty-shell"}
	if got := smp.channelsOpened(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("channels opened = %v, want %v", got, want)
	}
}

// A poll must never hold up a button press or a connection test, because the
// unit answers one session at a time.
func TestQueryStateStepsAsideWhileAnotherExchangeRuns(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{answersOn: "shell"})
	client := New()
	client.mu.Lock()
	defer client.mu.Unlock()

	if _, polled := client.QueryState(smp.config); polled {
		t.Fatal("the poll should have been skipped while the SMP was busy")
	}
}

func TestDiagnoseNamesTheChannelThatAnswered(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{answersOn: "shell", bannerOnStderr: true})

	result := New().Diagnose(smp.config)
	if !result.OK || result.Stage != StageOK {
		t.Fatalf("diagnosis = %#v", result)
	}
	if !strings.Contains(result.Detail, "plain shell") {
		t.Fatalf("detail = %q, want the channel type that answered", result.Detail)
	}
}

// Total silence is a channel or session-limit problem, not a command problem,
// so the advice has to point somewhere other than the command bytes.
func TestDiagnoseBlamesTheSessionWhenNothingIsSaid(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{closeChannels: true})

	result := New().Diagnose(smp.config)
	if result.OK || result.Stage != StageCommand {
		t.Fatalf("diagnosis = %#v", result)
	}
	if !strings.Contains(result.Summary, "sent nothing back on any SSH channel") {
		t.Fatalf("summary = %q", result.Summary)
	}
	if !strings.Contains(result.Hint, "SIS session is not attaching") {
		t.Fatalf("hint = %q", result.Hint)
	}
}

// A banner followed by no reply at all means the SIS session did attach, so
// the advice must not point at the channel the way total silence does.
func TestDiagnoseSeparatesAGreetingFromTotalSilence(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{answersOn: "shell", ignoreAllCommands: true})

	result := New().Diagnose(smp.config)
	if result.OK || result.Stage != StageCommand {
		t.Fatalf("diagnosis = %#v", result)
	}
	if !strings.Contains(result.Summary, "answered neither query") {
		t.Fatalf("summary = %q", result.Summary)
	}
	if !strings.Contains(result.Hint, "Its banner arrived over the plain shell") {
		t.Fatalf("hint = %q", result.Hint)
	}
}

// A unit that answers 46I but ignores the escape-prefixed form has a working
// SIS session, which is a different repair from a channel that never carries
// one, so the test connection has to separate the two.
func TestDiagnoseSeparatesIgnoredEscapeCommands(t *testing.T) {
	smp := startFakeSMP(t, fakeSMP{answersOn: "shell", ignoreEscapeCommands: true})

	result := New().Diagnose(smp.config)
	if result.OK || result.Stage != StageCommand {
		t.Fatalf("diagnosis = %#v", result)
	}
	if !strings.Contains(result.Summary, "plain SIS commands work") {
		t.Fatalf("summary = %q", result.Summary)
	}
	if !strings.Contains(result.Detail, "46I") {
		t.Fatalf("detail = %q, want the plain command that answered", result.Detail)
	}
}

// fakeSMP stands in for the SSH side of an SMP 351.
type fakeSMP struct {
	// answersOn is the channel setup that carries the SIS session: "shell",
	// "pty-shell", or "exec". Any other setup is accepted and then ignored,
	// which is how the unit behaves when its SIS session does not attach.
	answersOn string
	// closeChannels hangs up instead of holding an accepted channel open.
	closeChannels bool
	// bannerOnStderr moves the copyright banner to the channel's error
	// stream, where some firmware puts it.
	bannerOnStderr bool
	// ignoreEscapeCommands drops commands that start with the escape byte and
	// answers the plain ones.
	ignoreEscapeCommands bool
	// ignoreAllCommands sends the banner and then nothing further.
	ignoreAllCommands bool
}

type fakeSMPServer struct {
	config   config.Config
	firmware fakeSMP

	mu       sync.Mutex
	channels []string
}

func startFakeSMP(t *testing.T, firmware fakeSMP) *fakeSMPServer {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	serverConfig.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	server := &fakeSMPServer{firmware: firmware}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn, serverConfig)
		}
	}()

	host, portString, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	server.config = config.Default()
	server.config.SmpHost = host
	server.config.SmpSSHPort = port
	server.config.SmpUsername = "admin"
	server.config.SmpPassword = "secret"
	return server
}

func (s *fakeSMPServer) channelsOpened() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.channels...)
}

func (s *fakeSMPServer) serve(conn net.Conn, serverConfig *ssh.ServerConfig) {
	defer conn.Close()
	sshConn, newChannels, requests, err := ssh.NewServerConn(conn, serverConfig)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range newChannels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go s.serveSession(channel, channelRequests)
	}
}

func (s *fakeSMPServer) serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	terminal := false
	for request := range requests {
		switch request.Type {
		case "pty-req":
			terminal = true
			_ = request.Reply(true, nil)
		case "shell":
			_ = request.Reply(true, nil)
			setup := "shell"
			if terminal {
				setup = "pty-shell"
			}
			if s.opened(setup) {
				s.converse(channel)
				return
			}
			if s.firmware.closeChannels {
				return
			}
		case "exec":
			_ = request.Reply(true, nil)
			if s.opened("exec") {
				var payload struct{ Command string }
				_ = ssh.Unmarshal(request.Payload, &payload)
				s.greet(channel)
				s.reply(channel, strings.TrimSuffix(payload.Command, "\r"))
				return
			}
			if s.firmware.closeChannels {
				return
			}
		default:
			_ = request.Reply(false, nil)
		}
	}
}

// opened records the channel setup and reports whether this firmware carries
// its SIS session on it. A setup it does not answer is left open and mute
// unless the firmware hangs up instead.
func (s *fakeSMPServer) opened(setup string) bool {
	s.mu.Lock()
	s.channels = append(s.channels, setup)
	s.mu.Unlock()
	return setup == s.firmware.answersOn
}

func (s *fakeSMPServer) converse(channel ssh.Channel) {
	s.greet(channel)
	buffered := ""
	chunk := make([]byte, 256)
	for {
		read, err := channel.Read(chunk)
		if read > 0 {
			buffered += string(chunk[:read])
			for {
				command, rest, ok := nextFakeCommand(buffered)
				if !ok {
					break
				}
				buffered = rest
				s.reply(channel, command)
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *fakeSMPServer) greet(channel ssh.Channel) {
	banner := "(c) Copyright 2014-2019, Extron Electronics, SMP 351, V2.11, 60-1324-01\r\n" +
		"Sat, 12 Sep 2026 13:04:11\r\n"
	if s.firmware.bannerOnStderr {
		_, _ = io.WriteString(channel.Stderr(), banner)
		return
	}
	_, _ = io.WriteString(channel, banner)
}

func (s *fakeSMPServer) reply(channel ssh.Channel, command string) {
	if s.firmware.ignoreAllCommands {
		return
	}
	if s.firmware.ignoreEscapeCommands && strings.HasPrefix(command, "\x1b") {
		return
	}
	response, ok := fakeSISReply(command)
	if !ok {
		response = "E13"
	}
	_, _ = io.WriteString(channel, response+"\r\n")
}

// nextFakeCommand takes one command off the stream. Commands arrive either
// terminated by a carriage return or, for the basic forms such as 46I, with no
// terminator at all.
func nextFakeCommand(buffered string) (string, string, bool) {
	if index := strings.IndexByte(buffered, '\r'); index >= 0 {
		return buffered[:index], buffered[index+1:], true
	}
	if _, ok := fakeSISReply(buffered); ok {
		return buffered, "", true
	}
	return "", "", false
}

func fakeSISReply(command string) (string, bool) {
	if strings.HasPrefix(command, "\x1b") {
		command += "\r"
	}
	response := responseForCommand(command)
	if response == "E10" {
		return "", false
	}
	return response, true
}

func startTestSSHServer(
	t *testing.T,
	configure func(*ssh.ServerConfig),
) (config.Config, <-chan error) {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{}
	serverConfig.AddHostKey(signer)
	configure(serverConfig)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		sshConn, channels, requests, err := ssh.NewServerConn(conn, serverConfig)
		if err != nil {
			done <- err
			return
		}
		defer sshConn.Close()
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			_ = channel.Reject(ssh.UnknownChannelType, "test server does not accept channels")
		}
		done <- nil
	}()

	host, portString, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SmpHost = host
	cfg.SmpSSHPort = port
	return cfg, done
}
