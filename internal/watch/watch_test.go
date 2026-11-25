package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KangminNa/naru/internal/kinds"
	"github.com/KangminNa/naru/internal/model"
)

var ctx = context.Background()

type fakeServices struct{ all []model.Service }

func (f *fakeServices) List(context.Context) ([]model.Service, error) { return f.all, nil }
func (f *fakeServices) Get(context.Context, model.ServiceID) (model.Service, error) {
	return model.Service{}, model.ErrNotFound
}
func (f *fakeServices) NameTaken(context.Context, model.ServiceName) bool { return false }

type fakeContainers struct {
	states   model.ContainerStates
	err      error
	restarts map[string]int
	oom      map[string]bool
}

func (f *fakeContainers) All(context.Context) (model.ContainerStates, error) { return f.states, f.err }
func (f *fakeContainers) One(_ context.Context, name string) (model.ContainerState, error) {
	return model.ContainerState{Name: name, Restarts: f.restarts[name], OOMKilled: f.oom[name]}, nil
}
func (f *fakeContainers) Logs(context.Context, string, int) (string, error) { return "", nil }
func (f *fakeContainers) BelongingTo(context.Context, model.ServiceID) ([]string, error) {
	return nil, nil
}

type fakePorts struct {
	deaf  map[string]bool       // 아무 포트도 응답하지 않는 주소
	only  map[string]model.Port // 이 포트만 응답하는 주소
	tried []model.Port
}

func (f *fakePorts) Answers(_ context.Context, host string, port model.Port) error {
	f.tried = append(f.tried, port)
	if f.deaf[host] {
		return errors.New("connection refused")
	}
	if p, ok := f.only[host]; ok && p != port {
		return errors.New("connection refused")
	}
	return nil
}

type fakeUsage struct {
	mu    sync.Mutex
	read  []string
	fails map[string]bool
}

func (f *fakeUsage) Usage(_ context.Context, name string) (model.ResourceUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = append(f.read, name)
	if f.fails[name] {
		return model.ResourceUsage{}, errors.New("no such container")
	}
	return model.ResourceUsage{CPUPercent: 12.5, MemUsed: 100 << 20, MemLimit: 8 << 30}, nil
}

type fakeDeployer struct{ busy map[model.ServiceID]bool }

func (f *fakeDeployer) Deploy(context.Context, model.ServiceID, model.DeployReason) (model.DeploymentID, error) {
	return 0, nil
}
func (f *fakeDeployer) RollBack(context.Context, model.ServiceID, model.DeploymentID) (model.DeploymentID, error) {
	return 0, nil
}
func (f *fakeDeployer) IsDeploying(id model.ServiceID) bool { return f.busy[id] }

type fakeWebServer struct {
	up          bool
	unreachable []model.ReachProblem
}

func (f *fakeWebServer) SyncNow(context.Context) error { return nil }
func (f *fakeWebServer) Status() model.WebServerStatus {
	if f.up {
		return model.WebServerStatus{Connected: true, Unreachable: f.unreachable}
	}
	return model.WebServerStatus{LastError: "caddy: dial unix: no such file", Unreachable: f.unreachable}
}

type fakeCerts struct{ list model.Certificates }

func (f *fakeCerts) Read(context.Context) (model.Certificates, error) { return f.list, nil }

type fakeAdmin struct{ d model.DomainName }

func (f *fakeAdmin) Get(context.Context) (model.DomainName, model.SetBy) {
	return f.d, model.SetByScreen
}
func (f *fakeAdmin) Set(context.Context, model.DomainName) error { return nil }

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type recorder struct {
	mu     sync.Mutex
	events []model.Event
}

func (r *recorder) Publish(e model.Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }
func (r *recorder) take() []model.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

type rig struct {
	w          *healthWatcher
	services   *fakeServices
	containers *fakeContainers
	ports      *fakePorts
	deployer   *fakeDeployer
	web        *fakeWebServer
	certs      *fakeCerts
	clock      *fakeClock
	events     *recorder
	dns        *fakeDNS
	usage      *fakeUsage
	stats      *fakeStats
	metrics    *fakeMetrics
}

// fakeStats는 디스크 남은 공간만 정해 둔다.
type fakeStats struct {
	free, total uint64
	cpu         float64
	memUsed     uint64
}

func (s *fakeStats) Now() model.ServerSnapshot {
	return model.ServerSnapshot{HasDisk: s.total > 0, DiskFree: s.free, DiskTotal: s.total,
		HasCPU: true, CPUPercent: s.cpu, HasMem: true, MemUsed: s.memUsed, MemTotal: 100}
}

// fakeMetrics는 남긴 것을 기억한다.
type fakeMetrics struct {
	added     []map[model.ServiceID]model.MetricPoint
	compacted int
}

func (m *fakeMetrics) Add(_ context.Context, _ time.Time, p map[model.ServiceID]model.MetricPoint) error {
	m.added = append(m.added, p)
	return nil
}
func (m *fakeMetrics) Range(context.Context, time.Time, time.Time) (map[model.ServiceID][]model.MetricPoint, error) {
	return nil, nil
}
func (m *fakeMetrics) Compact(context.Context, time.Time) error { m.compacted++; return nil }

func name(s string) model.ServiceName { n, _ := model.ParseServiceName(s); return n }

func newRig() *rig {
	r := &rig{
		services: &fakeServices{all: []model.Service{
			{ID: 1, Name: name("blog"), Kind: model.KindImage, Port: 80, Live: model.LiveState{Alias: "naru-blog", Instance: "naru-blog-3"}},
			{ID: 2, Name: name("site"), Kind: model.KindStatic, Live: model.LiveState{Release: "4"}},
			{ID: 3, Name: name("nas"), Kind: model.KindExternal, External: "192.168.0.20:5000"},
		}},
		containers: &fakeContainers{states: model.ContainerStates{"naru-blog-3": {Name: "naru-blog-3", Running: true, State: "running"}}},
		ports:      &fakePorts{deaf: map[string]bool{}, only: map[string]model.Port{}},
		deployer:   &fakeDeployer{busy: map[model.ServiceID]bool{}},
		web:        &fakeWebServer{up: true},
		certs:      &fakeCerts{},
		clock:      &fakeClock{now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)},
		events:     &recorder{},
	}
	r.dns = &fakeDNS{ips: map[string][]string{}}
	r.usage = &fakeUsage{fails: map[string]bool{}}
	r.stats = &fakeStats{free: 50 << 30, total: 100 << 30, cpu: 10, memUsed: 40}
	r.metrics = &fakeMetrics{}
	lookup := kinds.NewLookup(kinds.Tools{})
	w, _ := NewHealthWatcher(Parts{
		Services: r.services, Kinds: lookup, Containers: r.containers, Ports: r.ports,
		Deployer: r.deployer, WebServer: r.web, Certs: r.certs, Admin: &fakeAdmin{}, Clock: r.clock, Events: r.events,
		Usage: r.usage, Stats: r.stats, Metrics: r.metrics, WebServerName: "naru-caddy",
		Diagnoser: NewDiagnoser(DiagnoserParts{Kinds: lookup, Ports: r.ports, DNS: r.dns, Admin: &fakeAdmin{}, Certs: r.certs, Clock: r.clock, PublicIP: "198.51.100.24",
			WebServer: r.web, AppNetwork: "shelf-net"}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.w = w.(*healthWatcher)
	return r
}

func (r *rig) check() []model.Event {
	r.clock.now = r.clock.now.Add(30 * time.Second)
	r.w.check(ctx)
	return r.events.take()
}

func (r *rig) crash() {
	r.containers.states["naru-blog-3"] = model.ContainerState{Name: "naru-blog-3", State: "exited", ExitCode: 1}
}

func TestDownNeedsTwoChecksAndEachChangeIsSentOnce(t *testing.T) {
	r := newRig()
	if ev := r.check(); len(ev) != 0 {
		t.Fatalf("a healthy start says nothing: %v", ev)
	}
	r.crash()
	if ev := r.check(); len(ev) != 0 {
		t.Fatalf("one bad check is not enough (deploys and restarts blink): %v", ev)
	}
	ev := r.check()
	if len(ev) != 1 {
		t.Fatalf("the second bad check reports it: %v", ev)
	}
	down, ok := ev[0].(model.ServiceDown)
	if !ok || down.Service != 1 || down.Name != "blog" || down.Why != "crashed" {
		t.Fatalf("%+v", ev[0])
	}
	if ev := r.check(); len(ev) != 0 {
		t.Fatalf("still down is not news: %v", ev)
	}
	if snap := r.w.Snapshot(); len(snap.Down) != 1 || snap.Down[0].Why != "crashed" || !snap.DockerUp || !snap.WebServerUp {
		t.Fatalf("%+v", snap)
	}
	r.containers.states["naru-blog-3"] = model.ContainerState{Name: "naru-blog-3", Running: true, State: "running"}
	ev = r.check()
	if len(ev) != 1 {
		t.Fatalf("recovery is reported right away: %v", ev)
	}
	if up, ok := ev[0].(model.ServiceUp); !ok || up.Name != "blog" {
		t.Fatalf("%+v", ev[0])
	}
	if len(r.w.Snapshot().Down) != 0 {
		t.Fatal("no longer down")
	}
}

func TestWhatWasDownAtStartIsNotAlerted(t *testing.T) {
	r := newRig()
	r.crash()
	if ev := append(r.check(), r.check()...); len(ev) != 0 {
		t.Fatalf("a restart of Naru must not flood alerts: %v", ev)
	}
	if len(r.w.Snapshot().Down) != 1 {
		t.Fatal("but it is shown as down")
	}
}

func TestRunningButNotAnsweringIsDown(t *testing.T) {
	r := newRig()
	r.check()
	r.ports.deaf["naru-blog-3"] = true
	r.check()
	ev := r.check()
	if len(ev) != 1 || ev[0].(model.ServiceDown).Why != "noanswer" {
		t.Fatalf("%v", ev)
	}
}

func TestStoppedAndDeployingServicesAreLeftAlone(t *testing.T) {
	r := newRig()
	r.check()
	r.crash()
	r.deployer.busy[1] = true
	if ev := append(r.check(), r.check()...); len(ev) != 0 {
		t.Fatalf("a deploy swaps containers — not an outage: %v", ev)
	}
	r.deployer.busy[1] = false
	r.services.all[0].Live.Stopped = true
	if ev := append(r.check(), r.check()...); len(ev) != 0 {
		t.Fatalf("stopped on purpose is not an outage: %v", ev)
	}
}

func TestOnlyContainerServicesAreWatched(t *testing.T) {
	r := newRig()
	r.ports.deaf["192.168.0.20"] = true // 외부 연결은 감시하지 않는다
	r.check()
	if ev := append(r.check(), r.check()...); len(ev) != 0 {
		t.Fatalf("static sites and external services are not watched: %v", ev)
	}
}

func TestDockerAndWebServerOutages(t *testing.T) {
	r := newRig()
	r.check()
	r.containers.err = errors.New("dial unix /var/run/docker.sock: connect: permission denied")
	r.crash() // Docker를 못 읽을 때는 서비스를 판정하지 않는다
	if ev := r.check(); len(ev) != 0 {
		t.Fatalf("%v", ev)
	}
	ev := r.check()
	if len(ev) != 1 {
		t.Fatalf("docker down once: %v", ev)
	}
	if d, ok := ev[0].(model.DockerDown); !ok || d.Why == "" {
		t.Fatalf("%+v", ev[0])
	}
	if r.w.Snapshot().DockerUp {
		t.Fatal("snapshot says so")
	}
	r.containers.err = nil
	r.containers.states["naru-blog-3"] = model.ContainerState{Name: "naru-blog-3", Running: true, State: "running"}
	if ev := r.check(); len(ev) != 1 {
		t.Fatalf("docker back: %v", ev)
	} else if _, ok := ev[0].(model.DockerUp); !ok {
		t.Fatalf("%+v", ev[0])
	}

	r.web.up = false
	r.check()
	ev = r.check()
	if len(ev) != 1 {
		t.Fatalf("web server down once: %v", ev)
	}
	if _, ok := ev[0].(model.WebServerDown); !ok {
		t.Fatalf("%+v", ev[0])
	}
	r.web.up = true
	if ev := r.check(); len(ev) != 1 {
		t.Fatalf("%v", ev)
	} else if _, ok := ev[0].(model.WebServerUp); !ok {
		t.Fatalf("%+v", ev[0])
	}
}

func TestCertificateEndingSoonOncePerDay(t *testing.T) {
	r := newRig()
	d, _ := model.ParseDomainName("blog.example.com")
	r.services.all[0].Domains = []model.Domain{{Domain: d, HTTPS: true}}
	r.certs.list = model.Certificates{{Names: []string{"blog.example.com"}, NotBefore: r.clock.now.Add(-80 * 24 * time.Hour), NotAfter: r.clock.now.Add(5 * 24 * time.Hour)}}
	ev := r.check()
	if len(ev) != 1 {
		t.Fatalf("%v", ev)
	}
	if c, ok := ev[0].(model.CertificateEndingSoon); !ok || c.Domain != "blog.example.com" {
		t.Fatalf("%+v", ev[0])
	}
	if ev := r.check(); len(ev) != 0 {
		t.Fatalf("not again the same day: %v", ev)
	}
	r.clock.now = r.clock.now.Add(24 * time.Hour)
	if ev := r.check(); len(ev) != 1 {
		t.Fatalf("again the next day: %v", ev)
	}
}

func TestUsageOfRunningContainerServices(t *testing.T) {
	r := newRig()
	r.check()
	snap := r.w.Snapshot()
	if u, ok := snap.Usage[1]; !ok || u.CPUPercent != 12.5 || u.MemUsed != 100<<20 {
		t.Fatalf("%+v", snap.Usage)
	}
	if len(snap.Usage) != 1 || len(r.usage.read) != 2 || r.usage.read[0] != "naru-blog-3" || r.usage.read[1] != "naru-caddy" {
		t.Fatalf("only running app containers are read (not static sites or external services) — and the web server for traffic: %v", r.usage.read)
	}

	r.crash()
	r.usage.read = nil
	r.check()
	if len(r.w.Snapshot().Usage) != 0 || len(r.usage.read) != 0 {
		t.Fatal("a stopped container has no usage")
	}

	r.containers.states["naru-blog-3"] = model.ContainerState{Name: "naru-blog-3", Running: true, State: "running"}
	r.usage.fails["naru-blog-3"] = true
	r.check()
	if len(r.w.Snapshot().Usage) != 0 {
		t.Fatal("a failed read leaves it out")
	}

	r.usage.fails["naru-blog-3"] = false
	r.containers.err = errors.New("docker down")
	r.check()
	if len(r.w.Snapshot().Usage) != 0 {
		t.Fatal("without Docker there is nothing to read")
	}
}

func TestUsageIsReadFourAtATime(t *testing.T) {
	r := newRig()
	r.services.all = nil
	for i := 1; i <= 9; i++ {
		c := fmt.Sprintf("naru-app%d-1", i)
		r.services.all = append(r.services.all, model.Service{ID: model.ServiceID(i), Name: name(fmt.Sprintf("app%d", i)), Kind: model.KindImage, Port: 80,
			Live: model.LiveState{Alias: fmt.Sprintf("naru-app%d", i), Instance: c}})
		r.containers.states[c] = model.ContainerState{Name: c, Running: true, State: "running"}
	}
	slow := &slowUsage{}
	r.w.p.Usage = slow
	r.check()
	if got := len(r.w.Snapshot().Usage); got != 9 {
		t.Fatalf("all nine: %d", got)
	}
	if slow.peak > 4 || slow.peak < 2 {
		t.Fatalf("read in parallel, at most four at once: peak %d", slow.peak)
	}
}

type slowUsage struct {
	mu        sync.Mutex
	now, peak int
}

func (s *slowUsage) Usage(context.Context, string) (model.ResourceUsage, error) {
	s.mu.Lock()
	s.now++
	s.peak = max(s.peak, s.now)
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	s.now--
	s.mu.Unlock()
	return model.ResourceUsage{CPUPercent: 1}, nil
}

func TestDiskRunningLowIsAlertedAsItGetsWorse(t *testing.T) {
	r := newRig()
	disk := func() []model.Finding {
		var out []model.Finding
		for _, f := range r.w.Snapshot().Findings {
			if f.Key == "find.disk" {
				out = append(out, f)
			}
		}
		return out
	}
	kinds := func(evs []model.Event) string {
		var s []string
		for _, e := range evs {
			switch x := e.(type) {
			case model.DiskLow:
				s = append(s, fmt.Sprintf("low(urgent=%v)", x.Urgent))
			case model.DiskOK:
				s = append(s, "ok")
			}
		}
		return strings.Join(s, " ")
	}
	r.check()
	if len(disk()) != 0 {
		t.Fatal("half free is fine")
	}
	r.stats.free = 15 << 30
	if got := kinds(r.check()); got != "low(urgent=false)" {
		t.Fatalf("%q", got)
	}
	if f := disk(); len(f) != 1 || f[0].Level != model.FindingWarning || f[0].Args[0] != "15.0 GB" || f[0].Args[1] != "15" || f[0].Service != 0 {
		t.Fatalf("%+v", f)
	}
	if got := kinds(r.check()); got != "" {
		t.Fatal("once per change")
	}
	r.stats.free = 8 << 30
	if got := kinds(r.check()); got != "low(urgent=true)" || disk()[0].Level != model.FindingUrgent {
		t.Fatalf("worse is alerted again: %q", got)
	}
	r.stats.free = 12 << 30
	if got := kinds(r.check()); got != "" || disk()[0].Level != model.FindingWarning {
		t.Fatalf("getting a little better is quiet: %q", got)
	}
	r.stats.free = 30 << 30
	if got := kinds(r.check()); got != "ok" || len(disk()) != 0 {
		t.Fatalf("back above 20%% is alerted once: %q", got)
	}

	// 처음 보았을 때 이미 모자랐으면 알리지 않고 보여 주기만 한다
	r2 := newRig()
	r2.stats.free = 5 << 30
	if got := kinds(r2.check()); got != "" || len(r2.w.Snapshot().Findings) == 0 {
		t.Fatalf("%q", got)
	}
}

func names(evs []model.Event) string {
	var s []string
	for _, e := range evs {
		switch x := e.(type) {
		case model.ResourceHigh:
			s = append(s, fmt.Sprintf("%s.high(%d)", x.What, x.Percent))
		case model.ResourceOK:
			s = append(s, fmt.Sprintf("%s.ok(%d)", x.What, x.Percent))
		case model.ServiceRestarted:
			s = append(s, fmt.Sprintf("restarted(%s,oom=%v,#%d)", x.Name, x.OOM, x.Count))
		}
	}
	return strings.Join(s, " ")
}

func TestHighMemoryAndCPUMustLast(t *testing.T) {
	r := newRig()
	r.check()
	r.stats.memUsed = 95
	for i := 0; i < 10; i++ { // 30초마다 — 처음 본 때부터 4분 30초까지
		if got := names(r.check()); got != "" {
			t.Fatalf("not yet five minutes: %q", got)
		}
	}
	if got := names(r.check()); got != "memory.high(95)" {
		t.Fatalf("%q", got)
	}
	r.stats.memUsed = 87
	if got := names(r.check()); got != "" {
		t.Fatal("between 85 and 90 it stays high")
	}
	r.stats.memUsed = 80
	if got := names(r.check()); got != "memory.ok(80)" {
		t.Fatalf("%q", got)
	}

	// CPU는 10분 — 중간에 한 번이라도 내려가면 처음부터
	r.stats.cpu = 99
	for i := 0; i < 15; i++ {
		r.check()
	}
	r.stats.cpu = 50
	r.check()
	r.stats.cpu = 99
	for i := 0; i < 20; i++ {
		if got := names(r.check()); got != "" {
			t.Fatalf("check %d: %q", i, got)
		}
	}
	if got := names(r.check()); got != "cpu.high(99)" {
		t.Fatalf("%q", got)
	}
	found := false
	for _, f := range r.w.Snapshot().Findings {
		found = found || (f.Key == "find.cpu" && f.Args[0] == "99")
	}
	if !found {
		t.Fatal("it is in what needs attention while high")
	}
}

func TestRestartsAreNoticedOncePerHour(t *testing.T) {
	r := newRig()
	r.containers.restarts = map[string]int{"naru-blog-3": 2} // 이미 2번 — 처음 본 것은 기준
	r.containers.oom = map[string]bool{}
	if got := names(r.check()); got != "" {
		t.Fatalf("%q", got)
	}
	r.containers.restarts["naru-blog-3"], r.containers.oom["naru-blog-3"] = 3, true
	if got := names(r.check()); got != "restarted(blog,oom=true,#3)" {
		t.Fatalf("%q", got)
	}
	var f model.Finding
	for _, x := range r.w.Snapshot().Findings {
		if x.Key == "find.oom" {
			f = x
		}
	}
	if f.Service != 1 || f.Args[1] != "3" {
		t.Fatalf("%+v", r.w.Snapshot().Findings)
	}
	r.containers.restarts["naru-blog-3"], r.containers.oom["naru-blog-3"] = 4, false
	if got := names(r.check()); got != "" {
		t.Fatal("at most one alert an hour")
	}
	for i := 0; i < 120; i++ {
		r.check()
	}
	r.containers.restarts["naru-blog-3"] = 5
	if got := names(r.check()); got != "restarted(blog,oom=false,#5)" {
		t.Fatalf("%q", got)
	}
	// 새 배포는 새로 센다
	r.services.all[0].Live.Instance = "naru-blog-4"
	r.containers.states["naru-blog-4"] = model.ContainerState{Name: "naru-blog-4", Running: true, State: "running"}
	r.containers.restarts["naru-blog-4"] = 0
	if got := names(r.check()); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestUsageIsRecordedEveryMinuteAndTidiedHourly(t *testing.T) {
	r := newRig()
	r.check() // 30초
	r.check() // 1분 — 남는다
	r.check()
	r.check()
	if len(r.metrics.added) != 2 {
		t.Fatalf("once a minute: %d", len(r.metrics.added))
	}
	p := r.metrics.added[0]
	if p[0].DiskTotal != 100<<30 || p[0].DiskUsed != 50<<30 || p[0].CPU != 10 || p[0].MemUsed != 40 {
		t.Fatalf("the server: %+v", p[0])
	}
	if _, ok := p[1]; !ok {
		t.Fatalf("the running service: %+v", p)
	}
	if !slices.Contains(r.usage.read, "naru-caddy") {
		t.Fatal("traffic is read from the web server container")
	}
	if r.metrics.compacted != 1 {
		t.Fatal("tidied on the first check")
	}
	for i := 0; i < 120; i++ {
		r.check()
	}
	if r.metrics.compacted != 2 {
		t.Fatalf("then hourly: %d", r.metrics.compacted)
	}
}
