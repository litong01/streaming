package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultSmpSSHPort          = 22023
	DefaultHTTPPort            = 8080
	DefaultStreamIndex         = 1
	DefaultEnglishPreset       = 2
	DefaultMandarinPreset      = 1
	DefaultPollIntervalSeconds = 3
	CurrentSchemaVersion       = 2
)

type Config struct {
	SchemaVersion       int    `json:"schemaVersion"`
	SmpHost             string `json:"smpHost"`
	SmpSSHPort          int    `json:"smpSshPort"`
	SmpUsername         string `json:"smpUsername"`
	SmpPassword         string `json:"smpPassword"`
	HTTPPort            int    `json:"httpPort"`
	StreamIndex         int    `json:"streamIndex"`
	EnglishPreset       int    `json:"englishPreset"`
	MandarinPreset      int    `json:"mandarinPreset"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
}

type PublicConfig struct {
	SmpHost             string `json:"smpHost"`
	SmpSSHPort          int    `json:"smpSshPort"`
	SmpUsername         string `json:"smpUsername"`
	HasPassword         bool   `json:"hasPassword"`
	HTTPPort            int    `json:"httpPort"`
	StreamIndex         int    `json:"streamIndex"`
	EnglishPreset       int    `json:"englishPreset"`
	MandarinPreset      int    `json:"mandarinPreset"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	IsSmpConfigured     bool   `json:"isSmpConfigured"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

func Default() Config {
	return Config{
		SchemaVersion:       CurrentSchemaVersion,
		SmpSSHPort:          DefaultSmpSSHPort,
		HTTPPort:            DefaultHTTPPort,
		StreamIndex:         DefaultStreamIndex,
		EnglishPreset:       DefaultEnglishPreset,
		MandarinPreset:      DefaultMandarinPreset,
		PollIntervalSeconds: DefaultPollIntervalSeconds,
	}
}

func Path() (string, error) {
	if env := os.Getenv("STREAMING_CONFIG"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "streaming", "config.json"), nil
}

func Load(path string) (*Store, error) {
	store := &Store{path: path, cfg: Default()}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, err
			}
			if err := store.saveLocked(); err != nil {
				return nil, err
			}
			return store, nil
		}
		return nil, err
	}
	var loaded Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, err
	}
	store.cfg = loaded
	needsSave := migratePresetMapping(&store.cfg)
	store.cfg = withDefaults(store.cfg)
	if needsSave {
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Save(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = withDefaults(cfg)
	return s.saveLocked()
}

func (s *Store) Public() PublicConfig {
	cfg := s.Get()
	return PublicConfig{
		SmpHost:             cfg.SmpHost,
		SmpSSHPort:          cfg.SmpSSHPort,
		SmpUsername:         cfg.SmpUsername,
		HasPassword:         cfg.SmpPassword != "",
		HTTPPort:            cfg.HTTPPort,
		StreamIndex:         cfg.StreamIndex,
		EnglishPreset:       cfg.EnglishPreset,
		MandarinPreset:      cfg.MandarinPreset,
		PollIntervalSeconds: cfg.PollIntervalSeconds,
		IsSmpConfigured:     cfg.IsSmpConfigured(),
	}
}

func (c Config) IsSmpConfigured() bool {
	return c.SmpHost != "" && c.SmpUsername != ""
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func withDefaults(cfg Config) Config {
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	if cfg.SmpSSHPort == 0 {
		cfg.SmpSSHPort = DefaultSmpSSHPort
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = DefaultHTTPPort
	}
	if cfg.StreamIndex == 0 {
		cfg.StreamIndex = DefaultStreamIndex
	}
	if cfg.EnglishPreset == 0 {
		cfg.EnglishPreset = DefaultEnglishPreset
	}
	if cfg.MandarinPreset == 0 {
		cfg.MandarinPreset = DefaultMandarinPreset
	}
	if cfg.PollIntervalSeconds == 0 {
		cfg.PollIntervalSeconds = DefaultPollIntervalSeconds
	}
	return cfg
}

func migratePresetMapping(cfg *Config) bool {
	if cfg.SchemaVersion >= CurrentSchemaVersion {
		return false
	}
	if cfg.EnglishPreset == 1 && cfg.MandarinPreset == 2 {
		cfg.EnglishPreset = DefaultEnglishPreset
		cfg.MandarinPreset = DefaultMandarinPreset
	}
	cfg.SchemaVersion = CurrentSchemaVersion
	return true
}
