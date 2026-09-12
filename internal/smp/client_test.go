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

func TestSMP351ErrorResponses(t *testing.T) {
	for _, response := range []string{"E10", "E24", "banner\r\nE13\r\n"} {
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

func TestReadUntilSISResponseTerminator(t *testing.T) {
	for _, response := range []string{
		"Strc1*1\r\n",
		"Strc1*1\r\r\n", // The SMP guide documents an extra CR over SSH.
	} {
		got, err := readUntilTerminator(strings.NewReader(response), time.Second)
		if err != nil {
			t.Fatalf("%q: %v", response, err)
		}
		if got != "Strc1*1" {
			t.Errorf("%q => %q, want Strc1*1", response, got)
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
