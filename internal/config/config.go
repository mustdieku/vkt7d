package config

import (
	"errors"
	"flag"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port         string
	Baud         int
	Address      int
	DBURL        string
	DeviceName   string
	Interval     time.Duration
	BatchHourly  int
	BatchDaily   int
	BatchMonthly int
	BatchTotal   int
	Overlap      int
	Timeout      time.Duration
	Migrate      bool
	Verbose      bool
	DebugSerial  bool
}

func Load() Config {
	c := Config{}
	_ = c.Parse(os.Args[1:])
	return c
}

// Parse parses command-line arguments without modifying the global
// flag.CommandLine. This makes configuration independently testable and
// allows callers to parse more than one configuration in the same process.
func (c *Config) Parse(args []string) error {
	fs := flag.NewFlagSet("vkt7d", flag.ContinueOnError)

	fs.StringVar(&c.Port, "port", env("VKT7_PORT", "/dev/ttyUSB0"), "serial port")
	fs.IntVar(&c.Baud, "baud", envInt("VKT7_BAUD", 9600), "baud rate")
	fs.IntVar(&c.Address, "address", envInt("VKT7_ADDRESS", 1), "VKT-7 network address")
	fs.StringVar(&c.DBURL, "db-url", env("VKT7_DB_URL", "postgres://vkt7:vkt7@127.0.0.1:5432/vkt7?sslmode=disable"), "PostgreSQL URL")
	fs.StringVar(&c.DeviceName, "device-name", env("VKT7_DEVICE_NAME", "vkt7-1"), "device name")
	fs.DurationVar(&c.Interval, "interval", envDuration("VKT7_INTERVAL", 3*time.Hour), "poll interval")
	fs.IntVar(&c.BatchHourly, "batch-hourly", envInt("VKT7_BATCH_HOURLY", 48), "max hourly records per session")
	fs.IntVar(&c.BatchDaily, "batch-daily", envInt("VKT7_BATCH_DAILY", 30), "max daily records per session")
	fs.IntVar(&c.BatchMonthly, "batch-monthly", envInt("VKT7_BATCH_MONTHLY", 12), "max monthly records per session")
	fs.IntVar(&c.BatchTotal, "batch-total", envInt("VKT7_BATCH_TOTAL", 12), "max total records per session")
	fs.IntVar(&c.Overlap, "overlap", envInt("VKT7_OVERLAP", 0), "re-read this many previous periods")
	fs.DurationVar(&c.Timeout, "timeout", envDuration("VKT7_TIMEOUT", 8*time.Second), "serial operation timeout")
	fs.BoolVar(&c.Migrate, "migrate", false, "apply embedded schema")
	fs.BoolVar(&c.Verbose, "verbose", false, "verbose logging")
	fs.BoolVar(&c.DebugSerial, "debug-serial", false, "log raw VKT-7 TX/RX frames (implies verbose)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if c.DebugSerial {
		c.Verbose = true
	}

	if c.Interval <= 0 {
		return errors.New("interval must be greater than zero")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	if c.Baud <= 0 {
		return errors.New("baud must be greater than zero")
	}
	if c.Address < 0 || c.Address > 255 {
		return errors.New("address must be in range 0..255")
 	}

	return nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return d
	}
	return n
}

func envDuration(k string, d time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	x, e := time.ParseDuration(v)
	if e != nil {
		return d
	}
	return x
}
