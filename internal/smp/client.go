package smp

import (
	"bufio"
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
	connectTimeout        = 10 * time.Second
	readTimeout           = 5 * time.Second
	webProbeTimeout       = 2 * time.Second
	probeWindow           = 3 * time.Second
	archiveStreamIndex    = 1
	confidenceStreamIndex = 3
)

var (
	streamEnabledRegex  = regexp.MustCompile(`(?im)^(?:Strc\d+\*)?([01])\r?$`)
	selectedPresetRegex = regexp.MustCompile(`,\s*(\d+)\*`)
	singlePresetRegex   = regexp.MustCompile(`^(\d+)\*`)
	sisErrorRegex       = regexp.MustCompile(`(?i)^E(10|12|13|14|17|18|22|24|26|28)$`)
	presetResponseRegex = regexp.MustCompile(`^\d+\*`)
)

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
	send commandSender
}

func New() *Client {
	return &Client{}
}

func (c *Client) QueryState(cfg config.Config) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	actionClient, closeAction, err := c.actionClient(cfg)
	if err != nil {
		return failed("SMP is not reachable", err)
	}
	defer closeAction()
	return actionClient.queryState(cfg)
}

func (c *Client) queryState(cfg config.Config) State {
	enabled, err := c.queryStreamEnabled(cfg)
	if err != nil {
		return failed("SMP is not reachable", err)
	}
	preset, err := c.queryActiveStreamingPreset(cfg)
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
	actionClient, closeAction, err := c.actionClient(cfg)
	if err != nil {
		return failed("Stop failed", err)
	}
	defer closeAction()
	if err := actionClient.stopBoth(cfg); err != nil {
		return failed("Stop failed", err)
	}
	return actionClient.queryState(cfg)
}

func (c *Client) start(cfg config.Config, preset int, expected ActiveStream) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	actionClient, closeAction, err := c.actionClient(cfg)
	if err != nil {
		return failed("Start failed", err)
	}
	defer closeAction()
	return actionClient.startPreset(cfg, preset, expected)
}

func (c *Client) startPreset(cfg config.Config, preset int, expected ActiveStream) State {
	if err := c.stopBoth(cfg); err != nil {
		return failed("Start failed", err)
	}
	if _, err := c.recallStreamingPreset(cfg, archiveStreamIndex, preset); err != nil {
		return failed("Start failed", fmt.Errorf("recall Archive preset %d: %w", preset, err))
	}
	if _, err := c.recallStreamingPreset(cfg, confidenceStreamIndex, cfg.ConfidencePreset); err != nil {
		return failed("Start failed", fmt.Errorf("recall Confidence preset %d: %w", cfg.ConfidencePreset, err))
	}
	if _, err := c.setStreamEnabled(cfg, confidenceStreamIndex, true); err != nil {
		return failed("Start failed", fmt.Errorf("start Confidence encoder: %w", err))
	}
	if _, err := c.setStreamEnabled(cfg, archiveStreamIndex, true); err != nil {
		// Do not leave the preview encoder running after a partial start.
		_, _ = c.setStreamEnabled(cfg, confidenceStreamIndex, false)
		return failed("Start failed", fmt.Errorf("start Archive encoder: %w", err))
	}

	state := c.queryState(cfg)
	if state.StreamEnabled {
		state.ActiveStream = expected
		if expected == ActiveEnglish {
			state.StatusMessage = fmt.Sprintf("Streaming English (preset %d)", preset)
		} else {
			state.StatusMessage = fmt.Sprintf("Streaming Mandarin (preset %d)", preset)
		}
	}
	return state
}

func (c *Client) stopBoth(cfg config.Config) error {
	_, archiveErr := c.setStreamEnabled(cfg, archiveStreamIndex, false)
	_, confidenceErr := c.setStreamEnabled(cfg, confidenceStreamIndex, false)
	if archiveErr != nil {
		archiveErr = fmt.Errorf("stop Archive encoder: %w", archiveErr)
	}
	if confidenceErr != nil {
		confidenceErr = fmt.Errorf("stop Confidence encoder: %w", confidenceErr)
	}
	return errors.Join(archiveErr, confidenceErr)
}

func (c *Client) recallStreamingPreset(cfg config.Config, streamIndex, preset int) (string, error) {
	expected := fmt.Sprintf("3Rpr%d*%d", streamIndex, preset)
	return c.sendCommand(cfg, streamingPresetRecallCommand(streamIndex, preset), func(line string) bool {
		return strings.EqualFold(line, expected)
	})
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

func (c *Client) queryStreamEnabled(cfg config.Config) (bool, error) {
	response, err := c.sendCommand(cfg, streamEnabledQuery(cfg.StreamIndex), matchesStreamStatus)
	if err != nil {
		return false, err
	}
	match := streamEnabledRegex.FindStringSubmatch(response)
	if len(match) != 2 {
		return false, fmt.Errorf("unexpected stream status response %q", printable(response))
	}
	return match[1] == "1", nil
}

func (c *Client) queryActiveStreamingPreset(cfg config.Config) (*int, error) {
	response, err := c.sendCommand(cfg, streamingPresetQueryCommand(cfg.StreamIndex), func(line string) bool {
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

	query := streamEnabledQuery(cfg.StreamIndex)
	raw, err := probeCommand(client, query, probeWindow)
	if err != nil {
		return Diagnosis{
			Stage:   StageCommand,
			Address: addr,
			Summary: "Signed in to " + addr + ", but the SIS session could not be opened.",
			Detail:  err.Error(),
			Hint:    "SSH and the credentials are correct. Check that this account is allowed to issue SIS commands.",
		}
	}
	if strings.TrimSpace(raw) == "" {
		return Diagnosis{
			Stage:   StageCommand,
			Address: addr,
			Summary: "Signed in to " + addr + ", but the SMP sent nothing back.",
			Detail:  "sent: " + printable(query),
			Hint:    "The network, SSH, and the credentials are all correct, so only the SIS layer is left. Silence usually means the SMP did not recognise the command bytes.",
		}
	}
	if !streamEnabledRegex.MatchString(raw) {
		return Diagnosis{
			Stage:   StageCommand,
			Address: addr,
			Summary: "Signed in to " + addr + ", but the reply was not the expected SIS response.",
			Detail:  "sent: " + printable(query) + "  received: " + printable(raw),
			Hint:    "Everything up to the SIS command layer works. Send me the received text above and I can correct the command format.",
		}
	}

	return Diagnosis{
		OK:      true,
		Stage:   StageOK,
		Address: addr,
		Summary: addr + " answered correctly.",
		Detail:  "received: " + printable(raw),
	}
}

// probeCommand collects everything the SMP sends for a fixed window instead of
// waiting for a terminator, so a protocol mismatch is visible rather than
// showing up as a bare timeout.
func probeCommand(client *ssh.Client, command string, wait time.Duration) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := session.Shell(); err != nil {
		return "", err
	}
	if _, err := io.WriteString(stdin, command); err != nil {
		return "", err
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		tmp := make([]byte, 1024)
		for {
			n, err := stdout.Read(tmp)
			if n > 0 {
				mu.Lock()
				buf.Write(tmp[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(wait):
	}
	mu.Lock()
	defer mu.Unlock()
	return buf.String(), nil
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
	return sendOnClient(client, command, matchesResponse)
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
	action := &Client{send: func(
		_ config.Config,
		command string,
		matchesResponse func(string) bool,
	) (string, error) {
		return sendOnClient(client, command, matchesResponse)
	}}
	return action, func() { _ = client.Close() }, nil
}

func sendOnClient(
	client *ssh.Client,
	command string,
	matchesResponse func(string) bool,
) (string, error) {
	response, err := runCommand(client, command, matchesResponse)
	if err != nil {
		return "", err
	}
	if match := sisErrorRegex.FindStringSubmatch(response); len(match) == 2 {
		return "", fmt.Errorf("SMP returned SIS error E%s", match[1])
	}
	return response, nil
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
	command string,
	matchesResponse func(string) bool,
) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := session.Shell(); err != nil {
		return "", err
	}

	if _, err := io.WriteString(stdin, command); err != nil {
		return "", err
	}

	return readUntilResponse(stdout, readTimeout, matchesResponse)
}

// readUntilResponse ignores the copyright banner, timestamp, and any other
// unsolicited lines until it sees either the expected reply or an SIS error.
func readUntilResponse(
	r io.Reader,
	timeout time.Duration,
	matchesResponse func(string) bool,
) (string, error) {
	type event struct {
		line string
		err  error
		done bool
	}
	events := make(chan event, 8)
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			events <- event{line: strings.TrimSpace(scanner.Text())}
		}
		events <- event{err: scanner.Err(), done: true}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var transcript []string
	for {
		select {
		case current := <-events:
			if current.done {
				if current.err != nil {
					return "", current.err
				}
				return "", fmt.Errorf(
					"SMP closed the connection before replying; received %q",
					printable(strings.Join(transcript, "\n")),
				)
			}
			if current.line == "" {
				continue
			}
			transcript = append(transcript, current.line)
			if matchesResponse(current.line) || sisErrorRegex.MatchString(current.line) {
				return current.line, nil
			}
		case <-timer.C:
			return "", fmt.Errorf(
				"timed out waiting for SMP response; received %q",
				printable(strings.Join(transcript, "\n")),
			)
		}
	}
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
