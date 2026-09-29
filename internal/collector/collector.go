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
	c := &protocol.Client{Port: port, Address: byte(x.Cfg.Address), Timeout: x.Cfg.Timeout, Log: x.Log, Debug: x.Cfg.DebugSerial}
	if e = c.Begin(); e != nil {
		x.Log.Error("begin session", "error", e)
		x.Store.Log(ctx, id, "protocol_error", e)
		return
	}
	reportDay := 0
	if raw, e := c.ReadService(); e != nil {
		x.Log.Warn("service information", "error", e)
	} else if svc, e := protocol.ParseService(raw); e != nil {
		x.Log.Warn("service information", "error", e)
	} else {
		reportDay = svc.ReportDay
	}
	// Properties are refreshed every session, then active list is obtained.
	if e := x.collectProperties(ctx, c, id); e != nil {
		x.Log.Warn("properties", "error", e)
	}
	x.Log.Debug("collector: reading active elements")
	es, e := c.ActiveElements()
	if e != nil {
		x.Log.Error("active elements", "error", e)
		x.Store.Log(ctx, id, "protocol_error", e)
		return
	}
	if len(es) == 0 {
		e := fmt.Errorf("VKT-7 returned an empty active-element list")
		x.Log.Error("active elements", "error", e)
		x.Store.Log(ctx, id, "protocol_error", e)
		return
	}
	x.Log.Debug("collector: active elements ready", "count", len(es))
	// The report day is required for correct positioning of monthly and
	// total archives.  0 means that it is not known yet.
	reportDay, e = x.Store.ReportDay(ctx, id)
	if e != nil {
		x.Log.Warn("device report day", "error", e)
		reportDay = 0
	}
	if reportDay < 1 || reportDay > 31 {
		reportDay = 30
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
		x.collectArchive(ctx, c, id, es, job.typ, job.table, job.step, job.batch, reportDay)
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

func (x *Collector) collectArchive(ctx context.Context, c *protocol.Client, id int64, active []model.Element, typ int, table string, step time.Duration, batch int, reportDay int) {
	x.Log.Info("archive start", "table", table, "type", typ)

	// VKT-7 protocol section 5.4 requires this order for every archive type:
	// set value type -> read active elements -> write read list -> write date
	// -> read data. The active list is independent of the archive type, but
	// the protocol explicitly requires it to be requested as part of the
	// archive preparation and it can change after a scheme switch.
	if e := c.SetType(byte(typ)); e != nil {
		x.Log.Warn("set archive type", "table", table, "error", e)
		return
	}
	freshActive, e := c.ActiveElements()
	if e != nil {
		x.Log.Warn("archive active elements", "table", table, "error", e)
		return
	}
	if len(freshActive) == 0 {
		x.Log.Warn("archive active elements", "table", table, "error", "empty active-element list")
		return
	}
	active = freshActive
	es := filter(active, typ)
	if len(es) == 0 {
		x.Log.Warn("archive read list", "table", table, "error", "no meaningful active elements")
		return
	}
	if e := c.SetReadList(es); e != nil {
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
		start = parseStart(r, typ, reportDay)
	} else {
		start = next(*last, typ, reportDay)
		if x.Cfg.Overlap > 0 {
			if typ == protocol.Hourly {
				start = start.Add(-time.Duration(x.Cfg.Overlap) * time.Hour)
			} else if typ == protocol.Monthly || typ == protocol.Total {
				start = start.AddDate(0, -x.Cfg.Overlap, 0)
			} else {
				start = start.AddDate(0, 0, -x.Cfg.Overlap)
			}
		}
	}
	limit := batch
	if limit <= 0 {
		limit = 1
	}
	x.Log.Info("archive cursor", "table", table, "start", start, "batch", limit, "last", last)
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
				// Exception 3 means that there is no record for the
				// requested chronological mark. This is normal at the
				// beginning/end of an archive and must not terminate the
				// collector.
				//
				// For monthly/total archives we must move to the next
				// report date, not simply add one calendar month to an
				// arbitrary day such as the first day of the month.
				nextStart := advance(start, typ, reportDay)
				if !nextStart.After(start) {
					x.Log.Warn("archive date did not advance",
						"type", typ, "date", start)
					return
				}
				start = nextStart
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
			start = next(start, typ, reportDay)
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

func advance(t time.Time, typ int, reportDay int) time.Time {
	switch typ {
	case protocol.Hourly:
		return t.Add(time.Hour)
	case protocol.Monthly, protocol.Total:
		return monthReportDate(t, 1, reportDay)
	default:
		return t.AddDate(0, 0, 1)
	}
}

// monthReportDate returns the report date for a month. VKT-7 stores monthly
// archive records at the configured report day and hour 23. If a report day
// is outside the month (e.g. 30 February), clamp it to the last day.
func monthReportDate(t time.Time, monthOffset int, reportDay int) time.Time {
	first := time.Date(
		t.Year(),
		t.Month()+time.Month(monthOffset),
		1,
		23, 0, 0, 0,
		t.Location(),
	)

	lastDay := time.Date(
		first.Year(),
		first.Month()+1,
		0,
		23, 0, 0, 0,
		first.Location(),
	).Day()

	day := reportDay
	if day < 1 {
		day = 1
	}
	if day > lastDay {
		day = lastDay
	}

	return time.Date(
		first.Year(),
		first.Month(),
		day,
		23, 0, 0, 0,
		first.Location(),
	)
}

func due(t time.Time, typ int, now time.Time) bool {
	switch typ {
	case protocol.Hourly:
		return !t.After(now.Truncate(time.Hour).Add(-time.Hour))
	case protocol.Daily:
		return !t.After(now.AddDate(0, 0, -1))
	case protocol.Monthly, protocol.Total:
		// Monthly and total records are formed at the end of the report day.
		return t.Before(time.Date(
			now.Year(), now.Month(), now.Day(),
			0, 0, 0, 0, now.Location(),
		))
	default:
		return false
	}
}

func next(t time.Time, typ int, reportDay int) time.Time {
	switch typ {
	case protocol.Hourly:
		return t.Add(time.Hour)
	case protocol.Monthly, protocol.Total:
		return monthReportDate(t, 1, reportDay)
	default:
		return t.AddDate(0, 0, 1)
	}
}

func parseStart(d []byte, typ int, reportDay int) time.Time {
	// 0x3FF6 returns:
	//   [0:4]  start of hourly archive
	//   [4:8]  current date
	//   [8:12] start of daily archive
	//
	// It does NOT return a monthly/total archive start date.
	if len(d) >= 12 {
		var hourly = parseVTDate(d[0:4])
		var daily = parseVTDate(d[8:12])

		switch typ {
		case protocol.Hourly:
			return hourly
		case protocol.Daily:
			return daily
		case protocol.Monthly, protocol.Total:
			// Monthly and total archives use the report day. The
			// daily archive start provides the earliest known month.
			return monthReportDate(daily, 0, reportDay)
		}
	}

	// Older firmware may return only the hourly/current dates.
	if len(d) >= 4 {
		start := parseVTDate(d[0:4])
		switch typ {
		case protocol.Hourly:
			return start
		case protocol.Daily:
			return time.Date(
				start.Year(), start.Month(), start.Day(),
				23, 0, 0, 0, start.Location(),
			)
		case protocol.Monthly, protocol.Total:
			return monthReportDate(start, 0, reportDay)
		}
	}

	return time.Now().AddDate(-1, 0, 0)
}

func parseVTDate(d []byte) time.Time {
	if len(d) >= 4 {
		y := 2000 + int(d[2])
		m := time.Month(d[1])
		day := int(d[0])
		hour := int(d[3])
		return time.Date(y, m, day, hour, 0, 0, 0, time.Local)
	}
	return time.Time{}
}

func (x *Collector) collectProperties(ctx context.Context, c *protocol.Client, id int64) error {
	// Names are the actual VKT-7 element names from the protocol.
	// In particular, pressure precision is P1_dec/P2_dec, not P_dec.
	es := []model.Element{
		{44, "t_unit", 7},
		{45, "G_unit", 7},
		{46, "V_unit", 7},
		{47, "M_unit", 7},
		{48, "P_unit", 7},
		{53, "Qo_unit", 7},
		{55, "BNP_unit", 7},
		{56, "VOC_unit", 7},
		{57, "t_dec", 1},
		{59, "V1_dec", 1},
		{60, "M1_dec", 1},
		{61, "P1_dec", 1},
		{66, "Qo1_dec", 1},
		{69, "V2_dec", 1},
		{70, "M2_dec", 1},
		{76, "Qo2_dec", 1},
	}
	x.Log.Debug("collector: reading properties", "elements", len(es))
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
