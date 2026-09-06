package smp

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"streaming/internal/config"
)

const (
	connectTimeout = 10 * time.Second
	readTimeout    = 5 * time.Second
)

var (
	streamEnabledRegex  = regexp.MustCompile(`(?i)Strc\d+\*(\d+)`)
	selectedPresetRegex = regexp.MustCompile(`,\s*(\d+)\*`)
	singlePresetRegex   = regexp.MustCompile(`^(\d+)\*`)
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
	QueriedAtEpochMs int64        `json:"queriedAtEpochMs"`
}

type Client struct{}

func New() *Client {
	return &Client{}
}

func (c *Client) QueryState(cfg config.Config) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}

	enabled, err := c.queryStreamEnabled(cfg)
	if err != nil {
		return failed("Unable to reach SMP", err)
	}
	preset, err := c.queryActiveStreamingPreset(cfg)
	if err != nil {
		return failed("Unable to reach SMP", err)
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
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}

func (c *Client) StartEnglish(cfg config.Config) State {
	return c.startPreset(cfg, cfg.EnglishPreset, ActiveEnglish)
}

func (c *Client) StartMandarin(cfg config.Config) State {
	return c.startPreset(cfg, cfg.MandarinPreset, ActiveMandarin)
}

func (c *Client) Stop(cfg config.Config) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	if _, err := c.setStreamEnabled(cfg, false); err != nil {
		return failed("Stop failed", err)
	}
	return c.QueryState(cfg)
}

func (c *Client) startPreset(cfg config.Config, preset int, expected ActiveStream) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	if _, err := c.recallStreamingPreset(cfg, preset); err != nil {
		return failed("Start failed", err)
	}
	if _, err := c.setStreamEnabled(cfg, true); err != nil {
		return failed("Start failed", err)
	}
	state := c.QueryState(cfg)
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

func (c *Client) recallStreamingPreset(cfg config.Config, preset int) (string, error) {
	return c.sendCommand(cfg, fmt.Sprintf("3*%d*%d.", cfg.StreamIndex, preset))
}

func (c *Client) setStreamEnabled(cfg config.Config, enabled bool) (string, error) {
	value := 0
	if enabled {
		value = 1
	}
	return c.sendCommand(cfg, fmt.Sprintf("E %d*%d STRC}", cfg.StreamIndex, value))
}

func (c *Client) queryStreamEnabled(cfg config.Config) (bool, error) {
	response, err := c.sendCommand(cfg, fmt.Sprintf("E %d)STRC}", cfg.StreamIndex))
	if err != nil {
		return false, err
	}
	match := streamEnabledRegex.FindStringSubmatch(response)
	return len(match) == 2 && match[1] == "1", nil
}

func (c *Client) queryActiveStreamingPreset(cfg config.Config) (*int, error) {
	response, err := c.sendCommand(cfg, streamingPresetQueryCommand(cfg.StreamIndex))
	if err != nil {
		return nil, err
	}
	return parseSelectedStreamingPreset(response), nil
}

func (c *Client) sendCommand(cfg config.Config, command string) (string, error) {
	sshConfig := &ssh.ClientConfig{
		User:            cfg.SmpUsername,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.SmpPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         connectTimeout,
	}

	addr := fmt.Sprintf("%s:%d", cfg.SmpHost, cfg.SmpSSHPort)
	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return "", err
	}
	defer client.Close()

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

	if _, err := io.WriteString(stdin, command+"\r\n"); err != nil {
		return "", err
	}

	return readUntilTerminator(stdout, readTimeout)
}

func readUntilTerminator(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		data string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 1024)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), "]") {
					ch <- result{data: strings.TrimSpace(buf.String())}
					return
				}
			}
			if err != nil {
				ch <- result{data: strings.TrimSpace(buf.String()), err: err}
				return
			}
		}
	}()

	select {
	case out := <-ch:
		if out.data != "" {
			return out.data, nil
		}
		return "", out.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for SMP response")
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
	if match := selectedPresetRegex.FindStringSubmatch(response); len(match) == 2 {
		if value, err := strconv.Atoi(match[1]); err == nil {
			return &value
		}
	}
	if match := singlePresetRegex.FindStringSubmatch(strings.TrimSpace(response)); len(match) == 2 {
		if value, err := strconv.Atoi(match[1]); err == nil {
			return &value
		}
	}
	return nil
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
