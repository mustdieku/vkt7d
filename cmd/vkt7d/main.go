package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"vkt7d/internal/collector"
	"vkt7d/internal/config"
	"vkt7d/internal/storage"
)

func main() {
	cfg := config.Load()
	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	st, e := storage.New(ctx, cfg.DBURL)
	if e != nil {
		log.Error("postgres initial connection failed", "error", e)
		os.Exit(2)
	}
	defer st.Close()
	if cfg.Migrate {
		if e := st.Migrate(ctx); e != nil {
			log.Error("migration failed", "error", e)
			os.Exit(2)
		}
		log.Info("database schema applied")
	}
	co := &collector.Collector{Cfg: cfg, Log: log, Store: st}
	log.Info("vkt7d started", "port", cfg.Port, "baud", cfg.Baud, "address", cfg.Address, "interval", cfg.Interval)
	co.Run(ctx)
	log.Info("vkt7d stopped")
}
