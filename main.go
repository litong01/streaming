package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"streaming/internal/config"
	"streaming/internal/server"
	"streaming/internal/smp"
)

//go:embed web/control.html web/config.html
var webFS embed.FS

func main() {
	path, err := config.Path()
	if err != nil {
		log.Fatal(err)
	}
	key, err := decodeEncryptionKey(os.Getenv("STREAMING_CONFIG_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	initialConfig, err := decodeInitialConfig(os.Getenv("STREAMING_IMPORT_CONFIG"))
	if err != nil {
		log.Fatal(err)
	}
	store, err := config.LoadWithOptions(path, config.LoadOptions{
		EncryptionKey: key,
		RuntimePath:   os.Getenv("STREAMING_RUNTIME"),
		InitialConfig: initialConfig,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("config file: %s", path)

	control, err := webFS.ReadFile("web/control.html")
	if err != nil {
		log.Fatal(err)
	}
	configPage, err := webFS.ReadFile("web/config.html")
	if err != nil {
		log.Fatal(err)
	}

	srv := server.New(store, smp.New(), server.Pages{
		Control: control,
		Config:  configPage,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.ListenAndServe(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}

func decodeEncryptionKey(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode STREAMING_CONFIG_KEY: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("STREAMING_CONFIG_KEY must contain 32 bytes")
	}
	return key, nil
}

func decodeInitialConfig(value string) (*config.Config, error) {
	if value == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode STREAMING_IMPORT_CONFIG: %w", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse STREAMING_IMPORT_CONFIG: %w", err)
	}
	return &cfg, nil
}
