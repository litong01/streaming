package smp

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"streaming/internal/config"
)

// TestLiveSMP talks to a real unit and prints what each SSH channel type
// produced for each query. It is skipped unless SMP_HOST is set.
//
// Every command it sends is a query. It never recalls a preset and never
// enables or disables an encoder, so it is safe to run while a service is
// being streamed.
func TestLiveSMP(t *testing.T) {
	cfg, ok := liveConfig(t)
	if !ok {
		t.Skip("set SMP_HOST, SMP_USER and SMP_PASSWORD_FILE to test against a real SMP")
	}

	t.Run("channel ladder", func(t *testing.T) {
		client, err := dialClient(cfg)
		if err != nil {
			t.Fatalf("could not sign in to %s: %v", smpAddress(cfg), err)
		}
		defer client.Close()
		t.Logf("signed in to %s as %q", smpAddress(cfg), cfg.SmpUsername)

		// A bare carriage return is included because it asks the SIS parser
		// for nothing: whatever comes back is the unit announcing itself.
		queries := []string{
			"\r",
			streamEnabledQuery(cfg.StreamIndex),
			streamingPresetQueryCommand(cfg.StreamIndex),
		}
		for _, transport := range sisTransports {
			for _, query := range queries {
				probe := probeSIS(client, transport, query)
				t.Logf(
					"%-21s sent %-12s greeting=%-28q reply=%-28q err=%v",
					transport, printable(query),
					printable(probe.greeting), printable(probe.reply), probe.err,
				)
			}
		}
	})

	// Diagnose opens its own login, so it runs once the ladder above has hung
	// up rather than competing with it for the unit's SIS session.
	t.Run("diagnosis", func(t *testing.T) {
		result := New().Diagnose(cfg)
		t.Logf("ok=%t stage=%s", result.OK, result.Stage)
		t.Logf("summary: %s", result.Summary)
		t.Logf("detail:  %s", result.Detail)
		t.Logf("hint:    %s", result.Hint)
		if !result.OK {
			t.Fail()
		}
	})

	t.Run("state", func(t *testing.T) {
		state, polled := New().QueryState(cfg)
		if !polled {
			t.Fatal("another exchange was in flight")
		}
		t.Logf("%+v", state)
		if state.LastError != nil {
			t.Fatalf("state read failed: %s", *state.LastError)
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
		cfg.SmpSSHPort = parsed
	}
	// SMP_STREAM aims the queries at one encoder: 1 is Archive and 3 is
	// Confidence, which is the only way to read the second one, since the
	// saved configuration always drives Archive.
	if stream := strings.TrimSpace(os.Getenv("SMP_STREAM")); stream != "" {
		parsed, err := strconv.Atoi(stream)
		if err != nil {
			t.Fatalf("SMP_STREAM: %v", err)
		}
		cfg.StreamIndex = parsed
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
