package smp

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"streaming/internal/config"
)

// TestLiveSMP talks to a real unit and prints what it reports. It is skipped
// unless SMP_HOST is set.
//
// Everything it does is a read: it never starts or stops a stream and never
// touches an encoder, so it is safe to run while a service is being streamed.
func TestLiveSMP(t *testing.T) {
	cfg, ok := liveConfig(t)
	if !ok {
		t.Skip("set SMP_HOST, SMP_USER and SMP_PASSWORD_FILE to test against a real SMP")
	}
	client := New()
	ctx := context.Background()

	t.Run("channels", func(t *testing.T) {
		for _, channel := range []int{archiveChannel, confidenceChannel} {
			values, err := client.read(ctx, cfg, streamEnableURI(channel), publishURI(channel))
			if err != nil {
				t.Fatalf("%s: %v", channelName(channel), err)
			}
			enabled, err := values.number(streamEnableURI(channel))
			if err != nil {
				t.Fatal(err)
			}
			current, err := client.session(ctx, cfg, channel)
			if err != nil {
				t.Fatal(err)
			}
			destination := current.destination
			if destination == "" {
				destination = "none"
			}
			t.Logf(
				"%-10s encoder enabled=%d publishing=%t destination=%s",
				channelName(channel), enabled, current.publishing, destination,
			)
		}
	})

	t.Run("state", func(t *testing.T) {
		state, polled := client.QueryState(cfg)
		if !polled {
			t.Fatal("another exchange was in flight")
		}
		t.Logf("%+v", state)
		if state.LastError != nil {
			t.Fatalf("state read failed: %s", *state.LastError)
		}
	})

	t.Run("preview", func(t *testing.T) {
		body, err := client.Preview(ctx, cfg)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		defer body.Close()
		// Enough to prove the unit is producing a fragmented MP4 rather than
		// holding the connection open and sending nothing.
		head := make([]byte, 64*1024)
		read, err := io.ReadFull(body, head)
		if err != nil {
			t.Fatalf("read %d bytes of preview: %v", read, err)
		}
		t.Logf("preview delivered %d bytes starting with %q", read, head[4:12])
	})

	t.Run("diagnosis", func(t *testing.T) {
		result := client.Diagnose(cfg)
		t.Logf("ok=%t stage=%s", result.OK, result.Stage)
		t.Logf("summary: %s", result.Summary)
		t.Logf("detail:  %s", result.Detail)
		t.Logf("hint:    %s", result.Hint)
		if !result.OK {
			t.Fail()
		}
	})
}

func liveConfig(t *testing.T) (config.Config, bool) {
	t.Helper()

	host := strings.TrimSpace(os.Getenv("SMP_HOST"))
	if host == "" {
		return config.Config{}, false
	}
	cfg := config.Default()
	cfg.SmpHost = host
	cfg.SmpUsername = "admin"
	if user := strings.TrimSpace(os.Getenv("SMP_USER")); user != "" {
		cfg.SmpUsername = user
	}
	if port := strings.TrimSpace(os.Getenv("SMP_PORT")); port != "" {
		parsed, err := strconv.Atoi(port)
		if err != nil {
			t.Fatalf("SMP_PORT: %v", err)
		}
		cfg.SmpPort = parsed
	}
	cfg.SmpPassword = livePassword(t)
	return cfg, true
}

// livePassword prefers a file so the password stays out of shell history, the
// process list, and anything this test prints.
func livePassword(t *testing.T) string {
	t.Helper()

	if path := strings.TrimSpace(os.Getenv("SMP_PASSWORD_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("SMP_PASSWORD_FILE: %v", err)
		}
		// Only the trailing newline is removed: an Extron password is allowed
		// to start or end with a space.
		return strings.TrimRight(string(data), "\r\n")
	}
	return os.Getenv("SMP_PASSWORD")
}
