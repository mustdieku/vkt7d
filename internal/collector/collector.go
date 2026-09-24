package collector

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

type Collector struct {
	Cfg   config.Config
	Log   *slog.Logger
	Store *storage.Store
}

func (x *Collector) Run(ctx context.Context) {
	for {
		x.once(ctx)
		t := time.NewTimer(x.Cfg.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
func (x *Collector) once(ctx context.Context) {
	// DB first: Store was constructed only after a successful Ping. Verify again before serial.
	if e := x.Store.Pool.Ping(ctx); e != nil {
		x.Log.Error("postgres unavailable", "error", e)
		return
	}
	id, e := x.Store.Device(ctx, x.Cfg.DeviceName, x.Cfg.Address, x.Cfg.Port, x.Cfg.Baud)
	if e != nil {
		x.Log.Error("device row", "error", e)
		return
	}
	port, e := protocol.Open(x.Cfg.Port, x.Cfg.Baud)
	if e != nil {
		x.Log.Error("open serial", "error", e)
		x.Store.Log(ctx, id, "serial_error", e)
		return
	}
	defer port.Close()
	c := &protocol.Client{Port: port, Address: byte(x.Cfg.Address), Timeout: x.Cfg.Timeout, Log: x.Log}
	if e = c.Begin(); e != nil {
		x.Log.Error("begin session", "error", e)
		x.Store.Log(ctx, id, "protocol_error", e)
		return
	}
	// Properties are refreshed every session, then active list is obtained.
	if e := x.collectProperties(ctx, c, id); e != nil {
		x.Log.Warn("properties", "error", e)
	}
	es, e := c.ActiveElements()
	if e != nil {
		x.Log.Error("active elements", "error", e)
		x.Store.Log(ctx, id, "protocol_error", e)
		return
	}
	if e = x.Store.UpsertActive(ctx, id, es); e != nil {
		x.Log.Error("save active elements", "error", e)
	}
	// Use a compact list; only elements meaningful for the selected type are sent.
	for _, job := range []struct {
		typ   int
		table string
		step  time.Duration
		batch int
	}{{0, "hourly_archive", time.Hour, x.Cfg.BatchHourly}, {1, "daily_archive", 24 * time.Hour, x.Cfg.BatchDaily}, {2, "monthly_archive", 0, x.Cfg.BatchMonthly}, {3, "total_archive", 0, x.Cfg.BatchTotal}} {
		x.collectArchive(ctx, c, id, es, job.typ, job.table, job.step, job.batch)
	}
	if v, e := c.ReadCurrent(protocol.Current, filter(es, protocol.Current)); e == nil {
		_ = x.Store.SaveCurrent(ctx, "current_values", id, v)
	} else {
		x.Log.Warn("current values", "error", e)
	}
	if v, e := c.ReadCurrent(protocol.CurrentTotal, filter(es, protocol.CurrentTotal)); e == nil {
		_ = x.Store.SaveCurrent(ctx, "current_totals", id, v)
	} else {
		x.Log.Warn("current totals", "error", e)
	}
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
	case protocol.Total:
		return (a >= 3 && a <= 8) || (a >= 11 && a <= 13) || (a >= 17 && a <= 18) || (a >= 25 && a <= 30) || (a >= 33 && a <= 35) || (a >= 39 && a <= 40) || a == 81
	case protocol.Current:
		return (a <= 2) || (a >= 9 && a <= 10) || (a == 14 || a == 15 || a == 16) || (a >= 19 && a <= 21) || (a >= 22 && a <= 24) || (a >= 31 && a <= 32) || (a == 36) || (a >= 41 && a <= 43) || a == 77 || a == 78 || a == 81 || a == 82
	case protocol.CurrentTotal:
		return (a >= 3 && a <= 8) || (a >= 11 && a <= 13) || (a >= 17 && a <= 18) || (a >= 25 && a <= 30) || (a >= 33 && a <= 35) || (a >= 39 && a <= 40) || a == 81
	}
	return false
}

func (x *Collector) collectArchive(ctx context.Context, c *protocol.Client, id int64, active []model.Element, typ int, table string, step time.Duration, batch int) {
	es := filter(active, typ)
	if len(es) == 0 {
		return
	}
	// The type and read list are constant for the sequential read below.
	// The protocol explicitly permits the first three archive operations to
	// be performed once for a cyclic read.
	if e := c.PrepareArchive(typ, es); e != nil {
		x.Log.Warn("prepare archive", "table", table, "error", e)
		return
	}
	last, e := x.Store.Last(ctx, table, id)
	if e != nil {
		x.Log.Warn("last archive", "table", table, "error", e)
		return
	}
	var start time.Time
	if last == nil {
		r, e := c.ReadRange()
		if e != nil {
			x.Log.Warn("archive range", "table", table, "error", e)
			return
		}
		start = parseStart(r, typ)
	} else {
		start = next(*last, typ)
		if x.Cfg.Overlap > 0 {
			if typ == protocol.Hourly {
				start = start.Add(-time.Duration(x.Cfg.Overlap) * time.Hour)
			} else {
				start = start.AddDate(0, 0, -x.Cfg.Overlap)
			}
		}
	}
	limit := batch
	if limit <= 0 {
		limit = 1
	}
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return
		}
		if !due(start, typ, time.Now()) {
			break
		}
		v, fresh, e := x.readArchiveRecord(c, typ, start, es, active)
		if e != nil {
			if protocol.IsExceptionCode(e, 3) {
				// 0x3FF6 gives an archive lower bound. Some devices can still
				// return code 3 for that exact timestamp (rollover/incomplete
				// first record). Do not permanently block collection on it.
				start = advance(start, typ)
				i--
				continue
			}
			x.Log.Warn("archive read", "type", typ, "date", start, "error", e)
			return
		}
		if len(fresh) > 0 {
			active = fresh
			es = filter(active, typ)
		}
		if e = x.Store.SaveArchive(ctx, table, id, start, v); e != nil {
			x.Log.Error("save archive", "table", table, "error", e)
			return
		}
		x.Log.Info("archive saved", "table", table, "date", start)
		if typ == protocol.Hourly {
			start = start.Add(time.Hour)
		} else {
			start = next(start, typ)
		}
	}
}

func (x *Collector) readArchiveRecord(c *protocol.Client, typ int, when time.Time, es []model.Element, active []model.Element) (map[string]model.Value, []model.Element, error) {
	for attempt := 0; attempt < 2; attempt++ {
		v, err := c.ReadArchiveData(typ, when, es)
		if err == nil {
			return v, active, nil
		}
		if !protocol.IsExceptionCode(err, 5) {
			return nil, active, err
		}

		// The record belongs to another measurement scheme. VKT-7 requires
		// refreshing the active list and rebuilding the read list.
		fresh, err := c.ActiveElements()
		if err != nil {
			return nil, active, err
		}
		es = filter(fresh, typ)
		if len(es) == 0 {
			return nil, active, fmt.Errorf("no active elements for archive type %d after scheme change", typ)
		}
		if err = c.SetReadList(es); err != nil {
			return nil, active, err
		}
		active = fresh
	}
	return nil, active, &protocol.ExceptionError{Code: 5, Function: 0x03}
}

func advance(t time.Time, typ int) time.Time {
	switch typ {
	case protocol.Hourly:
		return t.Add(time.Hour)
	case protocol.Monthly, protocol.Total:
		return t.AddDate(0, 1, 0)
	default:
		return t.AddDate(0, 0, 1)
	}
}

func due(t time.Time, typ int, now time.Time) bool {
	switch typ {
	case protocol.Hourly:
		return !t.After(now.Truncate(time.Hour).Add(-time.Hour))
	case protocol.Daily:
		return !t.After(now.AddDate(0, 0, -1))
	case protocol.Monthly, protocol.Total:
		// Conservative: never request a future month. The device's report-day setting
		// is persisted in the device row and should be used for production scheduling.
		return !t.After(now.AddDate(0, 0, -1))
	default:
		return false
	}
}

func next(t time.Time, typ int) time.Time {
	switch typ {
	case protocol.Hourly:
		return t.Add(time.Hour)
	case protocol.Monthly, protocol.Total:
		return time.Date(
			t.Year(),
			t.Month()+1,
			1,
			0, 0, 0, 0,
			t.Location(),
		)
	default:
		return t.AddDate(0, 0, 1)
	}
}

func parseStart(d []byte, typ int) time.Time {
	if len(d) >= 3 {
		y := 2000 + int(d[2])
		m := time.Month(d[1])
		day := int(d[0])
		if typ == protocol.Monthly || typ == protocol.Total {
			// The protocol exposes hourly/daily archive starts, not a monthly start.
			// Use the beginning of the reported daily archive as a conservative
			// lower bound. Code 3 is handled by collectArchive.
			return time.Date(y, m, 1, 23, 0, 0, 0, time.Local)
		}
		hour := int(d[3])
		if typ == protocol.Daily {
			hour = 23
		}
		return time.Date(y, m, day, hour, 0, 0, 0, time.Local)
	}
	return time.Now().AddDate(-1, 0, 0)
}

func (x *Collector) collectProperties(ctx context.Context, c *protocol.Client, id int64) error {
	es := []model.Element{{44, "t_unit", 7}, {45, "G_unit", 7}, {46, "V_unit", 7}, {47, "M_unit", 7}, {48, "P_unit", 7}, {53, "Qo_unit", 7}, {55, "BNP_unit", 7}, {56, "VOC_unit", 7}, {57, "t_dec", 1}, {59, "V1_dec", 1}, {60, "M1_dec", 1}, {61, "P_dec", 1}, {66, "Qo1_dec", 1}, {69, "V2_dec", 1}, {70, "M2_dec", 1}, {76, "Qo2_dec", 1}}
	if e := c.SetType(protocol.Properties); e != nil {
		return e
	}
	if e := c.SetReadList(es); e != nil {
		return e
	}
	d, e := c.ReadData()
	if e != nil {
		return e
	}
	v, e := protocol.ParseProperties(es, d)
	if e != nil {
		return e
	}
	return x.Store.SaveProperties(ctx, id, v)
}

var _ = fmt.Sprintf
