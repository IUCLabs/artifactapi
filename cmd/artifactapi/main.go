package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/IUCLabs/artifactapi/internal/config"
	"github.com/IUCLabs/artifactapi/internal/server"
	"github.com/IUCLabs/artifactapi/internal/tui"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tui" {
		endpoint := os.Getenv("ARTIFACTAPI_ENDPOINT")
		if endpoint == "" {
			endpoint = "http://localhost:8000"
		}
		for i, arg := range os.Args {
			if arg == "--endpoint" && i+1 < len(os.Args) {
				endpoint = os.Args[i+1]
			}
		}

		app := tui.New(endpoint)
		if err := app.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv, err := server.New(cfg)
	if err != nil {
		slog.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	if err := srv.Run(ctx); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
