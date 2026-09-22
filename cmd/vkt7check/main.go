package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"vkt7d/internal/config"
	"vkt7d/internal/diagnostic"
	"vkt7d/internal/storage"
)

func main() {
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	st, err := storage.New(ctx, cfg.DBURL)
	if err != nil {
		log.Error("postgres connection failed", "error", err)
		os.Exit(2)
	}
	defer st.Close()
	if cfg.Migrate {
		if err := st.Migrate(ctx); err != nil {
			log.Error("migration failed", "error", err)
			os.Exit(2)
		}
	}
	if err := diagnostic.Run(ctx, cfg, log, st); err != nil {
		log.Error("check failed", "error", err)
		os.Exit(1)
	}
}
