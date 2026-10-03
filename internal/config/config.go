package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultSmpPort is the SMP's own web port, which also serves its
	// configuration API and its live preview.
	DefaultSmpPort             = 443
	DefaultHTTPPort            = 8080
	DefaultPollIntervalSeconds = 3
	CurrentSchemaVersion       = 4
)

var encryptedFileMagic = []byte("STREAMING-CONFIG-V1\x00")

type Config struct {
	SchemaVersion       int    `json:"schemaVersion"`
	SmpHost             string `json:"smpHost"`
	SmpPort             int    `json:"smpPort"`
	SmpUsername         string `json:"smpUsername"`
	SmpPassword         string `json:"smpPassword"`
	HTTPPort            int    `json:"httpPort"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	// AutoVolume is kept with the rest so the automatic gain carries on after
	// the tablet restarts, with nobody there to switch it back on.
	AutoVolume bool `json:"autoVolume"`
}

type PublicConfig struct {
	SmpHost             string `json:"smpHost"`
	SmpPort             int    `json:"smpPort"`
	SmpUsername         string `json:"smpUsername"`
	HasPassword         bool   `json:"hasPassword"`
	HTTPPort            int    `json:"httpPort"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	IsSmpConfigured     bool   `json:"isSmpConfigured"`
}

type Store struct {
	path        string
	runtimePath string
	key         []byte
	mu          sync.RWMutex
	cfg         Config
}

type LoadOptions struct {
	EncryptionKey []byte
	RuntimePath   string
	InitialConfig *Config
}

func Default() Config {
	return Config{
		SchemaVersion:       CurrentSchemaVersion,
		SmpPort:             DefaultSmpPort,
		HTTPPort:            DefaultHTTPPort,
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
	return LoadWithOptions(path, LoadOptions{})
}

func LoadWithOptions(path string, options LoadOptions) (*Store, error) {
	if len(options.EncryptionKey) != 0 && len(options.EncryptionKey) != 32 {
		return nil, fmt.Errorf("configuration encryption key must be 32 bytes")
	}
	store := &Store{
		path:        path,
		runtimePath: options.RuntimePath,
		key:         append([]byte(nil), options.EncryptionKey...),
		cfg:         Default(),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if options.InitialConfig != nil {
				store.cfg = withDefaults(*options.InitialConfig)
			}
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
	wasEncrypted := len(store.key) != 0 && hasEncryptedFileMagic(data)
	if hasEncryptedFileMagic(data) && len(store.key) == 0 {
		return nil, fmt.Errorf("encrypted configuration requires an encryption key")
	}
	loaded, err := decodeConfig(data, store.key)
	if err != nil {
		return store.startOver(err, options.InitialConfig)
	}
	store.cfg = loaded
	needsSave := migrate(&store.cfg)
	store.cfg = withDefaults(store.cfg)
	if needsSave || (len(store.key) != 0 && !wasEncrypted) {
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	} else if err := store.writeRuntimeLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func decodeConfig(data, key []byte) (Config, error) {
	var cfg Config
	if hasEncryptedFileMagic(data) {
		plain, err := decryptConfig(data, key)
		if err != nil {
			return cfg, err
		}
		data = plain
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse configuration: %w", err)
	}
	return cfg, nil
}

// startOver sets an unreadable configuration aside and begins again from
// defaults. Refusing to start would leave the tablet without a control page
// at all, restarting forever, with nobody there to notice; this way the
// configuration page is still there to fill in again.
func (s *Store) startOver(cause error, initial *Config) (*Store, error) {
	aside := s.path + ".unreadable"
	log.Printf("configuration %s is unreadable (%v); moved to %s and starting from defaults", s.path, cause, aside)
	if err := os.Rename(s.path, aside); err != nil {
		return nil, fmt.Errorf("set aside unreadable configuration: %w", err)
	}
	s.cfg = Default()
	if initial != nil {
		s.cfg = withDefaults(*initial)
	}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
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
		SmpPort:             cfg.SmpPort,
		SmpUsername:         cfg.SmpUsername,
		HasPassword:         cfg.SmpPassword != "",
		HTTPPort:            cfg.HTTPPort,
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
	if len(s.key) != 0 {
		data, err = encryptConfig(data, s.key)
		if err != nil {
			return err
		}
	}
	if err := writeFileDurably(s.path, data); err != nil {
		return err
	}
	return s.writeRuntimeLocked()
}

// writeFileDurably replaces a file so that a power cut leaves either the old
// contents or the new, never an empty file: the tablet is expected to lose
// power without warning. The data is flushed before the rename, and the
// rename itself before returning.
func writeFileDurably(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *Store) writeRuntimeLocked() error {
	if s.runtimePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.runtimePath), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		HTTPPort int `json:"httpPort"`
	}{HTTPPort: s.cfg.HTTPPort})
	if err != nil {
		return err
	}
	return writeFileDurably(s.runtimePath, data)
}

func hasEncryptedFileMagic(data []byte) bool {
	return len(data) >= len(encryptedFileMagic) &&
		string(data[:len(encryptedFileMagic)]) == string(encryptedFileMagic)
}

func encryptConfig(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(encryptedFileMagic)+len(nonce)+len(plaintext)+aead.Overhead())
	result = append(result, encryptedFileMagic...)
	result = append(result, nonce...)
	result = aead.Seal(result, nonce, plaintext, encryptedFileMagic)
	return result, nil
}

func decryptConfig(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	payload := data[len(encryptedFileMagic):]
	if len(payload) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("encrypted configuration is truncated")
	}
	nonce := payload[:aead.NonceSize()]
	ciphertext := payload[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, encryptedFileMagic)
	if err != nil {
		return nil, fmt.Errorf("decrypt configuration: %w", err)
	}
	return plaintext, nil
}

func withDefaults(cfg Config) Config {
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	if cfg.SmpPort == 0 {
		cfg.SmpPort = DefaultSmpPort
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = DefaultHTTPPort
	}
	if cfg.PollIntervalSeconds == 0 {
		cfg.PollIntervalSeconds = DefaultPollIntervalSeconds
	}
	return cfg
}

// migrate brings a file written by an earlier build up to date. Version 4
// dropped the streaming presets and the SIS port: the unit's own RTMP start
// and stop are driven over its web port now, so a file that still carries the
// old fields loses them the next time it is written, and the SSH port it held
// gives way to the default web port.
func migrate(cfg *Config) bool {
	previous := cfg.SchemaVersion
	if previous < CurrentSchemaVersion {
		cfg.SmpPort = 0
	}
	cfg.SchemaVersion = CurrentSchemaVersion
	return previous < CurrentSchemaVersion
}
