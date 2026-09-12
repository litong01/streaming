package smp

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"strings"
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

func TestStartRecallsLanguageOnArchiveAndPreviewOnConfidence(t *testing.T) {
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
			var commands []string
			client := &Client{send: func(
				_ config.Config,
				command string,
				matchesResponse func(string) bool,
			) (string, error) {
				commands = append(commands, command)
				response := responseForCommand(command)
				if !matchesResponse(response) {
					t.Fatalf("response %q did not match command %q", response, command)
				}
				return response, nil
			}}
			cfg := configuredTestConfig()

			state := test.start(client, cfg)
			want := []string{
				"\x1b1*0STRC\r",
				"\x1b3*0STRC\r",
				fmt.Sprintf("3*1*%d.", test.languagePreset),
				"3*3*3.",
				"\x1b3*1STRC\r",
				"\x1b1*1STRC\r",
				"\x1b1STRC\r",
				"46I",
			}
			if strings.Join(commands, "|") != strings.Join(want, "|") {
				t.Fatalf("commands = %q, want %q", commands, want)
			}
			if state.ActiveStream != test.active || !state.StreamEnabled {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestStopDisablesArchiveAndConfidence(t *testing.T) {
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
	want := []string{"\x1b1*0STRC\r", "\x1b3*0STRC\r", "\x1b1STRC\r", "46I"}
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
	case "3*1*1.":
		return "3Rpr1*1"
	case "3*1*2.":
		return "3Rpr1*2"
	case "3*3*3.":
		return "3Rpr3*3"
	default:
		return "E10"
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
