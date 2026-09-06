package config

import (
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
