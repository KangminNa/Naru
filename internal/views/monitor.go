package views

import (
	"context"
	"sort"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// MonitorParts는 서버 화면 모으기가 쓰는 것들이다.
type MonitorParts struct {
	Metrics  contract.MetricStore
	Audit    contract.AuditLog
	Services contract.ServiceReader
	Clock    contract.Clock
}

type monitorViewer struct{ p MonitorParts }

func NewMonitorViewer(p MonitorParts) contract.MonitorViewer { return monitorViewer{p} }

const (
	eventsShown = 30
	auditLooked = 90 * 24 * time.Hour
)

// Monitor는 그 기간의 기록을 칸(Step)으로 묶는다 — CPU·메모리는 평균과 최고, 디스크는 마지막, 트래픽은 그 칸 동안 받고 보낸 양.
func (v monitorViewer) Monitor(ctx context.Context, r model.MetricRange) (model.MonitorView, error) {
	now := v.p.Clock.Now()
	view := model.MonitorView{Range: r, From: now.Add(-r.Span()), To: now, Step: r.Step()}
	// 트래픽은 누적값의 차이라서 기간 앞의 한 칸을 더 읽는다
	data, err := v.p.Metrics.Range(ctx, view.From.Add(-view.Step), now)
	if err != nil {
		return view, err
	}
	view.Server = buckets(data[0], view.From, view.Step)

	events, _ := v.p.Audit.Recent(ctx, view.From, 2000)
	restarts, ooms := map[model.ServiceID]int{}, map[model.ServiceID]int{}
	for _, e := range events {
		if !e.System {
			continue
		}
		switch e.Action {
		case "service.restarted":
			restarts[e.Service]++
		case "service.oom":
			restarts[e.Service]++
			ooms[e.Service]++
		}
		if len(view.Events) < eventsShown {
			view.Events = append(view.Events, e)
		}
	}
	services, err := v.p.Services.List(ctx)
	if err != nil {
		return view, err
	}
	for _, s := range services {
		pts := buckets(data[s.ID], view.From, view.Step)
		if len(pts) == 0 && restarts[s.ID] == 0 {
			continue // 컨테이너가 없는 종류(정적 사이트·외부 연결)와 아직 기록이 없는 것
		}
		view.Services = append(view.Services, model.ServiceMetrics{Service: s.ID, Name: s.Name.String(), Points: pts, Restarts: restarts[s.ID], OOM: ooms[s.ID]})
	}
	return view, nil
}

// Audit는 관리자가 한 일이다 — 최근 것부터 n개. 서비스 이름이 비어 있으면 지금 이름을 채운다 (지운 서비스는 번호로).
func (v monitorViewer) Audit(ctx context.Context, n int) ([]model.AuditEntry, error) {
	all, err := v.p.Audit.Recent(ctx, v.p.Clock.Now().Add(-auditLooked), n*4)
	if err != nil {
		return nil, err
	}
	names := map[model.ServiceID]string{}
	if services, err := v.p.Services.List(ctx); err == nil {
		for _, s := range services {
			names[s.ID] = s.Name.String()
		}
	}
	var out []model.AuditEntry
	for _, e := range all {
		if e.System {
			continue
		}
		if e.Target == "" && e.Service != 0 {
			e.Target = names[e.Service]
		}
		out = append(out, e)
		if len(out) == n {
			break
		}
	}
	return out, nil
}

// buckets는 기록(오래된 것부터)을 from부터 step 칸으로 묶는다. 기록이 없는 칸은 빠진다.
func buckets(points []model.MetricPoint, from time.Time, step time.Duration) []model.MetricPoint {
	sort.SliceStable(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
	type acc struct {
		p      model.MetricPoint
		n      int
		cpu    float64
		mem    float64
		hasNet bool
	}
	slots := map[int]*acc{}
	var order []int
	var prevIn, prevOut uint64
	havePrev := false
	for _, p := range points {
		// 누적 바이트의 차이 — 줄었으면 웹서버가 다시 시작된 것이라 그 칸은 0부터 다시 센다
		var dIn, dOut uint64
		if havePrev && p.NetIn >= prevIn && p.NetOut >= prevOut {
			dIn, dOut = p.NetIn-prevIn, p.NetOut-prevOut
		}
		prevIn, prevOut, havePrev = p.NetIn, p.NetOut, true
		if p.At.Before(from) {
			continue
		}
		i := int(p.At.Sub(from) / step)
		a := slots[i]
		if a == nil {
			a = &acc{}
			a.p.At = from.Add(time.Duration(i) * step)
			slots[i] = a
			order = append(order, i)
		}
		a.n++
		a.cpu += p.CPU
		a.mem += float64(p.MemUsed)
		a.p.CPUMax = max(a.p.CPUMax, p.CPUMax, p.CPU)
		a.p.MemMax = max(a.p.MemMax, p.MemMax, p.MemUsed)
		a.p.MemTotal = max(a.p.MemTotal, p.MemTotal)
		if p.DiskTotal > 0 {
			a.p.DiskUsed, a.p.DiskTotal = p.DiskUsed, p.DiskTotal
		}
		a.p.NetIn += dIn
		a.p.NetOut += dOut
	}
	out := make([]model.MetricPoint, 0, len(order))
	for _, i := range order {
		a := slots[i]
		a.p.CPU = a.cpu / float64(a.n)
		a.p.MemUsed = uint64(a.mem / float64(a.n))
		out = append(out, a.p)
	}
	return out
}
