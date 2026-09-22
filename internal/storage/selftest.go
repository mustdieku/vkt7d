package storage

import (
	"context"
	"fmt"
	"time"
	"vkt7d/internal/model"
)

// SelfTest exercises the storage functions used by the collector. By default
// it only performs reads and schema checks. With write=true it performs a
// complete write/read/delete round-trip inside a transaction-like test scope
// and removes all test rows afterwards.
func (s *Store) SelfTest(ctx context.Context, write bool, deviceName string, address int, port string, baud int) error {
	if err := s.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='vkt7'`).Scan(&n); err != nil {
		return fmt.Errorf("schema query: %w", err)
	}
	if n < 9 {
		return fmt.Errorf("schema incomplete: only %d vkt7 tables found", n)
	}
	fmt.Printf("[DB TEST] connection/schema: OK (%d tables)\n", n)

	id, err := s.Device(ctx, deviceName, address, port, baud)
	if err != nil {
		return fmt.Errorf("Device(): %w", err)
	}
	fmt.Printf("[DB TEST] Device(): OK id=%d\n", id)
	if !write {
		return nil
	}

	cleanup := func() {
		_, _ = s.Pool.Exec(ctx, `DELETE FROM vkt7.devices WHERE id=$1`, id)
	}
	defer cleanup()

	elements := []model.Element{{0, "t1_1", 4}, {3, "V1_1", 4}, {81, "DI", 4}}
	if err := s.UpsertActive(ctx, id, elements); err != nil {
		return fmt.Errorf("UpsertActive(): %w", err)
	}
	fmt.Println("[DB TEST] UpsertActive(): OK")

	vals := map[string]model.Value{
		"t1_1": {Value: 65.25, Quality: 0xC0, NS: 0, Raw: []byte{0x00, 0x00, 0x82, 0x42}},
		"V1_1": {Value: 123.5, Quality: 0xC0, NS: 0, Raw: []byte{0x00, 0x00, 0xF7, 0x42}},
	}
	ts := time.Now().Truncate(time.Hour).Add(-time.Hour)
	if err := s.SaveArchive(ctx, "hourly_archive", id, ts, vals); err != nil {
		return fmt.Errorf("SaveArchive(): %w", err)
	}
	last, err := s.Last(ctx, "hourly_archive", id)
	if err != nil || last == nil {
		return fmt.Errorf("Last(): err=%v last=%v", err, last)
	}
	if !last.Equal(ts) {
		return fmt.Errorf("Last(): got %s want %s", last, ts)
	}
	fmt.Println("[DB TEST] SaveArchive()/Last(): OK")

	if err := s.SaveCurrent(ctx, "current_values", id, vals); err != nil {
		return fmt.Errorf("SaveCurrent(): %w", err)
	}
	if _, err := s.CurrentLast(ctx, "current_values", id); err != nil {
		return fmt.Errorf("CurrentLast(): %w", err)
	}
	fmt.Println("[DB TEST] SaveCurrent()/CurrentLast(): OK")

	s.Log(ctx, id, "selftest", nil)
	fmt.Println("[DB TEST] Log(): OK")
	fmt.Println("[DB TEST] write round-trip: OK; test rows will be removed")
	return nil
}
