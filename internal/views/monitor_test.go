package views

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KangminNa/naru/internal/model"
	"github.com/KangminNa/naru/internal/store"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestBucketsAverageAndPeakAndCountTraffic(t *testing.T) {
	from := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	pts := []model.MetricPoint{
		{At: from.Add(-time.Minute), NetIn: 1000, NetOut: 5000}, // 기간 앞 — 차이의 기준만
		{At: from, CPU: 10, MemUsed: 100, MemTotal: 1000, DiskUsed: 5, DiskTotal: 10, NetIn: 1500, NetOut: 6000},
		{At: from.Add(5 * time.Minute), CPU: 30, MemUsed: 300, MemTotal: 1000, DiskUsed: 6, DiskTotal: 10, NetIn: 1700, NetOut: 6500},
		{At: from.Add(20 * time.Minute), CPU: 50, CPUMax: 70, MemUsed: 500, MemMax: 600, MemTotal: 1000, NetIn: 100, NetOut: 200}, // 웹서버가 다시 시작됨
		{At: from.Add(21 * time.Minute), CPU: 50, MemUsed: 500, MemTotal: 1000, NetIn: 400, NetOut: 900},
	}
	got := buckets(pts, from, 15*time.Minute)
	if len(got) != 2 {
		t.Fatalf("empty slots are left out: %+v", got)
	}
	a, b := got[0], got[1]
	if !a.At.Equal(from) || a.CPU != 20 || a.CPUMax != 30 || a.MemUsed != 200 || a.MemMax != 300 || a.DiskUsed != 6 || a.NetIn != 700 || a.NetOut != 1500 {
		t.Fatalf("average, peak, last disk, traffic in the slot: %+v", a)
	}
	if !b.At.Equal(from.Add(15*time.Minute)) || b.CPUMax != 70 || b.MemMax != 600 || b.NetIn != 300 || b.NetOut != 700 {
		t.Fatalf("a counter that went down starts again from there: %+v", b)
	}
}

func TestMonitorCollectsServerServicesAndEvents(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "naru.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	services := store.NewServices(db)
	name := func(s string) model.ServiceName { n, _ := model.ParseServiceName(s); return n }
	blog, _ := services.Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog"})
	services.Create(ctx, model.NewService{Name: name("nas"), Kind: model.KindExternal, External: "192.168.0.2:80"})
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	metrics, audit := store.NewMetrics(db), store.NewAudit(db)
	for i := 0; i < 60; i++ {
		metrics.Add(ctx, now.Add(-time.Duration(i)*time.Minute), map[model.ServiceID]model.MetricPoint{
			0: {CPU: 5, MemUsed: 1, MemTotal: 4}, blog: {CPU: 1, MemUsed: 50 << 20},
		})
	}
	audit.Record(ctx, model.AuditEntry{At: now.Add(-10 * time.Minute), System: true, Action: "service.oom", Target: "blog", Service: blog, Detail: "1"})
	audit.Record(ctx, model.AuditEntry{At: now.Add(-5 * time.Minute), System: true, Action: "service.restarted", Target: "blog", Service: blog, Detail: "2"})
	audit.Record(ctx, model.AuditEntry{At: now.Add(-3 * time.Minute), Who: "lab", Action: "service.deploy", Service: blog, OK: true})
	audit.Record(ctx, model.AuditEntry{At: now.Add(-2 * time.Hour), System: true, Action: "disk.low", Detail: "9 GB"})

	v := NewMonitorViewer(MonitorParts{Metrics: metrics, Audit: audit, Services: services, Clock: fixedClock{now}})
	mv, err := v.Monitor(ctx, model.RangeHour)
	if err != nil {
		t.Fatal(err)
	}
	if len(mv.Server) != 60 || mv.Step != time.Minute || !mv.From.Equal(now.Add(-time.Hour)) {
		t.Fatalf("%d %v", len(mv.Server), mv.Step)
	}
	if len(mv.Services) != 1 || mv.Services[0].Name != "blog" || mv.Services[0].Restarts != 2 || mv.Services[0].OOM != 1 {
		t.Fatalf("services with records, restarts in the period: %+v", mv.Services)
	}
	if len(mv.Events) != 2 || mv.Events[0].Action != "service.restarted" {
		t.Fatalf("Naru's own events in the period, newest first — not admin actions: %+v", mv.Events)
	}
	if day, _ := v.Monitor(ctx, model.RangeDay); len(day.Events) != 3 || len(day.Server) != 5 {
		t.Fatalf("a day: 15-minute slots, older events too: %d %d", len(day.Events), len(day.Server))
	}

	admin, err := v.Audit(ctx, 10)
	if err != nil || len(admin) != 1 || admin[0].Target != "blog" || admin[0].Who != "lab" {
		t.Fatalf("admin actions only, with the service's name: %+v %v", admin, err)
	}
}
