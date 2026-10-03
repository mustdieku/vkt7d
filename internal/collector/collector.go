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
    port, e := protocol.Open(
        x.Cfg.Port,
        x.Cfg.Baud,
        x.Log,
        x.Cfg.DebugSerial,
    )
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
	serverVersion := 0
	// VKT-7 protocol 5.1:
	// after BeginSession the first ReadData must be performed so that the
	// server-version field is consumed before properties/archive operations.
	initialData, e := c.ReadData()
	if e != nil {
		x.Log.Error("initial read-data", "error", e)
		x.Store.Log(ctx, id, "protocol_error", e)
		return
	}
	if len(initialData) > 0 {
		serverVersion = int(initialData[0])
	}
	reportDay := 0
	var archiveMeta model.Record
	firmware := 0
	schemeTV1 := 0
	schemeTV2 := 0
	subscriberID := ""
	modelNo := 0
	serviceOK := false
	if raw, e := c.ReadService(); e != nil {
		x.Log.Warn("service information", "error", e)
	} else if svc, e := protocol.ParseService(raw); e != nil {
		x.Log.Warn("service information", "error", e)
	} else {
		serviceOK = true
		firmware = svc.Firmware
		schemeTV1 = svc.SchemeTV1
		schemeTV2 = svc.SchemeTV2
		subscriberID = svc.Subscriber
		reportDay = svc.ReportDay
		modelNo = svc.Model
		archiveMeta.SchemeTV1 = &schemeTV1
		archiveMeta.SchemeTV2 = &schemeTV2
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
	// ReadActiveDB is valid for a normal data type. The service record already
	// supplied the TV1/TV2 scheme numbers, so this extra read is only needed for
	// the active database field. The archive collector restores its own type
	// before reading archive data.
	if e := c.SetType(protocol.Current); e != nil {
		x.Log.Warn("select current type for archive metadata", "error", e)
	} else if db, _, _, e := c.ReadActiveDB(); e != nil {
		x.Log.Warn("active database", "error", e)
	} else {
		activeDB := int(db)
		archiveMeta.ActiveDB = &activeDB
	}
	if serviceOK && archiveMeta.ActiveDB != nil {
		if e := x.Store.Touch(ctx, id, firmware, serverVersion, schemeTV1, schemeTV2, subscriberID, reportDay, modelNo, *archiveMeta.ActiveDB); e != nil {
			x.Log.Warn("device metadata", "error", e)
		}
	}
	// Use a compact list; only elements meaningful for the selected type are sent.
	for _, job := range []struct {
		typ   int
		table string
		step  time.Duration
		batch int
	}{{0, "hourly_archive", time.Hour, x.Cfg.BatchHourly}, {1, "daily_archive", 24 * time.Hour, x.Cfg.BatchDaily}, {2, "monthly_archive", 0, x.Cfg.BatchMonthly}, {3, "total_archive", 0, x.Cfg.BatchTotal}} {
		x.collectArchive(ctx, c, id, es, job.typ, job.table, job.step, job.batch, reportDay, &archiveMeta)
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

func (x *Collector) collectArchive(ctx context.Context, c *protocol.Client, id int64, active []model.Element, typ int, table string, step time.Duration, batch int, reportDay int, meta *model.Record) {
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
		v, fresh, freshMeta, e := x.readArchiveRecord(c, typ, start, es, active, meta)
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
		if freshMeta != nil {
			*meta = *freshMeta
		}
		if e = x.Store.SaveArchive(ctx, table, id, start, v, meta); e != nil {
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

// archiveReadChunkBytes is deliberately kept below the maximum VKT-7 frame
// size. Some VKT-7 firmware versions behave incorrectly when a large read
// list is used: the write to 0x3FFF is acknowledged, but the following
// 0x3FFE response may contain a truncated/stale data section.
//
// The protocol itself allows a much larger frame, but splitting the list
// keeps the actual response small and makes the collector independent of
// the transmitter-buffer implementation of a particular VKT-7 firmware.
const archiveReadChunkBytes = 60

func splitArchiveReadList(es []model.Element) [][]model.Element {
	if len(es) == 0 {
		return nil
	}

	var chunks [][]model.Element
	var current []model.Element
	currentBytes := 0

	for _, e := range es {
		// Each returned element consists of:
		//   value[e.Size] + quality[1] + NS[1]
		n := e.Size + 2

		// An individual element must always fit.
		if n > archiveReadChunkBytes {
			if len(current) > 0 {
				chunks = append(chunks, current)
				current = nil
				currentBytes = 0
			}
			chunks = append(chunks, []model.Element{e})
			continue
		}

		if currentBytes+n > archiveReadChunkBytes && len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentBytes = 0
		}

		current = append(current, e)
		currentBytes += n
	}

	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	return chunks
}

func (x *Collector) readArchiveRecord(c *protocol.Client, typ int, when time.Time, es []model.Element, active []model.Element, meta *model.Record) (map[string]model.Value, []model.Element, *model.Record, error) {
	// A VKT-7 archive response is a variable-length sequence. Do not assume
	// that one large read list is safe for every firmware revision.
	//
	// The official protocol explicitly warns about transmitter-buffer
	// overflow when too many active elements are requested.
	for attempt := 0; attempt < 2; attempt++ {
		chunks := splitArchiveReadList(es)
		if len(chunks) == 0 {
			return nil, active, nil, fmt.Errorf(
				"empty archive read list for type %d", typ,
			)
		}

		result := make(map[string]model.Value)
		schemeChanged := false

		for chunkNo, chunk := range chunks {
			if err := c.SetReadList(chunk); err != nil {
				if protocol.IsExceptionCode(err, 5) {
					schemeChanged = true
					break
				}
				return nil, active, nil, fmt.Errorf(
					"set archive read list chunk %d/%d: %w",
					chunkNo+1, len(chunks), err,
				)
			}

			// The date is deliberately written for every chunk. The VKT-7
			// protocol says that ReadData uses the date written last, while
			// changing 0x3FFF changes only the read list.
			if err := c.SetDate(
				when,
				typ == protocol.Daily ||
					typ == protocol.Monthly ||
					typ == protocol.Total,
			); err != nil {
				if protocol.IsExceptionCode(err, 3) {
					return nil, active, nil, err
				}
				if protocol.IsExceptionCode(err, 5) {
					schemeChanged = true
					break
				}
				return nil, active, nil, fmt.Errorf(
					"set archive date %s chunk %d/%d: %w",
					when.Format(time.RFC3339),
					chunkNo+1,
					len(chunks),
					err,
				)
			}

			data, err := c.ReadData()
			if err != nil {
				if protocol.IsExceptionCode(err, 3) {
					return nil, active, nil, err
				}
				if protocol.IsExceptionCode(err, 5) {
					schemeChanged = true
					break
				}
				return nil, active, nil, fmt.Errorf(
					"read archive %s chunk %d/%d: %w",
					when.Format(time.RFC3339),
					chunkNo+1,
					len(chunks),
					err,
				)
			}

			// ParseElements must consume exactly the data represented by the
			// current read list. This catches stale/truncated VKT-7 responses
			// instead of silently writing corrupted records.
			values, err := protocol.ParseElements(chunk, data, typ)
			if err != nil {
				return nil, active, nil, fmt.Errorf(
					"parse archive %s chunk %d/%d: %w; elements=%d data_len=%d",
					when.Format(time.RFC3339),
					chunkNo+1,
					len(chunks),
					err,
					len(chunk),
					len(data),
				)
			}

			for name, value := range values {
				result[name] = value
			}
		}

		if !schemeChanged {
			return result, active, meta, nil
		}

		// The record belongs to another measurement scheme. The VKT-7
		// protocol requires refreshing the active list and rebuilding
		// the read list before retrying the same timestamp.
		fresh, err := c.ActiveElements()
		if err != nil {
			return nil, active, nil, err
		}

		es = filter(fresh, typ)
		if len(es) == 0 {
			return nil, active, nil, fmt.Errorf(
				"no active elements for archive type %d after scheme change",
				typ,
			)
		}

		active = fresh
		// A scheme change invalidates the scheme metadata captured at session
		// start. ReadScheme is a >=1.9 diagnostic operation, so it is used only
		// when the device explicitly reports the scheme-change condition.
		if err := c.SetType(protocol.Current); err != nil {
			return nil, active, nil, fmt.Errorf("select current type after scheme change: %w", err)
		}
		s1, _, _, err := c.ReadScheme(1)
		if err != nil {
			return nil, active, nil, fmt.Errorf("refresh TV1 scheme after scheme change: %w", err)
		}
		s2, _, _, err := c.ReadScheme(2)
		if err != nil {
			return nil, active, nil, fmt.Errorf("refresh TV2 scheme after scheme change: %w", err)
		}
		db, _, _, err := c.ReadActiveDB()
		if err != nil {
			return nil, active, nil, fmt.Errorf("refresh active DB after scheme change: %w", err)
		}
		i1, i2, idb := int(s1), int(s2), int(db)
		meta = &model.Record{SchemeTV1: &i1, SchemeTV2: &i2, ActiveDB: &idb}
		if err := c.SetType(byte(typ)); err != nil {
			return nil, active, nil, fmt.Errorf("restore archive type %d after scheme change: %w", typ, err)
		}
		if err := c.SetReadList(es); err != nil {
			return nil, active, nil, fmt.Errorf("restore archive read list after scheme change: %w", err)
		}
	}

	return nil, active, nil, &protocol.ExceptionError{
		Code:     5,
		Function: 0x03,
	}
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
