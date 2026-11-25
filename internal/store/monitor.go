package store

import (
	"context"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

const (
	minuteRes   = 60
	hourRes     = 3600
	minutesKept = 48 * time.Hour      // 1분 값을 이만큼
	metricsKept = 30 * 24 * time.Hour // 1시간 값을 이만큼
)

// Metrics는 자원 사용 기록이다 (MetricStore).
type Metrics struct{ db *DB }

func NewMetrics(db *DB) Metrics { return Metrics{db} }

func (m Metrics) Add(ctx context.Context, at time.Time, points map[model.ServiceID]model.MetricPoint) error {
	tx, err := m.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, p := range points {
		_, err := tx.ExecContext(ctx, `INSERT INTO metrics (at, service, res, cpu, cpu_max, mem_used, mem_max, mem_total, disk_used, disk_total, net_in, net_out)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			at.Unix(), int64(id), minuteRes, p.CPU, max(p.CPU, p.CPUMax), int64(p.MemUsed), int64(max(p.MemUsed, p.MemMax)), int64(p.MemTotal),
			int64(p.DiskUsed), int64(p.DiskTotal), int64(p.NetIn), int64(p.NetOut))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Range는 그 기간의 기록이다 — 서비스마다 오래된 것부터 (1시간 값과 1분 값이 섞여 있다).
func (m Metrics) Range(ctx context.Context, from, to time.Time) (map[model.ServiceID][]model.MetricPoint, error) {
	rows, err := m.db.sql.QueryContext(ctx, `SELECT at, service, cpu, cpu_max, mem_used, mem_max, mem_total, disk_used, disk_total, net_in, net_out
		FROM metrics WHERE at >= ? AND at <= ? ORDER BY at, res DESC`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[model.ServiceID][]model.MetricPoint{}
	for rows.Next() {
		var at, id, used, mx, total, dused, dtotal, in, outB int64
		var p model.MetricPoint
		if err := rows.Scan(&at, &id, &p.CPU, &p.CPUMax, &used, &mx, &total, &dused, &dtotal, &in, &outB); err != nil {
			return nil, err
		}
		p.At = time.Unix(at, 0)
		p.MemUsed, p.MemMax, p.MemTotal = uint64(used), uint64(mx), uint64(total)
		p.DiskUsed, p.DiskTotal, p.NetIn, p.NetOut = uint64(dused), uint64(dtotal), uint64(in), uint64(outB)
		out[model.ServiceID(id)] = append(out[model.ServiceID(id)], p)
	}
	return out, rows.Err()
}

// Compact는 2일 지난 1분 값을 1시간 값(평균·최고, 누적 바이트는 마지막)으로 묶고, 30일 지난 것을 지운다.
// 묶는 경계는 정시라서 한 시간이 두 번 묶이지 않는다.
func (m Metrics) Compact(ctx context.Context, now time.Time) error {
	cut := now.Add(-minutesKept).Truncate(time.Hour).Unix()
	tx, err := m.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO metrics (at, service, res, cpu, cpu_max, mem_used, mem_max, mem_total, disk_used, disk_total, net_in, net_out)
		SELECT (at / 3600) * 3600, service, ?, avg(cpu), max(cpu_max), CAST(avg(mem_used) AS INTEGER), max(mem_max), max(mem_total),
		       max(disk_used), max(disk_total), max(net_in), max(net_out)
		FROM metrics WHERE res = ? AND at < ? GROUP BY service, at / 3600`, hourRes, minuteRes, cut); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metrics WHERE res = ? AND at < ?`, minuteRes, cut); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metrics WHERE at < ?`, now.Add(-metricsKept).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// Audit은 기록이다 (AuditLog).
type Audit struct{ db *DB }

func NewAudit(db *DB) Audit { return Audit{db} }

func (a Audit) Record(ctx context.Context, e model.AuditEntry) error {
	_, err := a.db.sql.ExecContext(ctx, `INSERT INTO audit (at, system, who, ip, action, target, service, ok, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At.Unix(), e.System, cut(e.Who, 64), cut(e.IP, 64), cut(e.Action, 64), cut(e.Target, 200), int64(e.Service), e.OK, cut(e.Detail, 500))
	return err
}

func (a Audit) Recent(ctx context.Context, since time.Time, n int) ([]model.AuditEntry, error) {
	rows, err := a.db.sql.QueryContext(ctx, `SELECT at, system, who, ip, action, target, service, ok, detail FROM audit
		WHERE at >= ? ORDER BY at DESC, id DESC LIMIT ?`, since.Unix(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditEntry
	for rows.Next() {
		var at, svc int64
		var e model.AuditEntry
		if err := rows.Scan(&at, &e.System, &e.Who, &e.IP, &e.Action, &e.Target, &svc, &e.OK, &e.Detail); err != nil {
			return nil, err
		}
		e.At, e.Service = time.Unix(at, 0), model.ServiceID(svc)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a Audit) Prune(ctx context.Context, before time.Time) error {
	_, err := a.db.sql.ExecContext(ctx, `DELETE FROM audit WHERE at < ?`, before.Unix())
	return err
}

// cut은 남길 글자를 n바이트 안으로 자른다 (글자 중간에서 자르지 않는다).
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
