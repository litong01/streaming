package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPresetMapping(t *testing.T) {
	cfg := Default()
	if cfg.EnglishPreset != 2 || cfg.MandarinPreset != 1 {
		t.Fatalf(
			"unexpected defaults: English=%d Mandarin=%d",
			cfg.EnglishPreset,
			cfg.MandarinPreset,
		)
	}
}

func TestLoadMigratesOldDefaultPresetMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldConfig := []byte(`{
	  "smpHost": "192.0.2.1",
	  "smpSSHPort": 22023,
	  "smpUsername": "admin",
	  "httpPort": 8080,
	  "streamIndex": 1,
	  "englishPreset": 1,
	  "mandarinPreset": 2,
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
	if cfg.EnglishPreset != 2 || cfg.MandarinPreset != 1 {
		t.Fatalf(
			"mapping was not migrated: English=%d Mandarin=%d",
			cfg.EnglishPreset,
			cfg.MandarinPreset,
		)
	}
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", cfg.SchemaVersion, CurrentSchemaVersion)
	}
}

func TestLoadPreservesCustomPresetMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldConfig := []byte(`{
	  "englishPreset": 11,
	  "mandarinPreset": 12
	}`)
	if err := os.WriteFile(path, oldConfig, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if cfg.EnglishPreset != 11 || cfg.MandarinPreset != 12 {
		t.Fatalf(
			"custom mapping changed: English=%d Mandarin=%d",
			cfg.EnglishPreset,
			cfg.MandarinPreset,
		)
	}
}

func TestLoadForcesArchiveChannelA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldConfig := []byte(`{"streamIndex": 2}`)
	if err := os.WriteFile(path, oldConfig, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().StreamIndex; got != DefaultStreamIndex {
		t.Fatalf("stream index = %d, want %d", got, DefaultStreamIndex)
	}
}

func TestParseHostPort(t *testing.T) {
	tests := []struct {
		address string
		port    int
		host    string
		want    int
	}{
		{"192.0.2.10", 0, "192.0.2.10", DefaultSmpSSHPort},
		{"192.0.2.10:22024", 0, "192.0.2.10", 22024},
		{"smp.local", 22023, "smp.local", 22023},
		{"[2001:db8::1]:22023", 0, "2001:db8::1", 22023},
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

func TestValidateRejectsSamePresetsAndLowHTTPPort(t *testing.T) {
	cfg := Default()
	cfg.SmpHost = "192.0.2.10"
	cfg.SmpUsername = "admin"
	cfg.EnglishPreset = 1
	cfg.MandarinPreset = 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected preset collision error")
	}
	cfg.MandarinPreset = 2
	cfg.HTTPPort = 80
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected HTTP port error")
	}
	cfg.HTTPPort = DefaultHTTPPort
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.MandarinPreset = cfg.EnglishPreset
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected preset collision error")
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
