package diagnostic

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"vkt7d/internal/config"
	"vkt7d/internal/model"
	"vkt7d/internal/protocol"
	"vkt7d/internal/storage"
)

// Run performs a non-destructive end-to-end VKT-7 check using the same
// protocol and configuration used by the daemon. Archive data is read but
// is not written to PostgreSQL.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, st *storage.Store) error {
	if err := st.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	id, err := st.Device(ctx, cfg.DeviceName, cfg.Address, cfg.Port, cfg.Baud)
	if err != nil {
		return fmt.Errorf("device row: %w", err)
	}
	fmt.Printf("[DB] OK: PostgreSQL connected, device_id=%d, name=%s\n", id, cfg.DeviceName)
	fmt.Printf("[SERIAL] opening %s baud=%d address=%d\n", cfg.Port, cfg.Baud, cfg.Address)

	port, err := protocol.Open(cfg.Port, cfg.Baud)
	if err != nil {
		return fmt.Errorf("open serial %s: %w", cfg.Port, err)
	}
	defer port.Close()
	c := &protocol.Client{Port: port, Address: byte(cfg.Address), Timeout: cfg.Timeout, Log: log, Debug: cfg.DebugSerial}

	step := func(name string, fn func() error) error {
		fmt.Printf("[TEST] %-28s ... ", name)
		started := time.Now()
		err := fn()
		if err != nil {
			fmt.Printf("FAIL (%s): %v\n", time.Since(started).Round(time.Millisecond), err)
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("OK (%s)\n", time.Since(started).Round(time.Millisecond))
		return nil
	}

	if err := step("begin session", c.Begin); err != nil {
		return err
	}

	var deviceTime time.Time
	if err := step("device date/time", func() error {
		var e error
		deviceTime, e = c.ReadDeviceTime()
		return e
	}); err != nil {
		return err
	}
	fmt.Printf("[DEVICE] time=%s\n", deviceTime.Format(time.RFC3339))

	if err := step("service information", func() error {
		d, e := c.ReadService()
		if e != nil {
			return e
		}
		fmt.Printf("  raw=%X\n", d)
		if len(d) >= 16 {
			version := d[0]
			s1 := uint16(d[1]) | uint16(d[2])<<8
			s2 := uint16(d[3]) | uint16(d[4])<<8
			subscriber := string(d[5:13])
			fmt.Printf("  firmware=0x%02X scheme_tv1=%d scheme_tv2=%d subscriber=%q address=%d report_day=%d model=%d\n", version, s1, s2, subscriber, d[13], d[14], d[15])
		}
		return nil
	}); err != nil {
		return err
	}

	es := propertyElements()
	fmt.Printf("[PROPERTIES] request elements=%d bytes=%d\n", len(es), len(es)*6)
	for _, e := range es {
		fmt.Printf("  %3d %-12s size=%d\n", e.Address, e.Name, e.Size)
	}
	if err := step("properties: set type", func() error {
		return c.SetType(protocol.Properties)
	}); err != nil {
		return err
	}
	if err := step("properties: set read list", func() error {
		return c.SetReadList(es)
	}); err != nil {
		return err
	}
	var propertyData []byte
	if err := step("properties: read data", func() error {
		var err error
		propertyData, err = c.ReadData()
		if err == nil {
			fmt.Printf("  raw=%X\n", propertyData)
		}
		return err
	}); err != nil {
		return err
	}
	if err := step("properties: parse data", func() error {
		vals, err := protocol.ParseProperties(es, propertyData)
		if err == nil {
			printValues(vals)
		}
		return err
	}); err != nil {
		return err
	}

	// The active-element list describes the measurement scheme, not the
	// properties type. Some VKT-7 firmware revisions return an empty list
	// while type=6 (properties) is selected. Select a real data type before
	// asking for the active mask/list.
	if err := step("select current type for active list", func() error {
		return c.SetType(protocol.Current)
	}); err != nil {
		return err
	}

	var active []model.Element
	if err := step("active elements", func() error {
		var e error
		active, e = c.ActiveElements()
		if e == nil {
			fmt.Printf("\n[ACTIVE] %d elements\n", len(active))
			for _, x := range active {
				fmt.Printf("  %3d %-20s size=%d\n", x.Address, x.Name, x.Size)
			}
		}
		return e
	}); err != nil {
		return err
	}

	if err := step("scheme/active DB", func() error {
		// ReadScheme() is a version >=1.9 diagnostic operation and the
		// protocol requires a value-type write beforehand. Keep the selected
		// type at a normal data type (not properties) for devices that reject
		// the scheme register while type=6 is selected.
		if err := c.SetType(protocol.Current); err != nil {
			return err
		}
		for _, tv := range []int{1, 2} {
			s, q, ns, err := c.ReadScheme(tv)
			if err != nil {
				return fmt.Errorf("TV%d: %w", tv, err)
			}
			fmt.Printf("  TV%d scheme=%d quality=0x%02X ns=0x%02X\n", tv, s, q, ns)
		}
		// The active database is read after selecting a current-data type.
		if err := c.SetType(protocol.Current); err != nil {
			return err
		}
		db, q, ns, err := c.ReadActiveDB()
		if err != nil {
			return err
		}
		fmt.Printf("  current active DB=%d quality=0x%02X ns=0x%02X\n", db, q, ns)
		return nil
	}); err != nil {
		return err
	}

	// Current values are deliberately tested before archives: this exercises
	// the current-data path without depending on archive availability.
	if err := readCurrent(step, c, active, protocol.Current, "current values"); err != nil {
		return err
	}
	if err := readCurrent(step, c, active, protocol.CurrentTotal, "current totals"); err != nil {
		return err
	}

	types := []struct {
		typ   int
		name  string
		daily bool
	}{
		{protocol.Hourly, "hourly archive", false},
		{protocol.Daily, "daily archive", true},
		{protocol.Monthly, "monthly archive", true},
		{protocol.Total, "total archive", true},
	}
	for _, a := range types {
		if err := testArchive(ctx, step, c, active, a.typ, a.name, a.daily, deviceTime); err != nil {
			return err
		}
	}

	fmt.Println("[RESULT] VKT-7 end-to-end check completed successfully")
	return nil
}

func propertyElements() []model.Element {
	return []model.Element{
		{44, "t_unit", 7}, {45, "G_unit", 7}, {46, "V_unit", 7}, {47, "M_unit", 7}, {48, "P_unit", 7},
		{53, "Qo_unit", 7}, {55, "BNP_unit", 7}, {56, "VOC_unit", 7}, {57, "t_dec", 1}, {59, "V1_dec", 1},
		{60, "M1_dec", 1}, {61, "P1_dec", 1}, {66, "Qo1_dec", 1}, {69, "V2_dec", 1}, {70, "M2_dec", 1}, {76, "Qo2_dec", 1},
	}
}

func readCurrent(step func(string, func() error) error, c *protocol.Client, active []model.Element, typ int, name string) error {
	return step(name, func() error {
		es := filter(active, typ)
		if len(es) == 0 {
			return fmt.Errorf("no active elements for type %d", typ)
		}
		v, err := c.ReadCurrent(typ, es)
		if err == nil {
			printValues(v)
		}
		return err
	})
}

func testArchive(ctx context.Context, step func(string, func() error) error, c *protocol.Client, active []model.Element, typ int, name string, daily bool, now time.Time) error {
	es := filter(active, typ)
	if len(es) == 0 {
		return fmt.Errorf("%s: no active elements", name)
	}
	// Read the archive range after selecting the archive type. This is a
	// diagnostic operation and does not alter the device archive.
	var start time.Time
	if err := step(name+" range", func() error {
		if err := c.SetType(byte(typ)); err != nil {
			return err
		}
		r, err := c.ReadRange()
		if err != nil {
			return err
		}
		var e error
		start, e = parseRangeStart(r, typ)
		if e == nil {
			fmt.Printf("  range start=%s\n", start.Format("2006-01-02 15:04:05"))
		}
		return e
	}); err != nil {
		return err
	}

	// The device has finite archives; an upper bound prevents an accidental
	// endless diagnostic loop if a malformed range is returned.
	maxRecords := 1200
	if typ == protocol.Daily {
		maxRecords = 160
	}
	if typ == protocol.Monthly || typ == protocol.Total {
		maxRecords = 48
	}
	for i := 0; i < maxRecords && !start.After(now); i++ {
		t := start
		err := step(fmt.Sprintf("%s record %s", name, t.Format("2006-01-02 15:04")), func() error {
			v, err := c.ReadArchiveRecord(typ, t, es)
			if err == nil {
				printValues(v)
			}
			return err
		})
		if err != nil {
			return err
		}
		if typ == protocol.Hourly {
			start = start.Add(time.Hour)
		} else {
			start = start.AddDate(0, 0, 1)
		}
		if typ == protocol.Monthly || typ == protocol.Total {
			start = start.AddDate(0, 1, 0)
			start = start.AddDate(0, 0, -1)
		}
		_ = ctx
	}
	return nil
}

func parseRangeStart(d []byte, typ int) (time.Time, error) {
	if len(d) < 3 {
		return time.Time{}, fmt.Errorf("invalid range response: %x", d)
	}
	y, m, day := 2000+int(d[2]), time.Month(d[1]), int(d[0])
	if m < 1 || m > 12 || day < 1 || day > 31 {
		return time.Time{}, fmt.Errorf("invalid range date: %x", d[:3])
	}
	t := time.Date(y, m, day, 0, 0, 0, 0, time.Local)
	if typ == protocol.Monthly || typ == protocol.Total {
		t = time.Date(y, m, 1, 23, 0, 0, 0, time.Local)
	}
	return t, nil
}

func filter(es []model.Element, typ int) []model.Element {
	var out []model.Element
	for _, e := range es {
		if meaningful(e.Address, typ) {
			out = append(out, e)
		}
	}
	return out
}
func meaningful(a, typ int) bool {
	switch typ {
	case protocol.Hourly, protocol.Daily, protocol.Monthly:
		return (a >= 0 && a <= 18) || (a >= 22 && a <= 40) || (a >= 77 && a <= 82)
	case protocol.Total, protocol.CurrentTotal:
		return (a >= 3 && a <= 8) || (a >= 11 && a <= 13) || (a >= 17 && a <= 18) || (a >= 25 && a <= 30) || (a >= 33 && a <= 35) || (a >= 39 && a <= 40) || a == 81
	case protocol.Current:
		return a <= 2 || (a >= 9 && a <= 10) || a == 14 || a == 15 || a == 16 || (a >= 19 && a <= 21) || (a >= 22 && a <= 24) || (a >= 31 && a <= 32) || a == 36 || (a >= 41 && a <= 43) || a == 77 || a == 78 || a == 81 || a == 82
	}
	return false
}
func printValues(v map[string]model.Value) {
	for k, x := range v {
		fmt.Printf("  %-20s value=%v quality=0x%02X ns=0x%02X raw=%X\n", k, x.Value, x.Quality, x.NS, x.Raw)
	}
}
