package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"vkt7d/internal/config"
	"vkt7d/internal/storage"
)

func main() {
	cfg := config.Load()
	write := flag.Bool("write", false, "perform write/read round-trip and remove test rows afterwards")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
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
	if err := st.SelfTest(ctx, *write, cfg.DeviceName+"-dbtest", cfg.Address, cfg.Port, cfg.Baud); err != nil {
		log.Error("database test failed", "error", err)
		os.Exit(1)
	}
	log.Info("database test completed", "write_roundtrip", *write)
}
