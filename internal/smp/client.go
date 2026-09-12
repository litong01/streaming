package smp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"

	"streaming/internal/config"
)

const (
	connectTimeout     = 10 * time.Second
	readTimeout        = 5 * time.Second
	webProbeTimeout    = 2 * time.Second
	greetingWindow     = 1 * time.Second
	probeWindow        = 2 * time.Second
	partialLineWindow  = 500 * time.Millisecond
	archiveStreamIndex = 1
)

var (
	streamEnabledRegex  = regexp.MustCompile(`(?im)^(?:Strc\d+\*)?([01])\r?$`)
	selectedPresetRegex = regexp.MustCompile(`,\s*(\d+)\*`)
	singlePresetRegex   = regexp.MustCompile(`^(\d+)\*`)
	sisErrorRegex       = regexp.MustCompile(`(?i)^E(10|12|13|14|17|18|22|24|26|28)$`)
	presetResponseRegex = regexp.MustCompile(`^\d+\*`)
	presetRecallRegex   = regexp.MustCompile(`(?i)^3Rpr(\d+)\*(\d+)$`)
)

// startConfirmWait is how long a start waits before reading the encoder back.
// Measured on firmware 2.11, the six commands of a language switch complete in
// about two and a half seconds, and an encoder that is going to fall over does
// so shortly after. It is a variable so tests need not wait.
var startConfirmWait = 3 * time.Second

type ActiveStream string

const (
	ActiveNone     ActiveStream = "none"
	ActiveEnglish  ActiveStream = "english"
	ActiveMandarin ActiveStream = "mandarin"
)

type State struct {
	ActiveStream     ActiveStream `json:"activeStream"`
	StreamEnabled    bool         `json:"streamEnabled"`
	ActivePreset     *int         `json:"activePreset"`
	StatusMessage    string       `json:"statusMessage"`
	LastError        *string      `json:"lastError"`
	SmpReachable     bool         `json:"smpReachable"`
	QueriedAtEpochMs int64        `json:"queriedAtEpochMs"`
}

type commandSender func(config.Config, string, func(string) bool) (string, error)

type Client struct {
	// The SMP 351 keeps only a couple of SIS sessions. A second login that
	// arrives while another is open is answered with E22 or with nothing at
	// all, so the buttons, the poller, and the connection test take turns
	// through this lock instead of competing for the unit.
	mu   sync.Mutex
	send commandSender
	// transport is the SSH channel type this unit last answered on. It is
	// remembered between logins because the poller opens a fresh one every
	// few seconds and each wrong guess costs a read timeout.
	transport sisTransport
	settled   bool
}

func New() *Client {
	return &Client{}
}

// QueryState reads the SMP's current state. The second result is false when
// another exchange is already in flight: the unit answers one session at a
// time, so a poll steps aside rather than queueing behind a button press or a
// connection test and reporting a reading that is by then seconds old.
func (c *Client) QueryState(cfg config.Config) (State, bool) {
	if !cfg.IsSmpConfigured() {
		return notConfigured(), true
	}
	if !c.mu.TryLock() {
		return State{}, false
	}
	defer c.mu.Unlock()
	actionClient, closeAction, err := c.actionClient(cfg)
	if err != nil {
		return failed("SMP is not reachable", err), true
	}
	defer closeAction()
	return actionClient.queryState(cfg), true
}

func (c *Client) queryState(cfg config.Config) State {
	enabled, err := c.queryStreamEnabled(cfg, cfg.StreamIndex)
	if err != nil {
		return failed("SMP is not reachable", err)
	}
	preset, err := c.queryActiveStreamingPreset(cfg, cfg.StreamIndex)
	if err != nil {
		return failed("SMP is not reachable", err)
	}

	active := ActiveNone
	status := "Idle"
	switch {
	case enabled && preset != nil && *preset == cfg.EnglishPreset:
		active = ActiveEnglish
		status = fmt.Sprintf("Streaming English (preset %d)", cfg.EnglishPreset)
	case enabled && preset != nil && *preset == cfg.MandarinPreset:
		active = ActiveMandarin
		status = fmt.Sprintf("Streaming Mandarin (preset %d)", cfg.MandarinPreset)
	case enabled:
		label := "unknown"
		if preset != nil {
			label = strconv.Itoa(*preset)
		}
		status = fmt.Sprintf("Streaming (preset %s)", label)
	}

	return State{
		ActiveStream:     active,
		StreamEnabled:    enabled,
		ActivePreset:     preset,
		StatusMessage:    status,
		SmpReachable:     true,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}

func (c *Client) StartEnglish(cfg config.Config) State {
	return c.start(cfg, cfg.EnglishPreset, ActiveEnglish)
}

func (c *Client) StartMandarin(cfg config.Config) State {
	return c.start(cfg, cfg.MandarinPreset, ActiveMandarin)
}

func (c *Client) Stop(cfg config.Config) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	actionClient, closeAction, err := c.actionClient(cfg)
	if err != nil {
		return failed("Stop failed", err)
	}
	defer closeAction()
	if err := actionClient.stopArchive(cfg); err != nil {
		return failed("Stop failed", err)
	}
	return actionClient.queryState(cfg)
}

func (c *Client) start(cfg config.Config, preset int, expected ActiveStream) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	actionClient, closeAction, err := c.actionClient(cfg)
	if err != nil {
		return failed("Start failed", err)
	}
	defer closeAction()
	return actionClient.startPreset(cfg, preset, expected)
}

func (c *Client) startPreset(cfg config.Config, preset int, expected ActiveStream) State {
	// Only the Archive encoder is ever touched. The Confidence encoder is left
	// exactly as the operator configured it on the unit, because the preview
	// no longer depends on it.
	//
	// Read before disturbing anything: recalling a preset means switching the
	// encoder off first, so an encoder already running what was asked for is
	// left alone rather than having its push interrupted.
	archive, err := c.readStream(cfg, archiveStreamIndex)
	if err != nil {
		return failed("Start failed", err)
	}
	if archive.running(preset) {
		return alreadyStreaming(preset, expected)
	}
	if err := c.restartStream(cfg, archiveStreamIndex, preset); err != nil {
		return failed("Start failed", err)
	}

	// The unit reports an encoder as enabled the moment it is switched on,
	// which is before the destination has been contacted. A push that is
	// going to be refused drops the encoder again a second or two later, so
	// the result is confirmed rather than announced on trust.
	time.Sleep(startConfirmWait)

	state := c.queryState(cfg)
	if state.LastError != nil {
		return state
	}
	if !state.StreamEnabled {
		return failed("Start failed", fmt.Errorf(
			"the Archive encoder stopped within %s of starting; check that preset %d has a reachable destination",
			startConfirmWait, preset,
		))
	}
	state.ActiveStream = expected
	state.StatusMessage = languageStatus(expected, preset)
	return state
}

// restartStream puts one encoder onto a preset. It is switched off first
// because the SMP refuses to rewrite a destination while that encoder is live.
func (c *Client) restartStream(cfg config.Config, streamIndex, preset int) error {
	const name = "Archive"
	if _, err := c.setStreamEnabled(cfg, streamIndex, false); err != nil {
		return fmt.Errorf("stop %s encoder: %w", name, err)
	}
	if _, err := c.recallStreamingPreset(cfg, streamIndex, preset); err != nil {
		return fmt.Errorf("recall %s preset %d: %w", name, preset, err)
	}
	if _, err := c.setStreamEnabled(cfg, streamIndex, true); err != nil {
		return fmt.Errorf("start %s encoder: %w", name, err)
	}
	return nil
}

// streamState is one encoder's live configuration, which the SMP reports as
// two separate answers: whether it is switched on, and which saved preset its
// configuration still matches.
type streamState struct {
	enabled bool
	preset  *int
}

// running reports whether this encoder is already streaming the given preset.
// The SMP answers a preset query with "0*modified, not saved" once the live
// configuration has drifted from the saved preset, and switching an encoder
// off is itself enough to cause that, so a match means the configuration
// really is the preset's rather than merely having been selected once.
func (s streamState) running(preset int) bool {
	return s.enabled && s.preset != nil && *s.preset == preset
}

func (c *Client) readStream(cfg config.Config, streamIndex int) (streamState, error) {
	const name = "Archive"
	enabled, err := c.queryStreamEnabled(cfg, streamIndex)
	if err != nil {
		return streamState{}, fmt.Errorf("read %s encoder: %w", name, err)
	}
	preset, err := c.queryActiveStreamingPreset(cfg, streamIndex)
	if err != nil {
		return streamState{}, fmt.Errorf("read %s preset: %w", name, err)
	}
	return streamState{enabled: enabled, preset: preset}, nil
}

func languageStatus(stream ActiveStream, preset int) string {
	if stream == ActiveEnglish {
		return fmt.Sprintf("Streaming English (preset %d)", preset)
	}
	return fmt.Sprintf("Streaming Mandarin (preset %d)", preset)
}

// alreadyStreaming reports the state of a stream that was found running what
// was asked for, so nothing was sent to the unit and nothing needs confirming.
func alreadyStreaming(preset int, expected ActiveStream) State {
	return State{
		ActiveStream:     expected,
		StreamEnabled:    true,
		ActivePreset:     &preset,
		StatusMessage:    languageStatus(expected, preset),
		SmpReachable:     true,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}

// stopArchive switches off the encoder that pushes to YouTube. The Confidence
// encoder is deliberately left untouched.
func (c *Client) stopArchive(cfg config.Config) error {
	if _, err := c.setStreamEnabled(cfg, archiveStreamIndex, false); err != nil {
		return fmt.Errorf("stop Archive encoder: %w", err)
	}
	return nil
}

func (c *Client) recallStreamingPreset(cfg config.Config, streamIndex, preset int) (string, error) {
	return c.sendCommand(cfg, streamingPresetRecallCommand(streamIndex, preset), func(line string) bool {
		return matchesPresetRecall(line, streamIndex, preset)
	})
}

// matchesPresetRecall compares the numbers rather than the text, because the
// SMP 351 pads both of them to two digits: 3*3*3. is answered with 3Rpr03*03,
// not the 3Rpr3*3 that the command itself suggests.
func matchesPresetRecall(line string, streamIndex, preset int) bool {
	match := presetRecallRegex.FindStringSubmatch(strings.TrimSpace(line))
	if len(match) != 3 {
		return false
	}
	stream, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	recalled, err := strconv.Atoi(match[2])
	if err != nil {
		return false
	}
	return stream == streamIndex && recalled == preset
}

func (c *Client) setStreamEnabled(cfg config.Config, streamIndex int, enabled bool) (string, error) {
	value := 0
	if enabled {
		value = 1
	}
	expected := fmt.Sprintf("Strc%d*%d", streamIndex, value)
	return c.sendCommand(cfg, streamEnabledCommand(streamIndex, value), func(line string) bool {
		if strings.EqualFold(line, expected) {
			return true
		}
		match := streamEnabledRegex.FindStringSubmatch(line)
		return len(match) == 2 && match[1] == strconv.Itoa(value)
	})
}

func (c *Client) queryStreamEnabled(cfg config.Config, streamIndex int) (bool, error) {
	response, err := c.sendCommand(cfg, streamEnabledQuery(streamIndex), matchesStreamStatus)
	if err != nil {
		return false, err
	}
	match := streamEnabledRegex.FindStringSubmatch(response)
	if len(match) != 2 {
		return false, fmt.Errorf("unexpected stream status response %q", printable(response))
	}
	return match[1] == "1", nil
}

func (c *Client) queryActiveStreamingPreset(cfg config.Config, streamIndex int) (*int, error) {
	response, err := c.sendCommand(cfg, streamingPresetQueryCommand(streamIndex), func(line string) bool {
		return presetResponseRegex.MatchString(line)
	})
	if err != nil {
		return nil, err
	}
	return parseSelectedStreamingPreset(response), nil
}

// Stage names the last step of the connection that was reached, so the
// configuration page can say whether the network, the SSH service, the
// credentials, or the SIS layer is at fault.
type Stage string

const (
	StageAddress   Stage = "address"
	StageNetwork   Stage = "network"
	StageHandshake Stage = "handshake"
	StageAuth      Stage = "auth"
	StageCommand   Stage = "command"
	StageOK        Stage = "ok"
)

type Diagnosis struct {
	OK      bool   `json:"ok"`
	Stage   Stage  `json:"stage"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
	Address string `json:"address"`
	Hint    string `json:"hint"`
}

// Diagnose walks the connection one layer at a time so a failure can be
// attributed precisely. It never touches stored configuration.
func (c *Client) Diagnose(cfg config.Config) Diagnosis {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(cfg.SmpHost) == "" {
		return Diagnosis{Stage: StageAddress, Summary: "Enter the SMP address first."}
	}
	if strings.TrimSpace(cfg.SmpUsername) == "" {
		return Diagnosis{Stage: StageAddress, Summary: "Enter the SSH username first."}
	}
	addr := smpAddress(cfg)

	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return networkDiagnosis(cfg, addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(connectTimeout)); err != nil {
		return Diagnosis{Stage: StageNetwork, Address: addr, Summary: "Could not set a connection deadline.", Detail: err.Error()}
	}

	sshConn, channels, requests, err := ssh.NewClientConn(conn, addr, clientConfig(cfg))
	if err != nil {
		return sshDiagnosis(addr, err)
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer client.Close()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return Diagnosis{Stage: StageHandshake, Address: addr, Summary: "Could not clear the connection deadline.", Detail: err.Error()}
	}

	return c.commandDiagnosis(client, cfg, addr)
}

// commandDiagnosis asks the same question over every channel type the SMP
// might expect, and with a plain command as well as an escape-prefixed one.
// Which combination answers is what separates "the SIS session never attached
// to this channel" from "the SMP ignored these command bytes", and the two are
// fixed in different places.
func (c *Client) commandDiagnosis(client *ssh.Client, cfg config.Config, addr string) Diagnosis {
	statusQuery := streamEnabledQuery(cfg.StreamIndex)
	presetQuery := streamingPresetQueryCommand(cfg.StreamIndex)
	var probes []sisProbe

	for _, transport := range sisTransports {
		status := probeSIS(client, transport, statusQuery)
		probes = append(probes, status)
		if matchesAnyLine(status.reply, matchesStreamStatus) {
			// A successful test saves the buttons from repeating the search.
			c.transport, c.settled = transport, true
			return Diagnosis{
				OK:      true,
				Stage:   StageOK,
				Address: addr,
				Summary: addr + " answered correctly.",
				Detail:  "over the " + transport.String() + ", received: " + printable(status.reply),
			}
		}
		// A unit that says nothing at all on this channel has not started its
		// SIS session, so a second command would be shouted into the same
		// void. Only a channel that produced something is worth asking again.
		if !status.spoke() {
			continue
		}
		preset := probeSIS(client, transport, presetQuery)
		probes = append(probes, preset)
		if matchesAnyLine(preset.reply, presetResponseRegex.MatchString) {
			return Diagnosis{
				Stage:   StageCommand,
				Address: addr,
				Summary: "Signed in to " + addr + ", and plain SIS commands work, but escape-prefixed ones are ignored.",
				Detail:  probeTranscript(probes),
				Hint: "The SMP answered " + printable(presetQuery) + " over the " + transport.String() +
					" but ignored " + printable(statusQuery) + ". The escape byte is being swallowed on the way in, " +
					"which is a client-side fix rather than anything to change on the SMP.",
			}
		}
	}

	if unexpected, ok := firstReply(probes); ok {
		return Diagnosis{
			Stage:   StageCommand,
			Address: addr,
			Summary: "Signed in to " + addr + ", but the reply was not the expected SIS response.",
			Detail:  probeTranscript(probes),
			Hint:    "Everything up to the SIS command layer works, and the SMP is talking over the " + unexpected.transport.String() + ". Send me the received text above and I can correct the command format.",
		}
	}

	if greeter, ok := firstGreeting(probes); ok {
		return Diagnosis{
			Stage:   StageCommand,
			Address: addr,
			Summary: "Signed in to " + addr + ", but the SMP answered neither query.",
			Detail:  probeTranscript(probes),
			Hint: "Its banner arrived over the " + greeter.transport.String() + ", so the login and the channel are right and " +
				"the unit is discarding the commands themselves. A plain query went unanswered too, which points at SIS commands " +
				"being disabled for this account rather than at the command format.",
		}
	}

	return Diagnosis{
		Stage:   StageCommand,
		Address: addr,
		Summary: "Signed in to " + addr + ", but the SMP sent nothing back on any SSH channel.",
		Detail:  probeTranscript(probes),
		Hint: "The SMP never even sent its copyright banner, so its SIS session is not attaching to the login rather than " +
			"misreading the commands. Check that this account has SIS/Telnet rights and not web-only access, and that no other " +
			"SSH session, Toolbelt window, or second copy of this app is holding the unit's one SIS connection.",
	}
}

// sisProbe records what one channel type produced for one command. The
// greeting is kept apart from the reply because the SMP sends its banner
// unprompted: a banner with no reply means the command was ignored, while
// silence in both means the channel itself never carried a SIS session.
type sisProbe struct {
	transport sisTransport
	command   string
	greeting  string
	reply     string
	err       error
}

// spoke reports whether the SMP produced anything at all on this channel.
func (p sisProbe) spoke() bool {
	return strings.TrimSpace(p.greeting+p.reply) != ""
}

// probeSIS collects everything the SMP sends for a fixed window instead of
// waiting for a terminator, so a protocol mismatch is visible rather than
// showing up as a bare timeout.
func probeSIS(client *ssh.Client, transport sisTransport, command string) sisProbe {
	probe := sisProbe{transport: transport, command: command}
	channel, err := openSISChannel(client, transport, command)
	if err != nil {
		probe.err = err
		return probe
	}
	defer channel.Close()

	output := collectOutput(channel.output)
	// An exec channel carries the command at open time, so its greeting and
	// its reply arrive mixed together and cannot be told apart.
	if channel.carriesCommand {
		output.wait(greetingWindow + probeWindow)
		probe.reply = output.take()
		return probe
	}

	output.wait(greetingWindow)
	probe.greeting = output.take()
	if err := channel.write(command); err != nil {
		probe.err = err
		return probe
	}
	output.wait(probeWindow)
	probe.reply = output.take()
	return probe
}

func matchesAnyLine(raw string, matches func(string) bool) bool {
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if line = strings.TrimSpace(line); line != "" && matches(line) {
			return true
		}
	}
	return false
}

func firstReply(probes []sisProbe) (sisProbe, bool) {
	for _, probe := range probes {
		if strings.TrimSpace(probe.reply) != "" {
			return probe, true
		}
	}
	return sisProbe{}, false
}

func firstGreeting(probes []sisProbe) (sisProbe, bool) {
	for _, probe := range probes {
		if strings.TrimSpace(probe.greeting) != "" {
			return probe, true
		}
	}
	return sisProbe{}, false
}

func probeTranscript(probes []sisProbe) string {
	parts := make([]string, 0, len(probes))
	for _, probe := range probes {
		part := probe.transport.String() + ", sent " + printable(probe.command) + ": "
		switch {
		case probe.err != nil:
			part += probe.err.Error()
		case !probe.spoke():
			part += "nothing"
		default:
			part += "received " + printable(probe.greeting+probe.reply)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}

// outputCollector accumulates whatever arrives so a probe can report the raw
// exchange, including output that turns up with no terminator.
type outputCollector struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func collectOutput(r io.Reader) *outputCollector {
	collector := &outputCollector{done: make(chan struct{})}
	go func() {
		defer close(collector.done)
		chunk := make([]byte, 1024)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				collector.mu.Lock()
				collector.buf.Write(chunk[:n])
				collector.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return collector
}

// wait stops early when the SMP closes the channel, which is as final as the
// window running out.
func (c *outputCollector) wait(window time.Duration) {
	select {
	case <-c.done:
	case <-time.After(window):
	}
}

func (c *outputCollector) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	collected := c.buf.String()
	c.buf.Reset()
	return collected
}

// printable keeps the invisible bytes of an Extron exchange visible, since the
// protocol leans on carriage returns and escape characters.
func printable(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch {
		case r == '\r':
			out.WriteString("<CR>")
		case r == '\n':
			out.WriteString("<LF>")
		case r == 0x1b:
			out.WriteString("<ESC>")
		case unicode.IsPrint(r):
			out.WriteRune(r)
		default:
			fmt.Fprintf(&out, "<%02X>", r)
		}
	}
	return out.String()
}

// In Extron SIS tables, E means the ESC byte and } means carriage return.
// They are notation, not literal characters. Basic commands such as 46I and
// preset recall use their final command character as the terminator.
func streamEnabledCommand(streamIndex, enabled int) string {
	return fmt.Sprintf("\x1b%d*%dSTRC\r", streamIndex, enabled)
}

func streamEnabledQuery(streamIndex int) string {
	return fmt.Sprintf("\x1b%dSTRC\r", streamIndex)
}

func matchesStreamStatus(line string) bool {
	return streamEnabledRegex.MatchString(line)
}

func streamingPresetRecallCommand(streamIndex, preset int) string {
	return fmt.Sprintf("3*%d*%d.", streamIndex, preset)
}

func networkDiagnosis(cfg config.Config, addr string, err error) Diagnosis {
	result := Diagnosis{Stage: StageNetwork, Address: addr, Detail: err.Error()}

	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		result.Stage = StageAddress
		result.Summary = "The SMP host name could not be resolved."
		result.Hint = "Use the SMP's IP address instead of a name, or check this device's DNS settings."
		return result
	}

	var netError net.Error
	switch {
	case errors.As(err, &netError) && netError.Timeout():
		result.Summary = "No reply from " + addr + " within 10 seconds."
	case errors.Is(err, syscall.ECONNREFUSED):
		result.Summary = addr + " actively refused the connection."
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		result.Summary = "This device has no route to " + addr + "."
	default:
		result.Summary = "Could not open a connection to " + addr + "."
	}

	// Comparing against the SMP's web port separates "this device cannot reach
	// the SMP at all" from "only the SIS port is blocked", which are fixed in
	// completely different places.
	if webPort, ok := probeWebPort(cfg.SmpHost); ok {
		result.Hint = "The SMP answers on port " + webPort + " from this device, so the unit itself is reachable and port " +
			strconv.Itoa(cfg.SmpSSHPort) + " alone is failing. Check that SSH is enabled on the SMP, not just that the port number is listed, and that no network rule filters that port."
	} else {
		result.Hint = "The SMP does not answer on its web port from this device either, so nothing can reach the unit. Compare this device's IP address and subnet with the SMP's."
	}
	return result
}

func probeWebPort(host string) (string, bool) {
	for _, port := range []string{"80", "443"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), webProbeTimeout)
		if err == nil {
			_ = conn.Close()
			return port, true
		}
	}
	return "", false
}

func sshDiagnosis(addr string, err error) Diagnosis {
	result := Diagnosis{Stage: StageHandshake, Address: addr, Detail: err.Error()}
	message := strings.ToLower(err.Error())

	switch {
	case strings.Contains(message, "unable to authenticate"),
		strings.Contains(message, "no supported methods remain"):
		result.Stage = StageAuth
		result.Summary = "Reached SSH on " + addr + ", but the username or password was rejected."
		result.Hint = "The network path is fine. Re-enter the password, since a blank password box keeps the previously saved one."
	case strings.Contains(message, "no common algorithm"),
		strings.Contains(message, "algorithm negotiation"):
		result.Summary = "Reached SSH on " + addr + ", but no shared encryption algorithm."
		result.Hint = "The SMP firmware only offers algorithms this SSH client has retired. This is fixable in the client configuration."
	default:
		result.Summary = "Connected to " + addr + ", but the SSH handshake failed."
	}
	return result
}

func (c *Client) sendCommand(
	cfg config.Config,
	command string,
	matchesResponse func(string) bool,
) (string, error) {
	if c.send != nil {
		return c.send(cfg, command, matchesResponse)
	}
	client, err := dialClient(cfg)
	if err != nil {
		return "", err
	}
	defer client.Close()
	conn := c.connection(client)
	defer c.remember(conn)
	return conn.sendCommand(command, matchesResponse)
}

func (c *Client) connection(client *ssh.Client) *connection {
	return &connection{client: client, transport: c.transport, settled: c.settled}
}

// remember carries what the last login learned into the next one. The caller
// holds the lock for the whole exchange, so no other operation can be reading
// these at the same time.
func (c *Client) remember(conn *connection) {
	c.transport, c.settled = conn.transport, conn.settled
}

// actionClient keeps every command from one button press on a single SSH
// connection. The SMP 351 has a low connection limit and can report E22/E26
// when several short-lived logins arrive back-to-back.
func (c *Client) actionClient(cfg config.Config) (*Client, func(), error) {
	if c.send != nil {
		return c, func() {}, nil
	}
	client, err := dialClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	conn := c.connection(client)
	action := &Client{send: func(
		_ config.Config,
		command string,
		matchesResponse func(string) bool,
	) (string, error) {
		response, err := conn.sendCommand(command, matchesResponse)
		c.remember(conn)
		return response, err
	}}
	return action, func() { _ = client.Close() }, nil
}

// sisTransport is one way of asking the SMP to attach its SIS session to an
// SSH channel. Firmware disagrees about this: a channel can open cleanly and
// then stay silent because the unit wanted a terminal, or an interactive
// shell, so the type that answers is discovered rather than assumed.
type sisTransport int

const (
	shellTransport sisTransport = iota
	terminalTransport
	execTransport
)

var sisTransports = []sisTransport{shellTransport, terminalTransport, execTransport}

func (t sisTransport) String() string {
	switch t {
	case terminalTransport:
		return "shell with a terminal"
	case execTransport:
		return "exec channel"
	default:
		return "plain shell"
	}
}

// connection is one SSH login plus the channel type this unit turned out to
// answer on, so the search is paid for once per login instead of once per
// command.
type connection struct {
	client    *ssh.Client
	transport sisTransport
	settled   bool
}

func (conn *connection) sendCommand(command string, matchesResponse func(string) bool) (string, error) {
	if conn.settled {
		response, err := runCommand(conn.client, conn.transport, command, matchesResponse)
		if !mute(err) {
			return sisResponse(response, err)
		}
		// The channel type that used to work has gone quiet, which happens
		// after a firmware update or a reboot, so look for it again.
		conn.settled = false
	}

	var attempts []error
	for _, transport := range sisTransports {
		response, err := runCommand(conn.client, transport, command, matchesResponse)
		if mute(err) {
			attempts = append(attempts, fmt.Errorf("%s: %w", transport, err))
			continue
		}
		// Any other outcome, an SIS error included, proves this channel
		// carries the SIS session, so the rest of the commands can use it.
		conn.transport, conn.settled = transport, true
		return sisResponse(response, err)
	}
	return "", fmt.Errorf(
		"the SMP accepted the login but sent nothing back on any SSH channel type: %w",
		errors.Join(attempts...),
	)
}

func sisResponse(response string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if match := sisErrorRegex.FindStringSubmatch(response); len(match) == 2 {
		return "", fmt.Errorf("SMP returned SIS error E%s", match[1])
	}
	return response, nil
}

// sisChannel is an open SSH channel with the SMP's two output streams merged.
// The unit puts its banner and some errors on stderr, which a stdout-only
// reader cannot tell apart from silence.
type sisChannel struct {
	session *ssh.Session
	output  io.ReadCloser
	stdin   io.WriteCloser
	// carriesCommand is set when the channel type delivered the command as it
	// opened, leaving nothing to write.
	carriesCommand bool
}

func (s *sisChannel) write(command string) error {
	if s.carriesCommand {
		return nil
	}
	_, err := io.WriteString(s.stdin, command)
	return err
}

// Close releases the merged reader before the session, because the copies
// feeding it would otherwise stay parked on a write that nobody is going to
// read. The server runs for weeks between restarts and polls every few
// seconds, so a leak here would accumulate.
func (s *sisChannel) Close() {
	_ = s.output.Close()
	_ = s.session.Close()
}

// errChannelRejected marks a channel that never opened, which says the
// transport is wrong rather than the command.
var errChannelRejected = errors.New("SMP rejected this SSH channel")

func openSISChannel(client *ssh.Client, transport sisTransport, command string) (*sisChannel, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errChannelRejected, err)
	}
	channel, err := attachSIS(session, transport, command)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("%w: %v", errChannelRejected, err)
	}
	return channel, nil
}

func attachSIS(session *ssh.Session, transport sisTransport, command string) (*sisChannel, error) {
	if transport == terminalTransport {
		// Echo is switched off because a unit that hands out a terminal also
		// mirrors the command back, which would otherwise be read as a reply.
		modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 9600, ssh.TTY_OP_OSPEED: 9600}
		if err := session.RequestPty("vt100", 24, 80, modes); err != nil {
			return nil, err
		}
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return nil, err
	}
	channel := &sisChannel{
		session:        session,
		output:         mergeOutput(stdout, stderr),
		stdin:          stdin,
		carriesCommand: transport == execTransport,
	}
	start := session.Shell
	if channel.carriesCommand {
		start = func() error { return session.Start(command) }
	}
	if err := start(); err != nil {
		channel.Close()
		return nil, err
	}
	return channel, nil
}

// mergeOutput reads both streams at once. io.MultiReader would block on the
// first one until it ended, which on an interactive channel is never.
func mergeOutput(streams ...io.Reader) io.ReadCloser {
	reader, writer := io.Pipe()
	var open sync.WaitGroup
	open.Add(len(streams))
	for _, stream := range streams {
		go func() {
			defer open.Done()
			_, _ = io.Copy(writer, stream)
		}()
	}
	go func() {
		open.Wait()
		_ = writer.Close()
	}()
	return reader
}

// smpAddress keeps IPv6 hosts usable: ParseHostPort stores them without
// brackets, which a plain host:port concatenation would mangle.
func smpAddress(cfg config.Config) string {
	return net.JoinHostPort(cfg.SmpHost, strconv.Itoa(cfg.SmpSSHPort))
}

func clientConfig(cfg config.Config) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: strings.TrimSpace(cfg.SmpUsername),
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.SmpPassword),
			ssh.KeyboardInteractive(passwordChallenge(cfg.SmpPassword)),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         connectTimeout,
	}
}

func passwordChallenge(password string) ssh.KeyboardInteractiveChallenge {
	return func(_ string, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for index, question := range questions {
			if strings.Contains(strings.ToLower(question), "password") {
				answers[index] = password
			}
		}
		return answers, nil
	}
}

// dialClient applies the timeout to both the TCP connection and SSH
// authentication. ssh.Dial only bounds the TCP portion.
func dialClient(cfg config.Config) (*ssh.Client, error) {
	addr := smpAddress(cfg)
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(connectTimeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, addr, clientConfig(cfg))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, err
	}
	return ssh.NewClient(sshConn, channels, requests), nil
}

func runCommand(
	client *ssh.Client,
	transport sisTransport,
	command string,
	matchesResponse func(string) bool,
) (string, error) {
	channel, err := openSISChannel(client, transport, command)
	if err != nil {
		return "", err
	}
	defer channel.Close()

	// A channel that will not take the command never carried the SIS session,
	// which is the same dead end as one that was refused outright.
	if err := channel.write(command); err != nil {
		return "", fmt.Errorf("%w: %v", errChannelRejected, err)
	}
	return readUntilResponse(channel.output, readTimeout, matchesResponse)
}

// muteError means the channel produced no SIS text at all. That points at the
// channel or at the unit's session limit rather than at the command, so it is
// the one failure worth retrying on another channel type.
type muteError struct {
	transcript []string
	closed     bool
}

func (e *muteError) Error() string {
	received := printable(strings.Join(e.transcript, "\n"))
	if e.closed {
		return fmt.Sprintf("SMP closed the connection before replying; received %q", received)
	}
	return fmt.Sprintf("timed out waiting for SMP response; received %q", received)
}

func mute(err error) bool {
	if errors.Is(err, errChannelRejected) {
		return true
	}
	var silence *muteError
	return errors.As(err, &silence) && len(silence.transcript) == 0
}

// readUntilResponse ignores the copyright banner, timestamp, and any other
// unsolicited lines until it sees either the expected reply or an SIS error.
// Lines are cut on CR as well as LF, and a fragment that stops arriving is
// taken as complete, because some firmware leaves the last reply of a session
// without a terminator.
func readUntilResponse(
	r io.Reader,
	timeout time.Duration,
	matchesResponse func(string) bool,
) (string, error) {
	type event struct {
		data []byte
		err  error
	}
	events := make(chan event, 16)
	// A chatty unit can outrun the buffer, so the reader is told to give up
	// rather than sit on a send nobody will take once this call has returned.
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		chunk := make([]byte, 1024)
		send := func(current event) bool {
			select {
			case events <- current:
				return true
			case <-quit:
				return false
			}
		}
		for {
			n, err := r.Read(chunk)
			if n > 0 && !send(event{data: append([]byte(nil), chunk[:n]...)}) {
				return
			}
			if err != nil {
				send(event{err: err})
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	isResponse := func(line string) bool {
		return line != "" && (matchesResponse(line) || sisErrorRegex.MatchString(line))
	}

	var transcript []string
	var pending []byte
	var settled <-chan time.Time
	for {
		select {
		case current := <-events:
			if current.err != nil {
				fragment := strings.TrimSpace(string(pending))
				if isResponse(fragment) {
					return fragment, nil
				}
				if !errors.Is(current.err, io.EOF) {
					return "", current.err
				}
				return "", &muteError{transcript: appendLine(transcript, fragment), closed: true}
			}
			pending = append(pending, current.data...)
			var lines []string
			lines, pending = splitSISLines(pending)
			for _, line := range lines {
				if line = strings.TrimSpace(line); line == "" {
					continue
				}
				transcript = append(transcript, line)
				if isResponse(line) {
					return line, nil
				}
			}
			// An unterminated fragment is only judged once it stops growing,
			// since more of the line may still be on its way.
			settled = nil
			if strings.TrimSpace(string(pending)) != "" {
				settled = time.After(partialLineWindow)
			}
		case <-settled:
			settled = nil
			if fragment := strings.TrimSpace(string(pending)); isResponse(fragment) {
				return fragment, nil
			}
		case <-timer.C:
			fragment := strings.TrimSpace(string(pending))
			if isResponse(fragment) {
				return fragment, nil
			}
			return "", &muteError{transcript: appendLine(transcript, fragment)}
		}
	}
}

// splitSISLines cuts on both terminators the SMP mixes and hands back the
// trailing fragment that has none yet.
func splitSISLines(data []byte) ([]string, []byte) {
	var lines []string
	start := 0
	for index, current := range data {
		if current != '\r' && current != '\n' {
			continue
		}
		lines = append(lines, string(data[start:index]))
		start = index + 1
	}
	return lines, data[start:]
}

func appendLine(lines []string, line string) []string {
	if line == "" {
		return lines
	}
	return append(lines, line)
}

func streamingPresetQueryCommand(streamIndex int) string {
	switch streamIndex {
	case 2:
		return "47I"
	case 3:
		return "48I"
	default:
		return "46I"
	}
}

func parseSelectedStreamingPreset(response string) *int {
	line := strings.TrimSpace(response)
	if match := selectedPresetRegex.FindStringSubmatch(line); len(match) == 2 {
		return presetNumber(match[1])
	}
	if match := singlePresetRegex.FindStringSubmatch(line); len(match) == 2 {
		return presetNumber(match[1])
	}
	return nil
}

// presetNumber treats 0 as "none". The SMP 351 reports that as
// "0*modified, not saved" when the live stream does not match a saved preset.
func presetNumber(value string) *int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return nil
	}
	return &parsed
}

func notConfigured() State {
	msg := "Configure SMP host and credentials on the configuration page."
	return State{
		ActiveStream:     ActiveNone,
		StatusMessage:    "SMP not configured",
		LastError:        &msg,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}

func failed(message string, err error) State {
	detail := err.Error()
	return State{
		ActiveStream:     ActiveNone,
		StatusMessage:    message,
		LastError:        &detail,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}
