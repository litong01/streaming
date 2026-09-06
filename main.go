package main

import (
	"context"
	"embed"
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
	store, err := config.Load(path)
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
