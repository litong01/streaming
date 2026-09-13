package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A configuration written by a build that still recalled presets loses the
// preset numbers, and its SIS port gives way to the SMP's web port, which is
// where the streams are started now.
func TestLoadMigratesAPresetConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldConfig := []byte(`{
	  "schemaVersion": 3,
	  "smpHost": "192.0.2.1",
	  "smpSshPort": 22023,
	  "smpUsername": "admin",
	  "httpPort": 8080,
	  "streamIndex": 1,
	  "englishPreset": 2,
	  "mandarinPreset": 1,
	  "pollIntervalSeconds": 3
	}`)
	if err := os.WriteFile(path, oldConfig, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if cfg.SmpPort != DefaultSmpPort {
		t.Fatalf("SMP port = %d, want %d", cfg.SmpPort, DefaultSmpPort)
	}
	if cfg.SmpHost != "192.0.2.1" || cfg.SmpUsername != "admin" {
		t.Fatalf("connection details changed: %+v", cfg)
	}
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", cfg.SchemaVersion, CurrentSchemaVersion)
	}

	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"englishPreset", "mandarinPreset", "streamIndex", "smpSshPort"} {
		if bytes.Contains(rewritten, []byte(gone)) {
			t.Fatalf("rewritten configuration still carries %q", gone)
		}
	}
}

// A port the operator chose is theirs to keep, so it survives a migration that
// is only meant to clear the SIS port away.
func TestLoadKeepsAnExplicitPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":4,"smpPort":80}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().SmpPort; got != 80 {
		t.Fatalf("SMP port = %d, want 80", got)
	}
}

func TestParseHostPort(t *testing.T) {
	tests := []struct {
		address string
		port    int
		host    string
		want    int
	}{
		{"192.0.2.10", 0, "192.0.2.10", DefaultSmpPort},
		{"192.0.2.10:8443", 0, "192.0.2.10", 8443},
		{"smp.local", 80, "smp.local", 80},
		{"[2001:db8::1]:443", 0, "2001:db8::1", 443},
	}
	for _, test := range tests {
		host, port, err := ParseHostPort(test.address, test.port)
		if err != nil {
			t.Fatalf("%q: %v", test.address, err)
		}
		if host != test.host || port != test.want {
			t.Fatalf("%q => %s:%d, want %s:%d", test.address, host, port, test.host, test.want)
		}
	}
}

func TestParseHostPortRejectsURLAndSpaces(t *testing.T) {
	for _, address := range []string{"http://192.0.2.10", "192.0.2.10 1", ""} {
		if _, _, err := ParseHostPort(address, 0); err == nil {
			t.Fatalf("expected error for %q", address)
		}
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.SmpHost = "192.0.2.10"
	cfg.SmpUsername = "admin"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	// The local server has to be reachable from Fully Kiosk without root, so
	// a privileged port is rejected even though the SMP's own is one.
	lowPort := cfg
	lowPort.HTTPPort = 80
	if err := lowPort.Validate(); err == nil {
		t.Fatal("expected an HTTP port error")
	}

	noUser := cfg
	noUser.SmpUsername = " "
	if err := noUser.Validate(); err == nil {
		t.Fatal("expected a missing username error")
	}

	badPort := cfg
	badPort.SmpPort = 0
	if err := badPort.Validate(); err == nil {
		t.Fatal("expected an SMP port error")
	}
}

func TestEncryptedConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.bin")
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	key := bytes.Repeat([]byte{0x42}, 32)
	initial := Default()
	initial.SmpHost = "192.0.2.20"
	initial.SmpUsername = "admin"
	initial.SmpPassword = "secret"

	store, err := LoadWithOptions(path, LoadOptions{
		EncryptionKey: key,
		RuntimePath:   runtimePath,
		InitialConfig: &initial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().SmpPassword; got != "secret" {
		t.Fatalf("password = %q, want secret", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEncryptedFileMagic(data) {
		t.Fatal("configuration was not encrypted")
	}
	if bytes.Contains(data, []byte("secret")) {
		t.Fatal("encrypted file contains plaintext password")
	}

	reloaded, err := LoadWithOptions(path, LoadOptions{EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().SmpPassword; got != "secret" {
		t.Fatalf("reloaded password = %q, want secret", got)
	}

	var runtime struct {
		HTTPPort int `json:"httpPort"`
	}
	runtimeData, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(runtimeData, &runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.HTTPPort != DefaultHTTPPort {
		t.Fatalf("runtime HTTP port = %d, want %d", runtime.HTTPPort, DefaultHTTPPort)
	}
}

func TestLoadEncryptsExistingPlaintextConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"smpPassword":"migrate-me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x24}, 32)
	if _, err := LoadWithOptions(path, LoadOptions{EncryptionKey: key}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEncryptedFileMagic(data) || bytes.Contains(data, []byte("migrate-me")) {
		t.Fatal("plaintext configuration was not migrated to encrypted storage")
	}
}

func TestEncryptedConfigRejectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.bin")
	key := bytes.Repeat([]byte{0x11}, 32)
	if _, err := LoadWithOptions(path, LoadOptions{EncryptionKey: key}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOptions(path, LoadOptions{EncryptionKey: key}); err == nil {
		t.Fatal("expected tampered configuration to fail authentication")
	}
}
