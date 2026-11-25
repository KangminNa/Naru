// Package watch는 서비스·웹서버·Docker를 지켜본다 (v2-objects §4 J).
// 정해진 간격마다 보고, 바뀔 때만 이벤트를 낸다 — 보내는 일은 알리기(notify)가 듣고 한다.
//
// 알림 폭탄을 막는 규칙:
//   - 두 번 연속(약 1분) 문제일 때만 멈춤으로 본다 — 배포·재시작은 잠깐 깜빡인다.
//   - 직접 멈춘 서비스와 배포 중인 서비스는 보지 않는다.
//   - 처음 보았을 때 이미 문제였던 것은 알리지 않고 기준으로만 삼는다 (Naru를 다시 켤 때마다 쏟아지지 않게).
//   - 인증서 곧 끝남은 주소마다 하루 한 번.
//
// 본 것에 진단(Diagnoser)을 더해 "주의가 필요한 것"(Finding)을 무거운 것부터 담아 둔다 — 화면은 그것을 읽기만 한다.
package watch

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// Parts는 지켜보기가 쓰는 것들이다.
type Parts struct {
	Services      contract.ServiceReader
	Kinds         contract.KindLookup
	Containers    contract.ContainerWatcher
	Ports         contract.PortChecker
	Deployer      contract.Deployer
	WebServer     contract.WebServerSync
	Certs         contract.CertificateReader
	Admin         contract.AdminDomainSetting
	Diagnoser     contract.Diagnoser
	Usage         contract.UsageReader
	Stats         contract.ServerStats // 디스크·CPU·메모리 (없으면 보지 않는다)
	Metrics       contract.MetricStore // 1분마다 남긴다 (없으면 남기지 않는다)
	Audit         contract.AuditLog    // 90일 지난 기록을 지운다
	WebServerName string               // 웹서버 컨테이너 이름 — 트래픽을 읽는다
	Clock         contract.Clock
	Events        contract.EventPublisher
	Every         time.Duration // 0이면 30초
	Log           *slog.Logger
}

type healthWatcher struct {
	p Parts

	mu   sync.RWMutex
	snap model.WatchSnapshot

	checked    bool
	services   map[model.ServiceID]*flag
	docker     flag
	webServer  flag
	certWarned map[string]time.Time
	disk       diskLevel // 마지막으로 본 디스크 상태
	cpu, mem   pressure  // 서버 CPU·메모리가 오래 높은가
	restarts   map[model.ServiceID]*restartSeen
	recordedAt time.Time                           // 마지막으로 남긴 때
	tidiedAt   time.Time                           // 마지막으로 기록을 정리한 때
	diagnosed  map[model.ServiceID][]model.Finding // 서비스마다 마지막 진단 — 배포 중에는 이것을 그대로 둔다
}

// flag는 하나의 "괜찮은가"를 지켜본다 — 두 번 연속 나빠야 멈춤, 한 번 좋아지면 복구.
type flag struct {
	bad        int
	down       bool
	badAtStart bool // 처음 보았을 때부터 나빴다 — 멈춤이 되어도 알리지 않는다
	since      time.Time
	why        string
}

const (
	same = iota
	wentDown
	cameUp
)

func (f *flag) observe(ok, first bool, now time.Time, why string) int {
	if ok {
		f.bad, f.badAtStart, f.why = 0, false, ""
		if f.down {
			f.down = false
			return cameUp
		}
		return same
	}
	f.bad++
	f.why = why
	if first {
		f.badAtStart = true
	}
	if f.bad >= 2 && !f.down {
		f.down, f.since = true, now
		if f.badAtStart {
			return same
		}
		return wentDown
	}
	return same
}

// NewHealthWatcher는 지켜보기를 만든다. run은 ctx가 끝날 때까지 정해진 간격으로 본다.
func NewHealthWatcher(p Parts) (contract.HealthWatcher, func(context.Context)) {
	if p.Every == 0 {
		p.Every = 30 * time.Second
	}
	w := &healthWatcher{p: p, services: map[model.ServiceID]*flag{}, certWarned: map[string]time.Time{}, diagnosed: map[model.ServiceID][]model.Finding{},
		restarts: map[model.ServiceID]*restartSeen{}}
	w.snap = model.WatchSnapshot{DockerUp: true, WebServerUp: true}
	return w, w.run
}

func (w *healthWatcher) Snapshot() model.WatchSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snap
}

func (w *healthWatcher) run(ctx context.Context) {
	// 처음에는 조금 기다린다 — 함께 뜬 웹서버가 맞춰질 시간을 준다
	first := w.p.Every / 6
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.check(ctx)
			t.Reset(w.p.Every)
		}
	}
}

func (w *healthWatcher) check(ctx context.Context) {
	now := w.p.Clock.Now()
	first := !w.checked
	w.checked = true

	ws := w.p.WebServer.Status()
	switch w.webServer.observe(ws.Connected, first, now, ws.LastError) {
	case wentDown:
		w.p.Events.Publish(model.WebServerDown{Why: ws.LastError})
	case cameUp:
		w.p.Events.Publish(model.WebServerUp{})
	}

	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	states, derr := w.p.Containers.All(dctx)
	cancel()
	why := ""
	if derr != nil {
		why = derr.Error()
	}
	switch w.docker.observe(derr == nil, first, now, why) {
	case wentDown:
		w.p.Events.Publish(model.DockerDown{Why: why})
	case cameUp:
		w.p.Events.Publish(model.DockerUp{})
	}

	all, err := w.p.Services.List(ctx)
	if err != nil {
		w.p.Log.Warn("watch: list services", "err", err)
		return
	}
	if derr == nil { // Docker를 못 읽으면 서비스를 판정하지 않는다 — 모르는 것을 멈춤으로 보지 않는다
		w.judgeServices(ctx, all, states, now)
	} else {
		states = nil
	}
	findings := w.diagnose(ctx, all, states)
	findings = append(findings, w.warnCertificates(ctx, all, now)...)
	findings = append(findings, w.watchDisk(first)...)
	findings = append(findings, w.watchResources(now)...)
	findings = append(findings, w.watchRestarts(ctx, all, states, now)...)
	usage := w.readUsage(ctx, all, states)
	w.record(ctx, now, usage)
	w.tidy(ctx, now)
	w.publishSnapshot(all, findings, usage, now)
}

// readUsage는 실행 중인 컨테이너 서비스의 CPU·메모리를 읽는다. 하나에 1초쯤 걸려 동시에 넷까지.
// 못 읽은 것은 빼 둔다 — 화면에 "—"로 보인다.
func (w *healthWatcher) readUsage(ctx context.Context, all []model.Service, states model.ContainerStates) map[model.ServiceID]model.ResourceUsage {
	out := map[model.ServiceID]model.ResourceUsage{}
	if states == nil {
		return out
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		slots = make(chan struct{}, 4)
	)
	for _, s := range all {
		tools, ok := w.p.Kinds.Find(s.Kind)
		if !ok || !tools.Destination.Find(s).Container || s.Live.Stopped {
			continue
		}
		name := s.Live.CurrentContainer()
		if st, ok := states[name]; !ok || st.State != "running" {
			continue
		}
		wg.Add(1)
		go func(id model.ServiceID) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if u, err := w.p.Usage.Usage(uctx, name); err == nil {
				mu.Lock()
				out[id] = u
				mu.Unlock()
			}
		}(s.ID)
	}
	wg.Wait()
	return out
}

func (w *healthWatcher) judgeServices(ctx context.Context, all []model.Service, states model.ContainerStates, now time.Time) {
	seen := map[model.ServiceID]bool{}
	for _, s := range all {
		tools, ok := w.p.Kinds.Find(s.Kind)
		if !ok || !tools.Destination.Find(s).Container {
			continue // 지켜보는 것은 컨테이너 서비스뿐이다
		}
		if s.Live.Stopped || s.Live.CurrentContainer() == "" || w.p.Deployer.IsDeploying(s.ID) {
			continue // 직접 멈춤·아직 배포 전·배포 중 — 판정하지 않고, 기록도 지운다
		}
		seen[s.ID] = true
		f := w.services[s.ID]
		newcomer := f == nil
		if newcomer {
			f = &flag{}
			w.services[s.ID] = f
		}
		why := w.problem(ctx, s, states)
		switch f.observe(why == "", newcomer, now, why) {
		case wentDown:
			w.p.Events.Publish(model.ServiceDown{Service: s.ID, Name: s.Name.String(), Why: why})
		case cameUp:
			w.p.Events.Publish(model.ServiceUp{Service: s.ID, Name: s.Name.String()})
		}
	}
	for id := range w.services {
		if !seen[id] {
			delete(w.services, id)
		}
	}
}

// problem은 서비스가 지금 괜찮지 않은 이유다. 괜찮으면 빈 값.
func (w *healthWatcher) problem(ctx context.Context, s model.Service, states model.ContainerStates) string {
	st, ok := states[s.Live.CurrentContainer()]
	if !ok {
		return "missing"
	}
	switch st.State {
	case "restarting":
		return "restarting"
	case "exited", "dead":
		return "crashed"
	case "running":
	default:
		return "" // 만들어짐·일시정지 — 멈춤으로 보지 않는다
	}
	if s.Port == 0 {
		return ""
	}
	pctx, cancel := context.WithTimeout(ctx, answerAfter)
	defer cancel()
	if w.p.Ports.Answers(pctx, reach(s), s.Port) != nil {
		return "noanswer"
	}
	return ""
}

// diagnose는 서비스마다 진단을 받고, 멈춘 서비스는 그 이유를 앞에 더한다.
// 직접 멈춘 서비스는 말하지 않고, 배포 중인 서비스는 마지막 진단을 그대로 둔다.
func (w *healthWatcher) diagnose(ctx context.Context, all []model.Service, states model.ContainerStates) []model.Finding {
	var out []model.Finding
	seen := map[model.ServiceID]bool{}
	for _, s := range all {
		seen[s.ID] = true
		if s.Live.Stopped {
			delete(w.diagnosed, s.ID)
			continue
		}
		if w.p.Deployer.IsDeploying(s.ID) {
			out = append(out, w.diagnosed[s.ID]...)
			continue
		}
		found := w.p.Diagnoser.Diagnose(ctx, s, states)
		if f := w.services[s.ID]; f != nil && f.down && !(f.why == "noanswer" && hasKey(found, "find.port")) {
			down := model.Finding{Service: s.ID, Name: s.Name.String(), Key: "find." + f.why, Level: model.FindingUrgent}
			if f.why == "noanswer" {
				down.Args = []string{portArg(s.Port)}
			}
			found = append([]model.Finding{down}, found...)
		}
		w.diagnosed[s.ID] = found
		out = append(out, found...)
	}
	for id := range w.diagnosed {
		if !seen[id] {
			delete(w.diagnosed, id)
		}
	}
	return out
}

func hasKey(fs []model.Finding, key string) bool {
	for _, f := range fs {
		if f.Key == key {
			return true
		}
	}
	return false
}

// warnCertificates는 곧 끝나는 인증서를 찾는다 — 알림은 주소마다 하루 한 번, 찾은 것은 매번 돌려준다.
func (w *healthWatcher) warnCertificates(ctx context.Context, all []model.Service, now time.Time) []model.Finding {
	certs, err := w.p.Certs.Read(ctx)
	if err != nil || len(certs) == 0 {
		return nil
	}
	type owned struct {
		d    model.DomainName
		id   model.ServiceID
		name string
	}
	var domains []owned
	if d, _ := w.p.Admin.Get(ctx); !d.IsZero() {
		domains = append(domains, owned{d: d})
	}
	for _, s := range all {
		for _, d := range s.Domains {
			if d.HTTPS {
				domains = append(domains, owned{d.Domain, s.ID, s.Name.String()})
			}
		}
	}
	var out []model.Finding
	for _, o := range domains {
		st := certs.For(o.d, now)
		if !st.EndsSoon(now) {
			continue
		}
		out = append(out, model.Finding{Service: o.id, Name: o.name, Key: "find.cert", Level: model.FindingWarning,
			Args: []string{o.d.String(), st.NotAfter.Format("2006-01-02")}})
		if last, ok := w.certWarned[o.d.String()]; ok && now.Sub(last) < 24*time.Hour {
			continue
		}
		w.certWarned[o.d.String()] = now
		w.p.Events.Publish(model.CertificateEndingSoon{Domain: o.d.String(), NotAfter: st.NotAfter})
	}
	return out
}

func (w *healthWatcher) publishSnapshot(all []model.Service, findings []model.Finding, usage map[model.ServiceID]model.ResourceUsage, now time.Time) {
	snap := model.WatchSnapshot{CheckedAt: now, DockerUp: !w.docker.down, WebServerUp: !w.webServer.down, Usage: usage}
	for _, s := range all {
		if f := w.services[s.ID]; f != nil && f.down {
			snap.Down = append(snap.Down, model.DownService{Service: s.ID, Name: s.Name.String(), Why: f.why, Since: f.since})
		}
	}
	sort.Slice(snap.Down, func(i, j int) bool { return snap.Down[i].Name < snap.Down[j].Name })
	// 무거운 것부터, 같은 무게면 서비스 이름순 (서버 전체의 것이 먼저)
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Level != b.Level {
			return a.Level < b.Level
		}
		return a.Name < b.Name
	})
	snap.Findings = findings
	w.mu.Lock()
	w.snap = snap
	w.mu.Unlock()
}

// diskLevel은 디스크 남은 공간의 상태다.
type diskLevel int

const (
	diskFine   diskLevel = iota
	diskLow              // 20% 미만
	diskUrgent           // 10% 미만
)

func diskLevelOf(free, total uint64) diskLevel {
	switch {
	case total == 0:
		return diskFine
	case free*10 < total:
		return diskUrgent
	case free*5 < total:
		return diskLow
	}
	return diskFine
}

// watchDisk는 데이터 폴더가 있는 디스크를 본다. 나빠질 때마다(주의 → 긴급) 알리고, 20% 이상으로 돌아오면 한 번 알린다.
// 처음 보았을 때 이미 모자랐으면 알리지 않고 "주의가 필요한 것"에만 둔다 (다시 켤 때마다 알리지 않게).
func (w *healthWatcher) watchDisk(first bool) []model.Finding {
	if w.p.Stats == nil {
		return nil
	}
	snap := w.p.Stats.Now()
	if !snap.HasDisk || snap.DiskTotal == 0 {
		return nil
	}
	level := diskLevelOf(snap.DiskFree, snap.DiskTotal)
	prev := w.disk
	w.disk = level
	if !first {
		switch {
		case level > prev:
			w.p.Events.Publish(model.DiskLow{Free: snap.DiskFree, Total: snap.DiskTotal, Urgent: level == diskUrgent})
		case level == diskFine && prev != diskFine:
			w.p.Events.Publish(model.DiskOK{Free: snap.DiskFree, Total: snap.DiskTotal})
		}
	}
	if level == diskFine {
		return nil
	}
	f := model.Finding{Key: "find.disk", Level: model.FindingWarning,
		Args: []string{model.HumanBytes(snap.DiskFree), strconv.Itoa(int(snap.DiskFree * 100 / snap.DiskTotal))}}
	if level == diskUrgent {
		f.Level = model.FindingUrgent
	}
	return []model.Finding{f}
}

// ── 자원 — 오래 높은가 ─────────────────────

// pressure는 서버 CPU·메모리 하나를 본다 — high 이상이 hold 동안 이어지면 알리고, low 아래로 내려오면 돌아왔다고 알린다.
type pressure struct {
	since   time.Time // high 이상이 된 때 (아니면 0)
	alerted bool
}

func (w *healthWatcher) track(p *pressure, what string, pct, high, low int, hold time.Duration, now time.Time) []model.Finding {
	switch {
	case pct >= high:
		if p.since.IsZero() {
			p.since = now
		}
		if !p.alerted && now.Sub(p.since) >= hold {
			p.alerted = true
			w.p.Events.Publish(model.ResourceHigh{What: what, Percent: pct})
		}
	case pct < low:
		p.since = time.Time{}
		if p.alerted {
			p.alerted = false
			w.p.Events.Publish(model.ResourceOK{What: what, Percent: pct})
		}
	default:
		if !p.alerted {
			p.since = time.Time{}
		}
	}
	if !p.alerted {
		return nil
	}
	return []model.Finding{{Key: "find." + what, Level: model.FindingWarning, Args: []string{strconv.Itoa(pct)}}}
}

// watchResources는 서버 메모리 90% 이상 5분, CPU 90% 이상 10분을 본다 (돌아옴: 85%·80% 아래).
func (w *healthWatcher) watchResources(now time.Time) []model.Finding {
	if w.p.Stats == nil {
		return nil
	}
	snap := w.p.Stats.Now()
	var out []model.Finding
	if snap.HasMem && snap.MemTotal > 0 {
		out = append(out, w.track(&w.mem, "memory", int(snap.MemUsed*100/snap.MemTotal), 90, 85, 5*time.Minute, now)...)
	}
	if snap.HasCPU {
		out = append(out, w.track(&w.cpu, "cpu", int(snap.CPUPercent), 90, 80, 10*time.Minute, now)...)
	}
	return out
}

// ── 다시 시작 ─────────────────────────────

// restartSeen은 서비스 컨테이너가 Docker에 의해 다시 시작된 횟수를 따라간다.
type restartSeen struct {
	container string
	count     int
	last      time.Time // 마지막으로 다시 시작된 것을 본 때
	oom       bool
	alerted   time.Time
}

const (
	restartShown   = 24 * time.Hour // "주의가 필요한 것"에 이만큼 둔다
	restartAlerted = time.Hour      // 알림은 서비스마다 이만큼에 한 번
)

// watchRestarts는 돌고 있는 앱 컨테이너의 다시 시작 횟수를 본다. 늘었으면 앱이 죽었다가 Docker가 다시 띄운 것이다.
// 배포로 컨테이너가 바뀌면 새로 센다. 직접 멈춘 것·배포 중인 것은 보지 않는다.
func (w *healthWatcher) watchRestarts(ctx context.Context, all []model.Service, states model.ContainerStates, now time.Time) []model.Finding {
	if states == nil {
		return nil
	}
	var out []model.Finding
	for _, s := range all {
		tools, ok := w.p.Kinds.Find(s.Kind)
		name := s.Live.CurrentContainer()
		if !ok || !tools.Destination.Find(s).Container || name == "" || s.Live.Stopped || w.p.Deployer.IsDeploying(s.ID) {
			continue
		}
		if _, ok := states[name]; !ok {
			continue
		}
		octx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := w.p.Containers.One(octx, name)
		cancel()
		if err != nil {
			continue
		}
		r := w.restarts[s.ID]
		if r == nil || r.container != name {
			w.restarts[s.ID] = &restartSeen{container: name, count: st.Restarts}
			continue
		}
		if st.Restarts > r.count {
			r.count, r.last, r.oom = st.Restarts, now, st.OOMKilled
			if now.Sub(r.alerted) >= restartAlerted {
				r.alerted = now
				w.p.Events.Publish(model.ServiceRestarted{Service: s.ID, Name: s.Name.String(), OOM: st.OOMKilled, ExitCode: st.ExitCode, Count: st.Restarts})
			}
		}
		if !r.last.IsZero() && now.Sub(r.last) < restartShown {
			f := model.Finding{Service: s.ID, Name: s.Name.String(), Key: "find.restarted", Level: model.FindingWarning,
				Args: []string{r.last.Local().Format("01-02 15:04"), strconv.Itoa(r.count)}}
			if r.oom {
				f.Key = "find.oom"
			}
			out = append(out, f)
		}
	}
	return out
}

// ── 기록 ──────────────────────────────────

// record는 1분마다 서버와 서비스의 자원 사용을 남긴다. 서버 트래픽은 웹서버 컨테이너가 받고 보낸 누적 바이트다.
func (w *healthWatcher) record(ctx context.Context, now time.Time, usage map[model.ServiceID]model.ResourceUsage) {
	if w.p.Metrics == nil || (!w.recordedAt.IsZero() && now.Sub(w.recordedAt) < time.Minute-5*time.Second) {
		return
	}
	w.recordedAt = now
	points := map[model.ServiceID]model.MetricPoint{}
	if w.p.Stats != nil {
		snap := w.p.Stats.Now()
		p := model.MetricPoint{CPU: snap.CPUPercent, MemUsed: snap.MemUsed, MemTotal: snap.MemTotal}
		if snap.HasDisk && snap.DiskTotal >= snap.DiskFree {
			p.DiskUsed, p.DiskTotal = snap.DiskTotal-snap.DiskFree, snap.DiskTotal
		}
		if w.p.WebServerName != "" && w.p.Usage != nil {
			uctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if u, err := w.p.Usage.Usage(uctx, w.p.WebServerName); err == nil {
				p.NetIn, p.NetOut = u.NetIn, u.NetOut
			}
			cancel()
		}
		points[0] = p
	}
	for id, u := range usage {
		points[id] = model.MetricPoint{CPU: u.CPUPercent, MemUsed: u.MemUsed, MemTotal: u.MemLimit}
	}
	if err := w.p.Metrics.Add(ctx, now, points); err != nil {
		w.p.Log.Warn("watch: record metrics", "err", err)
	}
}

// auditKept는 기록을 남기는 기간이다.
const auditKept = 90 * 24 * time.Hour

// tidy는 1시간마다 자원 기록을 묶고 오래된 기록을 지운다.
func (w *healthWatcher) tidy(ctx context.Context, now time.Time) {
	if !w.tidiedAt.IsZero() && now.Sub(w.tidiedAt) < time.Hour {
		return
	}
	w.tidiedAt = now
	if w.p.Metrics != nil {
		if err := w.p.Metrics.Compact(ctx, now); err != nil {
			w.p.Log.Warn("watch: compact metrics", "err", err)
		}
	}
	if w.p.Audit != nil {
		if err := w.p.Audit.Prune(ctx, now.Add(-auditKept)); err != nil {
			w.p.Log.Warn("watch: prune audit", "err", err)
		}
	}
}
