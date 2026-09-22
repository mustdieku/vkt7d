package config

import (
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
	flag.StringVar(&c.Port, "port", env("VKT7_PORT", "/dev/ttyUSB0"), "serial port")
	flag.IntVar(&c.Baud, "baud", envInt("VKT7_BAUD", 9600), "baud rate")
	flag.IntVar(&c.Address, "address", envInt("VKT7_ADDRESS", 1), "VKT-7 network address")
	flag.StringVar(&c.DBURL, "db-url", env("VKT7_DB_URL", "postgres://vkt7:vkt7@127.0.0.1:5432/vkt7?sslmode=disable"), "PostgreSQL URL")
	flag.StringVar(&c.DeviceName, "device-name", env("VKT7_DEVICE_NAME", "vkt7-1"), "device name")
	flag.DurationVar(&c.Interval, "interval", envDuration("VKT7_INTERVAL", 3*time.Hour), "poll interval")
	flag.IntVar(&c.BatchHourly, "batch-hourly", envInt("VKT7_BATCH_HOURLY", 48), "max hourly records per session")
	flag.IntVar(&c.BatchDaily, "batch-daily", envInt("VKT7_BATCH_DAILY", 30), "max daily records per session")
	flag.IntVar(&c.BatchMonthly, "batch-monthly", envInt("VKT7_BATCH_MONTHLY", 12), "max monthly records per session")
	flag.IntVar(&c.BatchTotal, "batch-total", envInt("VKT7_BATCH_TOTAL", 12), "max total records per session")
	flag.IntVar(&c.Overlap, "overlap", envInt("VKT7_OVERLAP", 0), "re-read this many previous periods")
	flag.DurationVar(&c.Timeout, "timeout", envDuration("VKT7_TIMEOUT", 8*time.Second), "serial operation timeout")
	flag.BoolVar(&c.Migrate, "migrate", false, "apply embedded schema")
	flag.BoolVar(&c.Verbose, "verbose", false, "verbose logging")
	flag.BoolVar(&c.DebugSerial, "debug-serial", false, "log raw VKT-7 TX/RX frames (implies verbose)")
	flag.Parse()
	if c.DebugSerial {
		c.Verbose = true
	}
	return c
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
