package collector

import (
	"testing"
	"time"

	"vkt7d/internal/protocol"
)

func TestParseStartUsesDailyArchiveStart(t *testing.T) {
	// 2026-09-07 11:00 hourly start
	// 2026-09-24 19:00 current time
	// 2026-09-07 23:00 daily start
	rangeData := []byte{
		7, 9, 0x1A, 11,
		24, 9, 0x1A, 19,
		7, 9, 0x1A, 23,
	}

	daily := parseStart(rangeData, protocol.Daily, 30)
	wantDaily := time.Date(
		2026, 9, 7, 23, 0, 0, 0, time.Local,
	)
	if !daily.Equal(wantDaily) {
		t.Fatalf("daily start = %s, want %s", daily, wantDaily)
	}

	monthly := parseStart(rangeData, protocol.Monthly, 30)
	wantMonthly := time.Date(
		2026, 9, 30, 23, 0, 0, 0, time.Local,
	)
	if !monthly.Equal(wantMonthly) {
		t.Fatalf("monthly start = %s, want %s", monthly, wantMonthly)
	}
}

func TestMonthlyReportDateClampsShortMonth(t *testing.T) {
	got := monthlyReportDate(
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.Local),
		30,
	)
	want := time.Date(
		2026, 2, 28, 23, 0, 0, 0, time.Local,
	)

	if !got.Equal(want) {
		t.Fatalf("monthly report date = %s, want %s", got, want)
	}
}