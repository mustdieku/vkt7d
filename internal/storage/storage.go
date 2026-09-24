package storage

import (
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
	"vkt7d/internal/model"
)

//go:embed migrations/001_initial.sql
var schemaFS embed.FS

// Schema is loaded from the migration at build time.
func Schema() string { b, _ := schemaFS.ReadFile("migrations/001_initial.sql"); return string(b) }

type Store struct{ Pool *pgxpool.Pool }

func New(ctx context.Context, url string) (*Store, error) {
	p, e := pgxpool.New(ctx, url)
	if e != nil {
		return nil, e
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return &Store{p}, nil
}
func (s *Store) Close()                            { s.Pool.Close() }
func (s *Store) Ping(ctx context.Context) error    { return s.Pool.Ping(ctx) }
func (s *Store) Migrate(ctx context.Context) error { _, e := s.Pool.Exec(ctx, Schema()); return e }
func (s *Store) Device(ctx context.Context, name string, address int, port string, baud int) (int64, error) {
	var id int64
	e := s.Pool.QueryRow(ctx, `INSERT INTO vkt7.devices(name,address,serial_port,baud_rate) VALUES($1,$2,$3,$4) ON CONFLICT(name) DO UPDATE SET address=excluded.address,serial_port=excluded.serial_port,baud_rate=excluded.baud_rate,updated_at=now() RETURNING id`, name, address, port, baud).Scan(&id)
	return id, e
}
func (s *Store) Touch(ctx context.Context, id int64, fw, sv, sc1, sc2, sub, report, modelNo, db int) error {
	_, e := s.Pool.Exec(ctx, `UPDATE vkt7.devices SET firmware_version=$2,server_version=$3,scheme_tv1=$4,scheme_tv2=$5,subscriber_id=$6,report_day=$7,model=$8,active_db=$9,last_seen_at=now(),updated_at=now() WHERE id=$1`, id, fw, sv, sc1, sc2, sub, report, modelNo, db)
	return e
}
func (s *Store) UpsertActive(ctx context.Context, id int64, es []model.Element) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `DELETE FROM vkt7.active_elements WHERE device_id=$1`, id); e != nil {
		return e
	}
	for _, x := range es {
		if _, e = tx.Exec(ctx, `INSERT INTO vkt7.active_elements(device_id,element_address,element_size) VALUES($1,$2,$3) ON CONFLICT(device_id,element_address) DO UPDATE SET element_size=excluded.element_size,last_seen_at=now()`, id, x.Address, x.Size); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func encode(v map[string]model.Value) (vals, q, ns, raw []byte) {
	a := map[string]any{}
	b := map[string]any{}
	c := map[string]any{}
	d := map[string]string{}
	for k, x := range v {
		a[k] = x.Value
		b[k] = x.Quality
		c[k] = x.NS
		d[k] = hex.EncodeToString(x.Raw)
	}
	vals, _ = json.Marshal(a)
	q, _ = json.Marshal(b)
	ns, _ = json.Marshal(c)
	raw, _ = json.Marshal(d)
	return
}
func (s *Store) SaveArchive(ctx context.Context, table string, id int64, t time.Time, v map[string]model.Value) error {
	vals, q, ns, raw := encode(v)
	var sql string
	switch table {
	case "hourly_archive":
		sql = `INSERT INTO vkt7.hourly_archive(device_id,archive_time,"values",quality,ns,raw) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(device_id,archive_time) DO UPDATE SET "values"=excluded."values",quality=excluded.quality,ns=excluded.ns,raw=excluded.raw,collected_at=now()`
	case "daily_archive", "monthly_archive", "total_archive":
		sql = fmt.Sprintf(`INSERT INTO vkt7.%s(device_id,archive_date,"values",quality,ns,raw) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(device_id,archive_date) DO UPDATE SET "values"=excluded."values",quality=excluded.quality,ns=excluded.ns,raw=excluded.raw,collected_at=now()`, table)
	default:
		return fmt.Errorf("bad archive table %q", table)
	}
	_, e := s.Pool.Exec(ctx, sql, id, t, vals, q, ns, raw)
	return e
}
func (s *Store) SaveProperties(ctx context.Context, id int64, v map[string]model.Value) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for name, x := range v {
		addr := propertyAddress(name)
		if addr < 0 {
			return fmt.Errorf("unknown VKT-7 property %q", name)
		}
		var numeric any
		switch n := x.Value.(type) {
		case int64:
			numeric = float64(n)
		case int:
			numeric = float64(n)
		case float32:
			numeric = float64(n)
		case float64:
			numeric = n
		}
		_, err = tx.Exec(ctx, `INSERT INTO vkt7.properties(device_id,element_address,name,value_text,numeric_value,raw)
			VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(device_id,element_address) DO UPDATE SET
			name=excluded.name,value_text=excluded.value_text,numeric_value=excluded.numeric_value,raw=excluded.raw,updated_at=now()`,
			id, addr, name, fmt.Sprint(x.Value), numeric, x.Raw)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func propertyAddress(name string) int {
	switch name {
	case "t_unit": return 44
	case "G_unit": return 45
	case "V_unit": return 46
	case "M_unit": return 47
	case "P_unit": return 48
	case "Qo_unit": return 53
	case "BNP_unit": return 55
	case "VOC_unit": return 56
	case "t_dec": return 57
	case "V1_dec": return 59
	case "M1_dec": return 60
	case "P1_dec": return 61
	case "Qo1_dec": return 66
	case "V2_dec": return 69
	case "M2_dec": return 70
	case "Qo2_dec": return 76
	default: return -1
	}
}
func (s *Store) SaveCurrent(ctx context.Context, table string, id int64, v map[string]model.Value) error {
	vals, q, ns, raw := encode(v)
	_, e := s.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO vkt7.%s(device_id,"values",quality,ns,raw) VALUES($1,$2,$3,$4,$5)`, table), id, vals, q, ns, raw)
	return e
}
func (s *Store) Last(ctx context.Context, table string, id int64) (*time.Time, error) {
	var t time.Time
	var e error
	if table == "hourly_archive" {
		e = s.Pool.QueryRow(ctx, `SELECT archive_time FROM vkt7.hourly_archive WHERE device_id=$1 ORDER BY archive_time DESC LIMIT 1`, id).Scan(&t)
	} else {
		e = s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT archive_date::timestamp FROM vkt7.%s WHERE device_id=$1 ORDER BY archive_date DESC LIMIT 1`, table), id).Scan(&t)
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return &t, nil
}
func (s *Store) CurrentLast(ctx context.Context, table string, id int64) (*time.Time, error) {
	var t time.Time
	e := s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT received_at FROM vkt7.%s WHERE device_id=$1 ORDER BY received_at DESC LIMIT 1`, table), id).Scan(&t)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return &t, nil
}
func (s *Store) Log(ctx context.Context, id int64, status string, err error) {
	var x any
	if err != nil {
		x = err.Error()
	}
	_, _ = s.Pool.Exec(ctx, `INSERT INTO vkt7.collection_log(device_id,status,error) VALUES($1,$2,$3)`, id, status, x)
}
