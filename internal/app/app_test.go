package app

// 인수 테스트 — 진짜 조립(app)을 그대로 띄우고 화면으로만 다룬다. 바깥(Docker·git·DNS·Caddy)만 가짜다.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
	"github.com/KangminNa/naru/internal/netcheck"
	"github.com/KangminNa/naru/internal/store"
	"github.com/KangminNa/naru/internal/system"
)

var ctx = context.Background()

// ── 가짜 바깥 ─────────────────────────────

type fakeRegistry struct{}

func (fakeRegistry) Build(_ context.Context, folder io.Reader, tag, _ string, _ io.Writer) (model.ImageID, error) {
	io.Copy(io.Discard, folder)
	return model.ImageID("sha256:" + tag), nil
}

func (fakeRegistry) Pull(_ context.Context, ref model.ImageRef, _ io.Writer) (model.ImageDetails, error) {
	return model.ImageDetails{ID: model.ImageID("sha256:" + ref.String()), Ports: []model.Port{80}}, nil
}

type fakeImages struct{}

func (fakeImages) Exists(context.Context, model.ImageID) bool { return true }
func (fakeImages) Remove(context.Context, string) error       { return nil }

// fakeDocker는 컨테이너를 기억한다. hold를 닫기 전까지 새 컨테이너는 응답하지 않는다(배포가 멈춰 있다).
type fakeDocker struct {
	mu       sync.Mutex
	states   model.ContainerStates
	down     bool
	hold     chan struct{}
	listens  map[string]model.Port // 컨테이너마다 앱이 실제로 듣는 포트 (없으면 어느 포트든 응답)
	nets     map[string][]model.DockerNetwork
	acts     []string // 컨테이너 화면의 동작 ("restart cairn-web-1")
	madeNets []string // 네트워크 화면에서 만든 것
	joins    []string
	specs    map[string]model.ContainerSpec // Naru가 띄운 것
	details  map[string]model.ContainerDetails
}

// Inspect는 details에 적힌 컨테이너만 안다 (ContainerInspector).
func (d *fakeDocker) Inspect(_ context.Context, name string) (model.ContainerDetails, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.details[name]
	if !ok {
		return model.ContainerDetails{}, model.ErrNotFound
	}
	return v, nil
}

func (d *fakeDocker) specOf(name string) model.ContainerSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[name]
}

// NetworksOf는 nets에 적힌 컨테이너만 안다. 웹서버(naru-caddy)는 처음에 앱 네트워크에만 있다.
func (d *fakeDocker) NetworksOf(_ context.Context, name string) ([]model.DockerNetwork, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == "naru-caddy" && d.nets[name] == nil {
		return []model.DockerNetwork{{Name: "naru-net", UserDefined: true}}, nil
	}
	n, ok := d.nets[name]
	if !ok {
		return nil, model.ErrNotFound
	}
	return n, nil
}
func (d *fakeDocker) Join(_ context.Context, network, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.joins = append(d.joins, network+"<-"+name)
	return nil
}
func (d *fakeDocker) Leave(context.Context, string, string) error { return nil }
func (d *fakeDocker) joined() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.joins...)
}

func (d *fakeDocker) Start(_ context.Context, s model.ContainerSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.states[s.Name] = model.ContainerState{Name: s.Name, Running: true, State: "running", Status: "Up 1 second", Service: s.Service}
	if d.specs == nil {
		d.specs = map[string]model.ContainerSpec{}
	}
	d.specs[s.Name] = s
	return nil
}

func (d *fakeDocker) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.states, name)
	return nil
}

func (d *fakeDocker) TurnOff(_ context.Context, name string) error {
	return d.act("stop", name, "exited", "Exited (137) now")
}
func (d *fakeDocker) TurnOn(_ context.Context, name string) error {
	return d.act("start", name, "running", "Up 1 second")
}
func (d *fakeDocker) Restart(_ context.Context, name string) error {
	return d.act("restart", name, "running", "Up 1 second")
}

// act는 컨테이너 화면의 동작을 기억하고, 알던 컨테이너면 상태를 바꾼다.
func (d *fakeDocker) act(what, name, state, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.acts = append(d.acts, what+" "+name)
	if st, ok := d.states[name]; ok {
		st.State, st.Running, st.Status = state, state == "running", status
		d.states[name] = st
	}
	return nil
}

// RemoveStopped는 멈춘 것만 지운다 (ContainerSweeper).
func (d *fakeDocker) RemoveStopped(_ context.Context, name string, anon bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.states[name]
	if !ok {
		return model.ErrNotFound
	}
	if st.State == "running" {
		return model.ErrRunning
	}
	delete(d.states, name)
	if anon {
		name += " +volumes"
	}
	d.acts = append(d.acts, "remove "+name)
	return nil
}

// JoinAs는 별칭과 함께 기억한다.
func (d *fakeDocker) JoinAs(_ context.Context, network, name string, aliases []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.states[name]; !ok {
		return model.ErrNotFound
	}
	d.joins = append(d.joins, network+"<-"+name+"@"+strings.Join(aliases, ","))
	return nil
}

// Networks는 앱 네트워크, compose 묶음 하나, 안 쓰는 것 하나, 기본 bridge다 (NetworkStore).
func (d *fakeDocker) Networks(context.Context) ([]model.NetworkRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows := []model.NetworkRow{{Name: "bridge", Driver: "bridge"}, {Name: "naru-net", Driver: "bridge", Containers: []string{"naru-caddy"}},
		{Name: "cairn_default", Driver: "bridge", Project: "cairn", Containers: []string{"cairn-web-1"}}}
	for _, n := range d.madeNets {
		rows = append(rows, model.NetworkRow{Name: n, Driver: "bridge"})
	}
	return rows, nil
}

func (d *fakeDocker) CreateNetwork(_ context.Context, name model.NetworkName, internal bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range d.madeNets {
		if n == name.String() {
			return model.ErrInUse
		}
	}
	d.madeNets = append(d.madeNets, name.String())
	return nil
}

func (d *fakeDocker) RemoveNetwork(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == "cairn_default" {
		return model.ErrInUse
	}
	for i, n := range d.madeNets {
		if n == name {
			d.madeNets = append(d.madeNets[:i], d.madeNets[i+1:]...)
			return nil
		}
	}
	return model.ErrNotFound
}

// DiskUsage는 이미지 둘(하나는 안 씀)과 볼륨 둘(하나는 씀)이다.
func (d *fakeDocker) DiskUsage(context.Context) (model.DockerDisk, error) {
	return model.DockerDisk{
		Images: []model.DiskImage{{ID: "sha256:0123456789abcdef", Tags: []string{"old/app:1"}, Size: 890 << 20, UsedBy: 0},
			{ID: "sha256:fedcba9876543210", Tags: []string{"shop-web:latest"}, Size: 200 << 20, UsedBy: 1}},
		Volumes: []model.DiskVolume{{Name: "shop_dbdata", Size: 2 << 30, UsedBy: 1, Project: "shop"}, {Name: "left_over", Size: 300 << 20}},
	}, nil
}

func (d *fakeDocker) RemoveVolume(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == "shop_dbdata" {
		return model.ErrInUse
	}
	d.acts = append(d.acts, "remove-volume "+name)
	return nil
}

func (d *fakeDocker) All(context.Context) (model.ContainerStates, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return nil, io.ErrUnexpectedEOF
	}
	out := model.ContainerStates{}
	for k, v := range d.states {
		out[k] = v
	}
	return out, nil
}

func (d *fakeDocker) One(_ context.Context, name string) (model.ContainerState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.states[name], nil
}

func (d *fakeDocker) Logs(context.Context, string, int) (string, error) { return "", nil }

// Recent는 앱 출력인 척한다.
func (d *fakeDocker) Recent(_ context.Context, name string, n int) ([]model.LogLine, error) {
	return []model.LogLine{
		{At: time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC), Source: model.LogApp, Stream: "stdout", Text: name + " listening on :80"},
		{At: time.Date(2026, 10, 10, 12, 0, 3, 0, time.UTC), Source: model.LogApp, Stream: "stderr", Text: "warn: <script>alert(1)</script>"},
	}, nil
}
func (d *fakeDocker) BelongingTo(context.Context, model.ServiceID) ([]string, error) { return nil, nil }

func (d *fakeDocker) Answers(_ context.Context, host string, port model.Port) error {
	d.mu.Lock()
	hold := d.hold
	p, picky := d.listens[host]
	d.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if picky && p != port {
		return errors.New("connection refused")
	}
	return nil
}

// Usage는 실행 중인 컨테이너의 사용량인 척한다.
func (d *fakeDocker) Usage(_ context.Context, name string) (model.ResourceUsage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.states[name]; !ok || st.State != "running" {
		return model.ResourceUsage{}, errors.New("no such container")
	}
	return model.ResourceUsage{CPUPercent: 3.25, MemUsed: 150 << 20, MemLimit: 2 << 30}, nil
}

func (d *fakeDocker) listen(host string, p model.Port) {
	d.mu.Lock()
	if d.listens == nil {
		d.listens = map[string]model.Port{}
	}
	d.listens[host] = p
	d.mu.Unlock()
}

func (d *fakeDocker) set(name string, st model.ContainerState) {
	d.mu.Lock()
	d.states[name] = st
	d.mu.Unlock()
}

type fakeGit struct{}

func (fakeGit) Download(_ context.Context, _ model.CodeSource, into string, _ io.Writer) (model.Commit, error) {
	os.MkdirAll(into, 0o755)
	os.WriteFile(filepath.Join(into, "Dockerfile"), []byte("FROM x\nEXPOSE 3000\n"), 0o644)
	os.WriteFile(filepath.Join(into, "index.html"), []byte("<h1>hi</h1>"), 0o644)
	return model.Commit{Hash: "a1b2c3d4e5f6", Message: "fix: things"}, nil
}

// fakeCerts는 웹서버 인증서 저장소인 척한다.
type fakeCerts struct {
	mu   sync.Mutex
	list model.Certificates
}

func (c *fakeCerts) Read(context.Context) (model.Certificates, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(model.Certificates{}, c.list...), nil
}

func (c *fakeCerts) issue(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, model.Certificate{Names: names, Issuer: "Let's Encrypt",
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)})
}

// fakeSnippet은 Caddy의 /adapt인 척한다 — "nonsense"는 모르는 지시어.
type fakeSnippet struct{}

func (fakeSnippet) Compile(_ context.Context, text string) ([]byte, []string, error) {
	if strings.Contains(text, "nonsense") {
		return nil, nil, errors.New("Caddyfile:2: unrecognized directive: nonsense")
	}
	var ignored []string
	if strings.Contains(text, "tls") {
		ignored = []string{"tls — 이 설정은 Naru가 정해요 / Naru manages tls"}
	}
	return []byte(`[{"match":[{"path":["/health"]}],"handle":[{"handler":"static_response","status_code":200}]}]`), ignored, nil
}

type fakeStats struct{}

func (fakeStats) Now() model.ServerSnapshot { return model.ServerSnapshot{Cores: 2} }

// fakeCaddy는 받은 설정을 기록한다.
type fakeCaddy struct {
	mu       sync.Mutex
	loads    []string
	fail     error
	refuseIf string // 설정에 이 글자가 있으면 거절한다 (웹서버가 틀린 설정을 거절하는 것처럼)
}

func (c *fakeCaddy) Send(_ context.Context, cfg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	if c.refuseIf != "" && strings.Contains(string(cfg), c.refuseIf) {
		return errors.New(`caddy: POST /load: 400 {"error":"loading new config: bad header"}`)
	}
	c.loads = append(c.loads, string(cfg))
	return nil
}

func (c *fakeCaddy) Ping(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fail
}

func (c *fakeCaddy) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.loads) == 0 {
		return ""
	}
	return c.loads[len(c.loads)-1]
}

// ── 하네스 ─────────────────────────────────

type harness struct {
	t      *testing.T
	app    *App
	srv    *httptest.Server
	client *http.Client
	docker *fakeDocker
	caddy  *fakeCaddy
	certs  *fakeCerts
}

// newHarness는 Naru를 통째로 띄운다. DNS는 naru.example.com → 198.51.100.24 로 고정한다.
func newHarness(t *testing.T, envDomain string) *harness {
	return newHarnessWith(t, harnessOpts{envDomain: envDomain})
}

type harnessOpts struct {
	envDomain  string
	compose    bool          // NARU_HOST_DATA를 정하고 가짜 docker compose를 붙인다
	watchEvery time.Duration // 0이 아니면 지켜보기 고리를 이 간격으로 돌린다
}

func newHarnessWith(t *testing.T, o harnessOpts) *harness {
	envDomain := o.envDomain
	t.Helper()
	lookup := func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "naru.example.com":
			return []string{"198.51.100.24"}, nil
		case "elsewhere.example.com":
			return []string{"203.0.113.9"}, nil
		}
		return nil, &url.Error{Op: "lookup", URL: host}
	}
	h := &harness{t: t, docker: &fakeDocker{states: model.ContainerStates{}}, caddy: &fakeCaddy{}, certs: &fakeCerts{}}
	cfg := Config{DataDir: t.TempDir(), Network: "naru-net", AdminDomain: envDomain, Version: "test"}
	var compose contract.ComposeRunner
	if o.compose {
		cfg.HostData = t.TempDir()
		compose = &fakeCompose{docker: h.docker}
	}
	a, err := OpenWith(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Outside{
		Compose: compose, Sweeper: h.docker, Disk: h.docker, Volumes: h.docker, NetStore: h.docker,
		Builder: fakeRegistry{}, Puller: fakeRegistry{}, Images: fakeImages{},
		Starter: h.docker, Remover: h.docker, Switch: h.docker, Watcher: h.docker, Ports: h.docker, AppLogs: h.docker, Usage: h.docker, Networks: h.docker, Inspect: h.docker,
		Code: fakeGit{}, DNS: netcheck.NewDNS(lookup), Stats: fakeStats{}, Sender: h.caddy, Certs: h.certs, Snippet: fakeSnippet{},
		Clock: system.Clock{}, Random: system.Random{}, ReadyTimeout: 5 * time.Second, WatchEvery: o.watchEvery,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.app = a
	runCtx, stop := context.WithCancel(ctx)
	go a.syncWS(runCtx)
	go a.alerts(runCtx)
	if o.watchEvery > 0 {
		go a.watch(runCtx)
	}
	h.srv = httptest.NewServer(a.handler)
	t.Cleanup(func() {
		h.srv.Close()
		stop()
		h.release()
		a.Close()
	})
	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

func (h *harness) get(path string) (int, string, string) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
	req.Header.Set("Accept-Language", "ko-KR")
	return h.do(req)
}

func (h *harness) post(path string, form url.Values) (int, string, string) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "ko-KR")
	return h.do(req)
}

func (h *harness) do(req *http.Request) (int, string, string) {
	h.t.Helper()
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Location"), string(body)
}

func (h *harness) settings() store.Settings   { return store.NewSettings(h.app.db) }
func (h *harness) services() store.Services   { return store.NewServices(h.app.db) }
func (h *harness) history() store.Deployments { return store.NewDeployments(h.app.db) }
func (h *harness) secretsOf() store.Secrets   { return store.NewSecrets(h.app.db) }
func (h *harness) setting(key string) string  { v, _ := h.settings().Get(ctx, key); return v }
func (h *harness) service(id int64) model.Service {
	s, _ := h.services().Get(ctx, model.ServiceID(id))
	return s
}

// hold는 새 컨테이너가 응답하지 않게 붙잡는다 — 배포가 진행 중인 채로 남는다.
func (h *harness) hold() {
	h.docker.mu.Lock()
	h.docker.hold = make(chan struct{})
	h.docker.mu.Unlock()
}

func (h *harness) release() {
	h.docker.mu.Lock()
	if h.docker.hold != nil {
		close(h.docker.hold)
		h.docker.hold = nil
	}
	h.docker.mu.Unlock()
}

// waitFor는 웹서버에 cond를 만족하는 설정이 닿기를 기다린다.
func (h *harness) waitFor(what string, cond func(cfg string) bool) {
	h.t.Helper()
	for i := 0; i < 300; i++ {
		if cond(h.caddy.last()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("the web server never got a config where %s:\n%s", what, h.caddy.last())
}

func (h *harness) createService(s model.NewService, sec model.ServiceSecrets) model.ServiceID {
	h.t.Helper()
	id, err := h.services().Create(ctx, s)
	if err != nil {
		h.t.Fatal(err)
	}
	h.secretsOf().Set(ctx, id, sec)
	return id
}

func (h *harness) addDomain(id model.ServiceID, d string, https, hsts bool) {
	h.t.Helper()
	dn, err := model.ParseDomainName(d)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := store.NewDomains(h.app.db).Add(ctx, id, model.DomainInput{Domain: dn, HTTPS: https}); err != nil {
		h.t.Fatal(err)
	}
	if hsts { // 화면에는 HSTS를 켜는 칸이 아직 없다 (v1에서 옮겨 온 주소만 켜져 있다)
		raw, err := sql.Open("sqlite", filepath.Join(h.app.cfg.DataDir, "naru.db"))
		if err != nil {
			h.t.Fatal(err)
		}
		defer raw.Close()
		if _, err := raw.Exec(`UPDATE domains SET hsts = 1 WHERE domain = ?`, dn.String()); err != nil {
			h.t.Fatal(err)
		}
	}
}

func name(s string) model.ServiceName {
	n, err := model.ParseServiceName(s)
	if err != nil {
		panic(err)
	}
	return n
}

// createAccount는 마법사를 계정 단계까지 지난다.
func (h *harness) createAccount() {
	h.t.Helper()
	code, loc, _ := h.post("/setup", url.Values{"token": {h.app.key.Value()}, "username": {"admin"}, "password": {"longenough"}, "password2": {"longenough"}})
	if code != http.StatusSeeOther || loc != "/setup/domain" {
		h.t.Fatalf("account step: %d → %q", code, loc)
	}
}

// signedIn은 설정을 마치고 로그인한 상태로 만든다.
func (h *harness) signedIn() {
	h.t.Helper()
	h.createAccount()
	h.settings().Set(ctx, model.SettingSetupDone, "1")
	h.settings().Set(ctx, model.SettingAdminDomain, "naru.example.com")
}

// ── 첫 설정 · 로그인 · 설정 ─────────────────────

func TestFreshInstallSendsEveryoneToSetup(t *testing.T) {
	h := newHarness(t, "")
	for _, p := range []string{"/", "/settings", "/login"} {
		if code, loc, _ := h.get(p); code != http.StatusSeeOther || loc != "/setup" {
			t.Errorf("%s: %d → %q, want /setup", p, code, loc)
		}
	}
	code, _, body := h.get("/setup")
	if code != http.StatusOK || !strings.Contains(body, "docker compose logs naru") || strings.Contains(body, `name="password"`) {
		t.Fatalf("without a token the setup page explains where the link is and shows no form")
	}
	if code, _, _ := h.get("/setup?token=wrong"); code != http.StatusForbidden {
		t.Fatalf("a wrong token is forbidden, got %d", code)
	}
	if code, _, body := h.get("/setup?token=" + h.app.key.Value()); code != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Fatal("the right token shows the account form")
	}
	h.waitFor("unknown hosts reach the admin screen before setup", func(cfg string) bool {
		return strings.Contains(cfg, "naru:8080") && !strings.Contains(cfg, `"status_code": 404`)
	})
}

func TestSetupRefusesWithoutTheToken(t *testing.T) {
	h := newHarness(t, "")
	code, _, _ := h.post("/setup", url.Values{"token": {"guess"}, "username": {"intruder"}, "password": {"longenough"}, "password2": {"longenough"}})
	if names, _ := h.app.Accounts.Names(ctx); code != http.StatusForbidden || len(names) != 0 {
		t.Fatalf("an account must not be created without the token (status %d)", code)
	}
	if code, _, _ := h.post("/setup", url.Values{"token": {"guess"}, "username": {"x"}, "password": {"longenough"}, "password2": {"longenough"}}); code != http.StatusForbidden {
		t.Fatal("the token is checked before anything else the form says")
	}
}

func TestWizardFromAccountToHome(t *testing.T) {
	h := newHarness(t, "")
	h.createAccount()

	if code, loc, _ := h.get("/"); code != http.StatusSeeOther || loc != "/setup/domain" {
		t.Fatalf("an unfinished wizard returns to the domain step: %d %q", code, loc)
	}

	// 다른 곳을 가리키는 도메인은 경고와 함께 머문다
	code, _, body := h.post("/setup/domain", url.Values{"domain": {"elsewhere.example.com"}, "action": {"check"}})
	if code != http.StatusOK || !strings.Contains(body, "203.0.113.9") {
		t.Fatalf("a domain pointing elsewhere is shown, not saved: %d", code)
	}
	if h.setting(model.SettingAdminDomain) != "" {
		t.Fatal("not saved before confirmation")
	}

	// 우리를 가리키는 도메인 — 사용자가 IP로 들어왔을 때 그 IP와 견준다
	req, _ := http.NewRequest("POST", h.srv.URL+"/setup/domain", strings.NewReader(url.Values{"domain": {"https://NARU.example.com/"}, "action": {"check"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "198.51.100.24"
	for _, c := range h.client.Jar.Cookies(req.URL) { // Host를 바꾸면 쿠키 저장소가 다른 호스트로 본다
		req.AddCookie(c)
	}
	if code, loc, _ := h.do(req); code != http.StatusSeeOther || loc != "/setup/https" {
		t.Fatalf("a matching domain moves on: %d %q", code, loc)
	}
	if v := h.setting(model.SettingAdminDomain); v != "naru.example.com" {
		t.Fatalf("the domain is saved normalized, got %q", v)
	}

	if code, _, _ := h.post("/setup/https", url.Values{"email": {"not-an-email"}, "action": {"save"}}); code != http.StatusBadRequest {
		t.Fatal("a malformed email is refused")
	}
	// 테스트 서버는 127.0.0.1로 들어와 있으니 관리 주소로 가라고 안내한다
	if code, _, body := h.post("/setup/https", url.Values{"email": {"me@example.com"}, "action": {"save"}}); code != http.StatusOK || !strings.Contains(body, "http://naru.example.com/login") {
		t.Fatalf("finishing points to the admin address: %d", code)
	}
	if h.setting(model.SettingACMEEmail) != "me@example.com" {
		t.Fatal("the email is saved")
	}
	code, _, body = h.get("/?ok=setup")
	if code != http.StatusOK || !strings.Contains(body, "설정을 마쳤어요") || strings.Contains(body, "관리 화면 주소가 아직 없어요") {
		t.Fatalf("home after setup: %d", code)
	}
	if code, loc, _ := h.get("/setup"); code != http.StatusSeeOther || loc != "/" {
		t.Fatal("setup is closed once done")
	}
	h.waitFor("the admin address has HTTPS and unknown hosts get 404", func(cfg string) bool {
		return strings.Contains(cfg, "naru.example.com") && strings.Contains(cfg, "me@example.com") && strings.Contains(cfg, `"status_code": 404`)
	})
}

func TestSkippingTheDomainShowsANoticeAtHome(t *testing.T) {
	h := newHarness(t, "")
	h.createAccount()
	h.post("/setup/domain", url.Values{"action": {"skip"}})
	h.post("/setup/https", url.Values{"action": {"skip"}})
	_, _, body := h.get("/")
	if !strings.Contains(body, "관리 화면 주소가 아직 없어요") {
		t.Fatal("without an admin address, home says so")
	}
}

// 입력 칸에서 Enter를 누르면 브라우저는 DOM에서 첫 번째 제출 버튼을 누른다.
// 그게 "건너뛰기"면 사용자가 적은 값이 버려진다.
func TestEnterSubmitsThePrimaryAction(t *testing.T) {
	h := newHarness(t, "")
	h.createAccount()
	firstSubmit := regexp.MustCompile(`<button[^>]*type="submit"[^>]*value="(\w+)"`)
	_, _, body := h.get("/setup/domain")
	if m := firstSubmit.FindStringSubmatch(body); m == nil || m[1] != "check" {
		t.Fatalf("domain step: first submit button is %v", m)
	}
	_, _, body = h.get("/setup/https")
	if m := firstSubmit.FindStringSubmatch(body); m == nil || m[1] != "save" {
		t.Fatalf("https step: first submit button is %v", m)
	}
}

func TestEnvironmentDomainWins(t *testing.T) {
	h := newHarness(t, "env.example.com")
	h.createAccount()
	_, _, body := h.get("/setup/domain")
	if !strings.Contains(body, "env.example.com") || strings.Contains(body, `name="domain"`) {
		t.Fatal("an environment domain is shown fixed, without an input")
	}
}

func TestLoginLogoutAndLockout(t *testing.T) {
	h := newHarness(t, "")
	h.createAccount()
	h.settings().Set(ctx, model.SettingSetupDone, "1")
	h.post("/logout", nil)
	if code, loc, _ := h.get("/"); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("signed out → login, got %d %q", code, loc)
	}
	if code, _, _ := h.post("/login", url.Values{"username": {"admin"}, "password": {"wrong"}}); code != http.StatusUnauthorized {
		t.Fatal("a wrong password is 401")
	}
	for i := 0; i < 5; i++ {
		h.post("/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	}
	if code, _, body := h.post("/login", url.Values{"username": {"admin"}, "password": {"longenough"}}); code != http.StatusTooManyRequests || !strings.Contains(body, "15분") {
		t.Fatalf("locked out, got %d", code)
	}
}

func TestChangePasswordFromSettings(t *testing.T) {
	h := newHarness(t, "")
	h.createAccount()
	h.settings().Set(ctx, model.SettingSetupDone, "1")

	if code, _, _ := h.post("/settings/password", url.Values{"current": {"longenough"}, "next": {"newpassword1"}, "next2": {"different1"}}); code != http.StatusBadRequest {
		t.Fatal("mismatched confirmation is refused")
	}
	if code, _, body := h.post("/settings/password", url.Values{"current": {"nope"}, "next": {"newpassword1"}, "next2": {"newpassword1"}}); code != http.StatusBadRequest || !strings.Contains(body, "지금 비밀번호가 맞지 않아요") {
		t.Fatal("a wrong current password is refused with its own message")
	}
	if code, loc, _ := h.post("/settings/password", url.Values{"current": {"longenough"}, "next": {"newpassword1"}, "next2": {"newpassword1"}}); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/settings?ok=password") {
		t.Fatalf("changed: %d %q", code, loc)
	}
	if code, _, _ := h.get("/settings"); code != http.StatusOK {
		t.Fatal("this device stays signed in")
	}
	h.post("/logout", nil)
	if code, loc, _ := h.post("/login", url.Values{"username": {"admin"}, "password": {"newpassword1"}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatal("the new password works")
	}
}

func TestSettingsChangeTheAdminAddress(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	code, _, body := h.post("/settings/domain", url.Values{"domain": {"elsewhere.example.com"}, "email": {"ops@example.com"}, "action": {"check"}})
	if code != http.StatusOK || !strings.Contains(body, "203.0.113.9") {
		t.Fatalf("a domain pointing elsewhere is shown first: %d", code)
	}
	if code, loc, _ := h.post("/settings/domain", url.Values{"domain": {"elsewhere.example.com"}, "email": {"ops@example.com"}, "action": {"save"}}); code != http.StatusSeeOther || !strings.Contains(loc, "ok=domain") {
		t.Fatalf("saved anyway: %d %q", code, loc)
	}
	if h.setting(model.SettingAdminDomain) != "elsewhere.example.com" || h.setting(model.SettingACMEEmail) != "ops@example.com" {
		t.Fatal("both are saved")
	}
	if code, _, _ := h.post("/settings/domain", url.Values{"domain": {"bad domain"}}); code != http.StatusBadRequest {
		t.Fatal("a malformed domain is refused")
	}
	h.waitFor("the new admin address is served", func(cfg string) bool { return strings.Contains(cfg, "elsewhere.example.com") })
}

func TestCrossSiteFormsAreRefused(t *testing.T) {
	h := newHarness(t, "")
	req, _ := http.NewRequest("POST", h.srv.URL+"/login", strings.NewReader("username=admin&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if code, _, _ := h.do(req); code != http.StatusForbidden {
		t.Fatalf("a foreign Origin is refused, got %d", code)
	}
}

func TestLanguageToggleAndSafeRedirect(t *testing.T) {
	h := newHarness(t, "")
	if _, loc, _ := h.get("/lang/en?back=/settings"); loc != "/settings" {
		t.Fatalf("back to the same page, got %q", loc)
	}
	_, _, body := h.get("/setup")
	if !strings.Contains(body, "You need the setup link") {
		t.Fatal("the language cookie switches the screen to English")
	}
	for _, evil := range []string{"https://evil.example", "//evil.example", "/\\evil.example"} {
		if _, loc, _ := h.get("/lang/ko?back=" + url.QueryEscape(evil)); loc != "/" {
			t.Errorf("back=%q must not leave the site, got %q", evil, loc)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, "")
	res, err := h.client.Get(h.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, k := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if res.Header.Get(k) == "" {
			t.Errorf("missing %s", k)
		}
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	h := newHarness(t, "")
	if code, _, _ := h.get("/nope"); code != http.StatusNotFound {
		t.Fatalf("got %d", code)
	}
}

// ── 홈 · 서비스 ────────────────────────────

func TestHomeListsServicesWithTheirState(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	blog := h.createService(model.NewService{Name: name("blog"), Kind: model.KindRepo, Alias: "shelf-blog", Port: 3000}, model.ServiceSecrets{})
	h.addDomain(blog, "blog.example.com", true, false)
	h.createService(model.NewService{Name: name("api"), Kind: model.KindImage, Alias: "naru-api", Port: 8080}, model.ServiceSecrets{})
	h.createService(model.NewService{Name: name("nas"), Kind: model.KindExternal, External: "host.docker.internal:5000"}, model.ServiceSecrets{})
	h.docker.set("shelf-blog", model.ContainerState{Name: "shelf-blog", Running: true, State: "running", Status: "Up 3 hours"})

	_, _, body := h.get("/")
	for _, want := range []string{"blog.example.com", "실행 중", "Up 3 hours", "컨테이너 없음", "주소만 연결", "/services/1", "3개를 이 서버에서"} {
		if !strings.Contains(body, want) {
			t.Errorf("home is missing %q", want)
		}
	}
	if strings.Contains(body, "Docker에 연결하지 못했어요") || strings.Contains(body, "웹서버에 연결하지 못했어요") {
		t.Error("no trouble notices while everything is reachable")
	}
}

func TestHomeSaysWhenDockerOrTheWebServerIsDown(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.createService(model.NewService{Name: name("blog"), Kind: model.KindRepo, Alias: "shelf-blog", Port: 3000}, model.ServiceSecrets{})
	h.docker.down = true
	h.caddy.mu.Lock()
	h.caddy.fail = errors.New("caddy: dial unix /run/caddy/admin.sock: connect: no such file")
	h.caddy.mu.Unlock()
	h.settings().Set(ctx, model.SettingACMEEmail, "x@example.com") // 설정을 바꿔 다시 보내게 한다
	h.post("/settings/domain", url.Values{"domain": {"naru.example.com"}, "email": {"y@example.com"}, "action": {"save"}})
	for i := 0; i < 300; i++ {
		if _, _, body := h.get("/"); strings.Contains(body, "admin.sock") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, body := h.get("/")
	for _, want := range []string{"Docker에 연결하지 못했어요", "웹서버에 연결하지 못했어요", "admin.sock", "확인 불가"} {
		if !strings.Contains(body, want) {
			t.Errorf("home is missing %q", want)
		}
	}
}

func TestServicePage(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	id := h.createService(model.NewService{Name: name("landing"), Kind: model.KindRepo, Source: "https://github.com/me/landing", Branch: "main", Alias: "shelf-landing", Port: 4023, BuildPath: "site"},
		model.ServiceSecrets{GitToken: "ghp_secret", WebhookSecret: "whsec_secret"})
	h.addDomain(id, "www.example.com", true, true)
	h.docker.set("shelf-landing", model.ContainerState{State: "restarting", Status: "Restarting (1) 3 seconds ago"})

	code, _, body := h.get("/services/1")
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	for _, want := range []string{"landing", "shelf-landing:4023", "www.example.com", "HSTS", "계속 다시 시작됨", "https://github.com/me/landing", "site", "https://naru.example.com/hooks/1", "whsec_secret"} {
		if !strings.Contains(body, want) {
			t.Errorf("service page is missing %q", want)
		}
	}
	if strings.Contains(body, "ghp_secret") {
		t.Fatal("the git token never reaches the screen")
	}
	for _, p := range []string{"/services/99", "/services/abc"} {
		if code, _, _ := h.get(p); code != http.StatusNotFound {
			t.Errorf("%s: got %d", p, code)
		}
	}
}

func TestCreatingAServiceStartsItsFirstDeploy(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	code, loc, _ := h.post("/services/new", url.Values{"kind": {"image"}, "source": {"traefik/whoami"}, "domain": {"Who.Example.com"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/services/1/deploys/") {
		t.Fatalf("%d %q", code, loc)
	}
	s := h.service(1)
	if s.Name.String() != "who" || s.Kind != model.KindImage || s.PrimaryDomain() != "who.example.com" || !s.AutoDeploy {
		t.Fatalf("the name comes from the domain: %+v", s)
	}
	if deps, _ := h.history().Recent(ctx, 1, 10); len(deps) != 1 || deps[0].Reason != model.ReasonCreate {
		t.Fatalf("the first deploy starts: %+v", deps)
	}
	if sec, _ := h.secretsOf().Get(ctx, 1); len(sec.WebhookSecret) < 32 {
		t.Fatal("every service gets its own webhook secret")
	}
	h.waitFor("the new service is routed to its container", func(cfg string) bool {
		return strings.Contains(cfg, "who.example.com") && strings.Contains(cfg, "naru-who:80")
	})

	// 외부 연결은 배포가 없다
	code, loc, _ = h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"localhost:5000"}, "domain": {"nas.example.com"}})
	if code != http.StatusSeeOther || loc != "/services/2" {
		t.Fatalf("%d %q", code, loc)
	}
	if nas := h.service(2); nas.External != "host.docker.internal:5000" || nas.Name.String() != "nas" {
		t.Fatalf("localhost means this server: %+v", nas)
	}
	if deps, _ := h.history().Recent(ctx, 2, 10); len(deps) != 0 {
		t.Fatal("nothing to deploy")
	}
}

func TestStaticSiteFromTheScreen(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	code, loc, _ := h.post("/services/new", url.Values{"kind": {"static"}, "source": {"https://github.com/me/site"}, "domain": {"www.example.com"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/services/1/deploys/") {
		t.Fatalf("%d %q", code, loc)
	}
	h.waitFor("the site is served from its folder", func(cfg string) bool { return strings.Contains(cfg, "/srv/sites/www/") })
	if _, err := os.Stat(filepath.Join(h.app.cfg.DataDir, "sites", "www")); err != nil {
		t.Fatal("files are published under data/sites")
	}
}

func TestCreateServiceValidation(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	taken := h.createService(model.NewService{Name: name("taken"), Kind: model.KindExternal, External: "x:1"}, model.ServiceSecrets{})
	h.addDomain(taken, "used.example.com", false, false)
	cases := []struct {
		form url.Values
		want string
	}{
		{url.Values{"kind": {"repo"}, "source": {"file:///etc"}}, "https://"},
		{url.Values{"kind": {"repo"}, "source": {"https://github.com/me/x"}, "branch": {"--upload-pack=x"}}, "브랜치"},
		{url.Values{"kind": {"repo"}, "source": {"https://github.com/me/x"}, "build_path": {"../../etc"}}, ".."},
		{url.Values{"kind": {"image"}, "source": {"bad image"}}, "이미지 이름"},
		{url.Values{"kind": {"external"}, "upstream": {"nope"}}, "주소:포트"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "domain": {"used.example.com"}}, "이미 쓰는"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "domain": {"naru.example.com"}}, "관리 화면"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "name": {"taken"}}, "이미 있는 이름"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "name": {"Bad_Name"}}, "영문 소문자"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "env": {"not an env line"}}, "KEY=value"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "volumes": {"relative:/data"}}, "서버경로"},
		{url.Values{"kind": {"image"}, "source": {"a/b"}, "port": {"99999"}}, "1~65535"},
		{url.Values{"kind": {"nonsense"}}, "종류"},
	}
	for _, c := range cases {
		code, _, body := h.post("/services/new", c.form)
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%v: %d, want message containing %q", c.form, code, c.want)
		}
	}
	if all, _ := h.services().List(ctx); len(all) != 1 {
		t.Fatal("nothing is created after a refused form")
	}
}

func TestSettingsDomainsAndDelete(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	env, _, _ := model.ParseEnvVars("A=1")
	id := h.createService(model.NewService{Name: name("api"), Kind: model.KindImage, Source: "me/api", Alias: "naru-api", Port: 3000}, model.ServiceSecrets{Env: env})

	code, loc, _ := h.post(fmt.Sprintf("/services/%d/settings", id), url.Values{"source": {"me/api"}, "port": {"8080"}, "env": {"A=1"}, "auto_deploy": {"1"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "ok=saved") {
		t.Fatalf("%d %q", code, loc)
	}
	if s := h.service(int64(id)); s.Port != 8080 {
		t.Fatal("port saved")
	}
	if _, loc, _ := h.post(fmt.Sprintf("/services/%d/settings", id), url.Values{"source": {"me/api"}, "port": {"8080"}, "env": {"A=2"}}); !strings.Contains(loc, "ok=redeploy") {
		t.Fatal("env changes say a redeploy is needed")
	}
	if code, _, body := h.post(fmt.Sprintf("/services/%d/settings", id), url.Values{"source": {"me/api"}, "env": {"oops"}}); code != http.StatusBadRequest || !strings.Contains(body, "KEY=value") || !strings.Contains(body, "oops") {
		t.Fatal("a bad settings form is shown again with what was typed")
	}

	h.post(fmt.Sprintf("/services/%d/domains", id), url.Values{"domain": {"api.example.com"}, "https": {"1"}})
	if s := h.service(int64(id)); s.PrimaryDomain() != "api.example.com" {
		t.Fatal("domain added")
	}
	h.waitFor("a port change reaches the web server", func(cfg string) bool {
		return strings.Contains(cfg, "api.example.com") && strings.Contains(cfg, "naru-api:8080")
	})
	if code, _, _ := h.post(fmt.Sprintf("/services/%d/domains", id), url.Values{"domain": {"naru.example.com"}}); code != http.StatusBadRequest {
		t.Fatal("the admin address cannot be taken")
	}
	s := h.service(int64(id))
	h.post(fmt.Sprintf("/services/%d/domains/%d/delete", id, s.Domains[0].ID), nil)
	if s := h.service(int64(id)); len(s.Domains) != 0 {
		t.Fatal("domain removed")
	}

	if code, _, _ := h.post(fmt.Sprintf("/services/%d/delete", id), url.Values{"confirm": {"nope"}}); code != http.StatusBadRequest {
		t.Fatal("deleting needs the name")
	}
	if _, loc, _ := h.post(fmt.Sprintf("/services/%d/delete", id), url.Values{"confirm": {"api"}}); loc != "/?ok=deleted" {
		t.Fatalf("deleted → home, got %q", loc)
	}
	if _, err := h.services().Get(ctx, id); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("gone")
	}
}

func TestDeployPageShowsStepsAndRefreshesWhileRunning(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	id := h.createService(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, model.ServiceSecrets{})
	did, _ := h.history().Start(ctx, id, model.ReasonManual)
	h.history().SaveLog(ctx, did, "\n▶ 이미지 받기 / pulling the image\nok\n▶ 새 컨테이너 시작 / starting the new container\n")

	_, _, body := h.get(fmt.Sprintf("/services/%d/deploys/%d", id, did))
	for _, want := range []string{"이미지 받기", "새 컨테이너 시작", `http-equiv="refresh"`, "진행 중", `class="doing"`} {
		if !strings.Contains(body, want) {
			t.Errorf("deploy page is missing %q", want)
		}
	}
	h.history().Finish(ctx, model.Deployment{ID: did, Status: model.DeploySuccess, Log: "▶ 이미지 받기 / pulling\n"})
	_, _, body = h.get(fmt.Sprintf("/services/%d/deploys/%d", id, did))
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("a finished deploy stops refreshing")
	}
	if code, _, _ := h.get(fmt.Sprintf("/services/%d/deploys/999", id)); code != http.StatusNotFound {
		t.Fatal("unknown deploy → 404")
	}
}

func TestDeployStopStartAndRollBackFromTheScreen(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	id := h.createService(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, model.ServiceSecrets{})
	_, loc, _ := h.post(fmt.Sprintf("/services/%d/deploy", id), nil)
	if !strings.HasPrefix(loc, fmt.Sprintf("/services/%d/deploys/", id)) {
		t.Fatalf("deploy → its page, got %q", loc)
	}
	h.app.wait()
	first, _ := h.history().Recent(ctx, id, 1)
	if first[0].Status != model.DeploySuccess {
		t.Fatalf("%+v", first[0])
	}
	h.post(fmt.Sprintf("/services/%d/deploy", id), nil)
	h.app.wait()

	if _, loc, _ := h.post(fmt.Sprintf("/services/%d/stop", id), nil); !strings.Contains(loc, "ok=stopped") || !h.service(int64(id)).Live.Stopped {
		t.Fatalf("stopped: %q", loc)
	}
	if _, loc, _ := h.post(fmt.Sprintf("/services/%d/start", id), nil); !strings.Contains(loc, "ok=started") || h.service(int64(id)).Live.Stopped {
		t.Fatalf("started: %q", loc)
	}
	_, loc, _ = h.post(fmt.Sprintf("/services/%d/rollback/%d", id, first[0].ID), nil)
	h.app.wait()
	if !strings.Contains(loc, "/deploys/") {
		t.Fatalf("rollback → its page, got %q", loc)
	}
	if latest, _ := h.history().Recent(ctx, id, 1); latest[0].Reason != model.ReasonRollBack || latest[0].Status != model.DeploySuccess {
		t.Fatalf("%+v", latest[0])
	}
	if _, loc, _ := h.post(fmt.Sprintf("/services/%d/rollback/999", id), nil); !strings.Contains(loc, "err=rollback") {
		t.Fatalf("an unknown deployment cannot be restored: %q", loc)
	}
}

// ── 인증서 · HTTPS 넘기기 ──────────────────────

func TestHTTPSRedirectStartsOnceTheCertificateExists(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	id := h.createService(model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "naru-blog", Port: 80}, model.ServiceSecrets{})
	h.post(fmt.Sprintf("/services/%d/domains", id), url.Values{"domain": {"elsewhere.example.com"}, "https": {"1"}})

	_, _, body := h.get(fmt.Sprintf("/services/%d", id))
	if !strings.Contains(body, "인증서 받는 중") || !strings.Contains(body, "203.0.113.9") {
		t.Fatal("without a certificate the page says so and shows where DNS points")
	}
	h.waitFor("the site is served over HTTP without a redirect", func(cfg string) bool {
		return strings.Contains(cfg, "elsewhere.example.com") && !strings.Contains(cfg, "Location")
	})

	h.certs.issue("elsewhere.example.com")
	h.certs.issue("naru.example.com")
	h.post(fmt.Sprintf("/services/%d/domains", id), url.Values{"domain": {"other.example.com"}}) // 지도를 다시 그리게 한다
	h.waitFor("the site and the admin address redirect to HTTPS", func(cfg string) bool {
		return strings.Count(cfg, `"Location"`) == 2 && strings.Contains(cfg, `"status_code": 307`)
	})
	_, _, body = h.get(fmt.Sprintf("/services/%d", id))
	if !strings.Contains(body, "HTTPS · ") || strings.Contains(body, "인증서 받는 중") || !strings.Contains(body, "Let&#39;s Encrypt") {
		t.Fatal("the page shows until when the certificate lasts")
	}
	_, _, body = h.get("/settings")
	if !strings.Contains(body, "HTTPS · ") {
		t.Fatal("settings show the admin address certificate")
	}
}

// ── 웹서버 설정 (M5) ──────────────────────────

func TestWebServerSettingsFromTheScreen(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	shop := h.createService(model.NewService{Name: name("shop"), Kind: model.KindImage, Source: "me/shop", Alias: "naru-shop", Port: 3000}, model.ServiceSecrets{})
	api := h.createService(model.NewService{Name: name("api"), Kind: model.KindImage, Source: "me/api", Alias: "naru-api", Port: 8080}, model.ServiceSecrets{})
	h.addDomain(shop, "shop.example.com", true, false)
	page := fmt.Sprintf("/services/%d", shop)

	code, loc, _ := h.post(page+"/web", url.Values{
		"headers": {"X-Frame-Options: DENY"}, "allow": {"10.0.0.0/8\n192.168.1.5"}, "login_user": {"admin"}, "login_password": {"longenough"},
		"paths": {"/api api 떼기"}, "advanced": {"respond /health 200"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "ok=web") {
		t.Fatalf("%d %q", code, loc)
	}
	cfg := h.caddy.last()
	for _, want := range []string{"X-Frame-Options", `"client_ip"`, "192.168.1.5/32", `"authentication"`, "$2a$", `"strip_path_prefix": "/api"`, "naru-api:8080", `"/health"`, `"encode"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the web server got %s", want)
		}
	}
	if strings.Contains(cfg, "longenough") {
		t.Fatal("the password itself never reaches the web server")
	}

	_, _, body := h.get(page)
	if !strings.Contains(body, "X-Frame-Options: DENY") || !strings.Contains(body, "/api api 떼기") || !strings.Contains(body, "비워 두면 지금 비밀번호 그대로") || strings.Contains(body, "$2a$") {
		t.Fatal("the screen shows the settings, and only that a password exists")
	}
	if _, _, body := h.get(fmt.Sprintf("/services/%d", api)); !strings.Contains(body, "이 서비스에 보내는 서비스") || !strings.Contains(body, "<b>shop</b>") {
		t.Fatal("the target service says who routes to it")
	}

	// 웹서버가 거절하면 되돌린다 — 이전 설정이 그대로 남고, 웹서버가 한 말이 보인다
	h.caddy.mu.Lock()
	h.caddy.refuseIf = "X-Refuse-Me"
	h.caddy.mu.Unlock()
	loads := len(h.caddy.loads)
	code, _, body = h.post(page+"/web", url.Values{"headers": {"X-Refuse-Me: 1"}, "login_user": {"admin"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "되돌렸어요") || !strings.Contains(body, "bad header") || !strings.Contains(body, "X-Refuse-Me: 1") {
		t.Fatalf("refused: %d", code)
	}
	if got, _ := store.NewWebSettings(h.app.db).Get(ctx, shop); len(got.Headers) != 1 || got.Headers[0].Name != "X-Frame-Options" || got.Login.User != "admin" {
		t.Fatalf("the previous settings are restored: %+v", got)
	}
	if len(h.caddy.loads) != loads && !strings.Contains(h.caddy.last(), "X-Frame-Options") {
		t.Fatal("the web server keeps serving the previous settings")
	}

	// 고급 칸의 틀린 지시어는 웹서버에 닿기 전에 막힌다
	code, _, body = h.post(page+"/web", url.Values{"advanced": {"nonsense"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "unrecognized directive: nonsense") {
		t.Fatalf("compile errors show Caddy's words: %d", code)
	}
	// 적용됐지만 반영되지 않은 지시어는 알려 준다
	if code, _, body := h.post(page+"/web", url.Values{"advanced": {"tls internal"}}); code != http.StatusOK || !strings.Contains(body, "반영되지 않았어요") {
		t.Fatalf("ignored directives are reported: %d", code)
	}
	for form, want := range map[string]string{"/api nowhere": "대상을 찾지 못했어요", "/api shop": "자기 자신으로는", "api api": "/경로 대상"} {
		if code, _, body := h.post(page+"/web", url.Values{"paths": {form}}); code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("%q: %d, want %q", form, code, want)
		}
	}
	if code, _, body := h.post(page+"/web", url.Values{"allow": {"not-an-ip"}}); code != http.StatusBadRequest || !strings.Contains(body, "대역") {
		t.Errorf("bad IP: %d", code)
	}
}

func TestImportFromNginxFillsTheFormWithoutSaving(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	shop := h.createService(model.NewService{Name: name("shop"), Kind: model.KindImage, Source: "me/shop", Alias: "naru-shop", Port: 3000}, model.ServiceSecrets{})
	nginx := "server {\n  server_name shop.example.com;\n  add_header X-Robots-Tag noindex;\n  location /api/ { proxy_pass http://api:8080/; }\n}"
	code, _, body := h.post(fmt.Sprintf("/services/%d/web/nginx", shop), url.Values{"nginx": {nginx}})
	if code != http.StatusOK || !strings.Contains(body, "X-Robots-Tag: noindex") || !strings.Contains(body, "/api api:8080 떼기") || !strings.Contains(body, "server_name") {
		t.Fatalf("the form is filled and skipped lines are listed: %d", code)
	}
	if got, _ := store.NewWebSettings(h.app.db).Get(ctx, shop); len(got.Headers) != 0 {
		t.Fatal("nothing is saved until the user applies")
	}
}

// ── 실행 방식 ─────────────────────────────

// 컨테이너는 별칭으로 보낸다 — 새 컨테이너가 같은 별칭을 받으니 배포해도 웹서버 설정이 그대로다.
func TestContainersAreReachedByTheirAlias(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"traefik/whoami"}, "domain": {"who.example.com"}})
	h.app.wait()
	h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"localhost:5000"}, "domain": {"nas.example.com"}})
	h.waitFor("containers by alias, this server through host.docker.internal, the admin screen as naru:8080", func(cfg string) bool {
		return strings.Contains(cfg, "naru-who:80") && strings.Contains(cfg, "host.docker.internal:5000") && strings.Contains(cfg, "naru:8080") &&
			!strings.Contains(cfg, `"storage"`)
	})
}

// 설치형(systemd)은 지웠다 — 그 설정으로 켜면 조용히 다른 기본값으로 뜨지 않고, 이유를 말하고 멈춘다.
func TestTheRemovedHostInstallRefusesToStart(t *testing.T) {
	for _, install := range []string{"host", "kubernetes"} {
		_, err := OpenWith(Config{DataDir: t.TempDir(), install: install}, slog.New(slog.NewTextHandler(io.Discard, nil)), Outside{Clock: system.Clock{}, Random: system.Random{}, Sender: &fakeCaddy{}})
		if err == nil || !strings.Contains(err.Error(), "NARU_INSTALL") || !strings.Contains(err.Error(), "deploy/docker") {
			t.Fatalf("%s: got %v", install, err)
		}
	}
	if _, err := (Config{DataDir: t.TempDir(), install: "docker"}).resolved(); err != nil {
		t.Fatalf("an old compose file that still says docker keeps working: %v", err)
	}
}

// ── 지켜보기 · 알리기 (M6-1) ──────────────────────

// receiver는 알림을 받는 쪽인 척한다.
type receiver struct {
	mu   sync.Mutex
	got  []map[string]any
	sigs []string
	srv  *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var m map[string]any
		json.NewDecoder(req.Body).Decode(&m)
		r.mu.Lock()
		r.got, r.sigs = append(r.got, m), append(r.sigs, req.Header.Get("X-Naru-Signature-256"))
		r.mu.Unlock()
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.got {
		out = append(out, fmt.Sprint(m["event"]))
	}
	return out
}

func (r *receiver) waitFor(t *testing.T, event string) map[string]any {
	t.Helper()
	for i := 0; i < 300; i++ {
		r.mu.Lock()
		for _, m := range r.got {
			if m["event"] == event {
				r.mu.Unlock()
				return m
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s alert arrived (got %v)", event, r.events())
	return nil
}

func TestAlertAddressesFromTheScreen(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	rcv := newReceiver(t)
	code, loc, _ := h.post("/settings/alerts", url.Values{"name": {"ops"}, "url": {rcv.srv.URL + "/hook/very-secret-path"}, "secret": {"s3cret"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "ok=alert-added") {
		t.Fatalf("%d %q", code, loc)
	}
	_, _, body := h.get("/settings")
	if !strings.Contains(body, "<b>ops</b>") || strings.Contains(body, "very-secret-path") || !strings.Contains(body, "서명") {
		t.Fatal("the address is shown masked, with its format")
	}
	if _, loc, _ := h.post("/settings/alerts/1/test", nil); !strings.Contains(loc, "ok=alert-tested") {
		t.Fatalf("test: %q", loc)
	}
	if m := rcv.waitFor(t, "test"); m["level"] != "info" || rcv.sigs[0] == "" {
		t.Fatalf("a signed test arrives: %v %v", m, rcv.sigs)
	}
	if _, _, body := h.get("/settings"); !strings.Contains(body, "최근에 보낸 것") || !strings.Contains(body, "Naru 알림 시험") {
		t.Fatal("the result is listed")
	}
	if code, _, body := h.post("/settings/alerts", url.Values{"url": {"ftp://example.com/x"}}); code != http.StatusBadRequest || !strings.Contains(body, "https://") {
		t.Fatalf("a bad address is refused: %d", code)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", http.StatusGone) }))
	defer dead.Close()
	h.post("/settings/alerts", url.Values{"url": {dead.URL + "/x"}})
	if code, _, body := h.post("/settings/alerts/2/test", nil); code != http.StatusBadGateway || !strings.Contains(body, "410") || strings.Contains(body, dead.URL+"/x") {
		t.Fatalf("a failing address says why, without the full address: %d", code)
	}
	h.post("/settings/alerts/2/delete", nil)
	if _, _, body := h.get("/settings"); strings.Contains(body, "/settings/alerts/2/test") {
		t.Fatal("removed")
	}
}

func TestAServiceThatStopsAndComesBackIsReported(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{watchEvery: 20 * time.Millisecond})
	h.signedIn()
	rcv := newReceiver(t)
	h.post("/settings/alerts", url.Values{"url": {rcv.srv.URL + "/hook"}})
	id := h.createService(model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "naru-blog", Port: 80}, model.ServiceSecrets{})
	store.NewLiveStates(h.app.db).Save(ctx, id, model.LiveState{Alias: "naru-blog", Instance: "naru-blog-1", Port: 80})
	h.docker.set("naru-blog-1", model.ContainerState{Name: "naru-blog-1", Running: true, State: "running"})
	time.Sleep(100 * time.Millisecond) // 건강한 모습을 먼저 본다

	h.docker.set("naru-blog-1", model.ContainerState{Name: "naru-blog-1", State: "exited", ExitCode: 1})
	down := rcv.waitFor(t, "service.down")
	if down["service"] != "blog" || down["level"] != "problem" || !strings.Contains(fmt.Sprint(down["detail"]), "멈췄어요") {
		t.Fatalf("%v", down)
	}
	h.docker.set("naru-blog-1", model.ContainerState{Name: "naru-blog-1", Running: true, State: "running"})
	if up := rcv.waitFor(t, "service.up"); up["level"] != "recovery" {
		t.Fatalf("%v", up)
	}
	time.Sleep(100 * time.Millisecond)
	count := 0
	for _, e := range rcv.events() {
		if e == "service.down" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("one outage, one alert: %v", rcv.events())
	}
}

// ── 진단 (M6-3) ─────────────────────────────

func TestWrongPortIsDiagnosedAndFixedFromHome(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{watchEvery: 20 * time.Millisecond})
	h.signedIn()
	id := h.createService(model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "naru-blog", Port: 3000}, model.ServiceSecrets{})
	store.NewLiveStates(h.app.db).Save(ctx, id, model.LiveState{Alias: "naru-blog", Instance: "naru-blog-1", Port: 3000})
	h.docker.set("naru-blog-1", model.ContainerState{Name: "naru-blog-1", Running: true, State: "running", Ports: []model.Port{8080}})
	h.docker.listen("naru-blog-1", 8080)                                                             // 이미지가 8080을 열었고, 앱도 거기서 듣는다
	h.post(fmt.Sprintf("/services/%d/domains", id), url.Values{"domain": {"elsewhere.example.com"}}) // DNS는 다른 서버를 가리킨다

	home := func(want string) string {
		t.Helper()
		for i := 0; i < 300; i++ {
			if _, _, body := h.get("/"); strings.Contains(body, want) {
				return body
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _, body := h.get("/")
		t.Fatalf("home never showed %q:\n%s", want, body)
		return ""
	}
	// 주소를 붙이기 전에 시작한 확인이 있을 수 있다 — 주소까지 본 결과(DNS 진단)를 기다린다. 포트 진단은 첫 확인부터 있다.
	body := home("elsewhere.example.com 주소가 다른 서버를 가리켜요")
	for _, want := range []string{fmt.Sprintf(`action="/services/%d/port"`, id), "주의가 필요한 것", "앱이 8080 포트를 듣고 있어요", "포트를 8080로 바꾸기",
		"elsewhere.example.com 주소가 다른 서버를 가리켜요", "203.0.113.9", "198.51.100.24"} {
		if !strings.Contains(body, want) {
			t.Errorf("home is missing %q", want)
		}
	}
	if strings.Contains(body, "앱이 응답하지 않아요") {
		t.Error("a known fix replaces the generic \"not answering\"")
	}
	if i, j := strings.Index(body, "8080 포트를 듣고"), strings.Index(body, "다른 서버를 가리켜요"); j < i {
		t.Error("heaviest first")
	}
	if _, _, page := h.get(fmt.Sprintf("/services/%d", id)); !strings.Contains(page, "포트를 8080로 바꾸기") {
		t.Error("the service page shows its own findings")
	}

	code, loc, _ := h.post(fmt.Sprintf("/services/%d/port", id), url.Values{"port": {"8080"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "ok=port") {
		t.Fatalf("%d %q", code, loc)
	}
	if h.service(int64(id)).Port != 8080 {
		t.Fatal("the port is saved")
	}
	h.waitFor("the web server follows the new port", func(cfg string) bool { return strings.Contains(cfg, "naru-blog:8080") })
	if deploys, _ := h.history().Recent(ctx, id, 5); len(deploys) != 0 {
		t.Fatal("no redeploy — the app already listens there")
	}
	body = home("다른 서버를 가리켜요")
	for i := 0; i < 300 && strings.Contains(body, "8080 포트를 듣고"); i++ {
		time.Sleep(10 * time.Millisecond)
		_, _, body = h.get("/")
	}
	if strings.Contains(body, "8080 포트를 듣고") {
		t.Fatal("fixed, so gone")
	}

	s := h.service(int64(id))
	h.post(fmt.Sprintf("/services/%d/domains/%d/delete", id, s.Domains[0].ID), nil)
	home("모두 정상이에요")

	if code, _, _ := h.post(fmt.Sprintf("/services/%d/port", id), url.Values{"port": {"0"}}); code != http.StatusBadRequest {
		t.Fatalf("a port is required: %d", code)
	}
}

// ── 환경 변수 (.env 붙여넣기) ─────────────────────

func TestPastingADotenvFileWhenCreatingAService(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()

	// 환경 변수 칸은 접힌 "더 정하기" 밖에 있다
	_, _, page := h.get("/services/new?kind=image")
	if i, j := strings.Index(page, `name="env"`), strings.Index(page, "<details"); i < 0 || j < 0 || i > j {
		t.Fatal("the env field sits above the folded options")
	}

	dotenv := "# app\nexport DATABASE_URL=\"postgres://u:p@db/app\"\nNODE_ENV=production # prod\nGREETING='hello world'\n"
	code, loc, _ := h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}, "env": {dotenv}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "envfix=quotes-2.export-1.comment-1") {
		t.Fatalf("%d %q", code, loc)
	}
	if strings.Contains(loc, "DATABASE_URL") || strings.Contains(loc, "postgres") {
		t.Fatal("names and values never go into the address")
	}
	h.app.wait()
	sec, _ := h.secretsOf().Get(ctx, 1)
	if got := strings.Join(sec.Env.List(), "|"); got != "DATABASE_URL=postgres://u:p@db/app|NODE_ENV=production|GREETING=hello world" {
		t.Fatalf("the container gets plain values: %q", got)
	}
	if _, _, body := h.get(loc); !strings.Contains(body, "따옴표 벗김 2줄") || !strings.Contains(body, "export 뺌 1줄") {
		t.Fatal("the deploy page says what was converted")
	}

	// 설정에서도 접지 않고 보이고, 틀린 줄은 몇 번째인지 말한다
	_, _, page = h.get("/services/1")
	if !strings.Contains(page, "DATABASE_URL=postgres://u:p@db/app") {
		t.Fatal("the settings show the stored form")
	}
	code, _, body := h.post("/services/1/settings", url.Values{"source": {"me/app"}, "env": {"A=1\nB=\"unclosed"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "따옴표가 닫히지 않았") || !strings.Contains(body, "(2번째 줄)") {
		t.Fatalf("%d — the error names the line", code)
	}
	code, loc, _ = h.post("/services/1/settings", url.Values{"source": {"me/app"}, "env": {"A = 1\nA=2"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "envfix=spaces-1.duplicate-1") || !strings.HasSuffix(loc, "#settings") {
		t.Fatalf("%d %q", code, loc)
	}
}

// ── .env 파일 칸 ─────────────────────────

func TestTheEnvFileBoxForRepositories(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()

	_, _, page := h.get("/services/new?kind=repo")
	if i, j := strings.Index(page, `name="env_file"`), strings.Index(page, "<details"); i < 0 || j < 0 || i > j {
		t.Fatal("the .env file box sits above the folded options for repositories")
	}
	for _, kind := range []string{"image", "static", "external"} {
		if _, _, page := h.get("/services/new?kind=" + kind); strings.Contains(page, `name="env_file"`) {
			t.Fatalf("%s builds nothing — no .env file box", kind)
		}
	}

	dotenv := "# 빌드에 필요\nNEXT_PUBLIC_API=\"https://api.example.com\" # 그대로\n"
	code, loc, _ := h.post("/services/new", url.Values{"kind": {"repo"}, "source": {"https://github.com/me/web"}, "env_file": {dotenv}})
	if code != http.StatusSeeOther || strings.Contains(loc, "envfix") || strings.Contains(loc, "NEXT_PUBLIC") {
		t.Fatalf("%d %q — kept as written, nothing converted", code, loc)
	}
	h.app.wait()
	sec, _ := h.secretsOf().Get(ctx, 1)
	if sec.EnvFile.Text() != dotenv || len(sec.Env.List()) != 0 {
		t.Fatalf("stored as written, apart from the env vars: %q", sec.EnvFile.Text())
	}
	deps, _ := h.history().Recent(ctx, 1, 5)
	if len(deps) != 1 {
		t.Fatalf("%+v", deps)
	}
	if dep, _ := h.history().Get(ctx, deps[0].ID); !strings.Contains(dep.Log, ".env 파일 2줄") || strings.Contains(dep.Log, "api.example.com") {
		t.Fatalf("the first deploy already has it:\n%s", dep.Log)
	}

	// 설정에서도 보이고, 바꾸면 다시 배포해야 반영된다
	_, _, page = h.get("/services/1")
	if !strings.Contains(page, `name="env_file"`) || !strings.Contains(page, "NEXT_PUBLIC_API=&#34;https://api.example.com&#34;") {
		t.Fatal("the settings show the file")
	}
	form := url.Values{"source": {"https://github.com/me/web"}, "auto_deploy": {"1"}, "env_file": {dotenv}}
	if _, loc, _ := h.post("/services/1/settings", form); strings.Contains(loc, "redeploy") {
		t.Fatalf("unchanged means no redeploy: %q", loc)
	}
	form.Set("env_file", "A=1")
	if _, loc, _ := h.post("/services/1/settings", form); !strings.Contains(loc, "ok=redeploy") {
		t.Fatalf("a changed file needs a redeploy: %q", loc)
	}
	form.Set("env_file", "A=\x00")
	if code, _, body := h.post("/services/1/settings", form); code != http.StatusBadRequest || !strings.Contains(body, "64KB") {
		t.Fatalf("%d", code)
	}
}

// ── 이 서버의 다른 컨테이너 · 앱을 다른 네트워크에도 ──────────────

func TestHomeSumsUpAndTheContainersPageListsEverything(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	if _, _, page := h.get("/"); strings.Contains(page, "이 서버의 다른 컨테이너") {
		t.Fatal("nothing else is running — no summary")
	}
	user := func(n string) model.DockerNetwork { return model.DockerNetwork{Name: n, UserDefined: true} }
	h.docker.set("cairn-web-1", model.ContainerState{Name: "cairn-web-1", Running: true, State: "running", Status: "Up 3 days (healthy)", Ports: []model.Port{3000},
		Project: "cairn", ComposeService: "web", DependsOn: "migrate:service_completed_successfully:false", Networks: []model.DockerNetwork{user("cairn_default")}})
	h.docker.set("cairn-db-1", model.ContainerState{Name: "cairn-db-1", Running: true, State: "running", Status: "Up 4 days", Ports: []model.Port{5432},
		Project: "cairn", Networks: []model.DockerNetwork{user("cairn_default")}})
	h.docker.set("cairn-browser-1", model.ContainerState{Name: "cairn-browser-1", Running: true, State: "running", Status: "Up 2 hours (unhealthy)",
		Project: "cairn", Networks: []model.DockerNetwork{user("cairn_default")}})
	h.docker.set("cairn-migrate-1", model.ContainerState{Name: "cairn-migrate-1", State: "exited", Status: "Exited (0) 4 days ago", Project: "cairn", ComposeService: "migrate"})
	h.docker.set("couchdb", model.ContainerState{Name: "couchdb", Running: true, State: "running", Status: "Up 2 weeks", Ports: []model.Port{5984}, Published: []model.Port{5984},
		Networks: []model.DockerNetwork{{Name: "bridge"}}})
	h.docker.set("old-api", model.ContainerState{Name: "old-api", State: "exited", Status: "Exited (1) 5 days ago"})
	h.docker.set("naru-caddy", model.ContainerState{Name: "naru-caddy", Running: true, State: "running", Status: "Up 1 day", Project: "naru"})
	h.docker.set("naru", model.ContainerState{Name: "naru", Running: true, State: "running", Status: "Up 1 day", Project: "naru"})

	// Naru가 띄운 것
	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}})
	h.app.wait()

	// 홈은 요약만
	_, _, home := h.get("/")
	for _, want := range []string{"이 서버의 다른 컨테이너", "4개 실행 중", "1개 멈춤", "1개 상태 검사 실패", `href="/containers"`} {
		if !strings.Contains(home, want) {
			t.Errorf("the home summary is missing %q", want)
		}
	}
	if strings.Contains(home, "cairn-web-1") {
		t.Fatal("the full list moved to the containers page")
	}

	_, _, page := h.get("/containers")
	for _, want := range []string{"cairn", "compose 묶음, 4개 중 3개 실행", "따로 띄운 것", `href="/containers/cairn-web-1"`, "Up 3 days (healthy)",
		`href="/services/new?kind=external&amp;upstream=cairn-web-1%3a3000"`, "기본 bridge에만 있어요", "Exited (1) 5 days ago", "끝남 (0)",
		`action="/containers/projects/cairn/restart"`, "Naru 자신이 든 묶음", "naru-app-1", "app 서비스"} {
		if !strings.Contains(page, want) {
			t.Errorf("the containers page is missing %q", want)
		}
	}
	if strings.Contains(page, `action="/containers/projects/naru/`) {
		t.Fatal("Naru's own project has no project buttons")
	}

	// 붙이고 나면 버튼 대신 어느 서비스가 보내는지
	h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"cairn-web-1:3000"}, "name": {"cairn"}})
	_, _, page = h.get("/containers")
	if !strings.Contains(page, `<a class="tag-link" href="/services/2">cairn 서비스</a>`) || strings.Contains(page, "upstream=cairn-web-1%3a3000") {
		t.Fatal("a linked container shows its service instead of the button")
	}
	// [주소 붙이기]는 외부 연결 화면을 채워서 연다
	_, _, form := h.get("/services/new?kind=external&upstream=cairn-web-1:3000")
	if !strings.Contains(form, `name="upstream" value="cairn-web-1:3000"`) {
		t.Fatal("the upstream field is filled in")
	}
	if _, _, form := h.get("/services/new?kind=external&upstream=" + url.QueryEscape(`"><script>`)); strings.Contains(form, "<script>") && !strings.Contains(form, `name="upstream" value=""`) {
		t.Fatal("only a well-formed address is filled in")
	}
}

// 컨테이너 화면의 동작 — 보호 규칙, 지우기 확인, 묶음은 끝난 일회성 컨테이너를 다시 돌리지 않는다, 관리 기록.
func TestContainerActionsAndTheirGuards(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.docker.set("cairn-web-1", model.ContainerState{Name: "cairn-web-1", Running: true, State: "running", Status: "Up 3 days", Project: "cairn",
		ComposeService: "web", DependsOn: "migrate:service_completed_successfully:false"})
	h.docker.set("cairn-worker-1", model.ContainerState{Name: "cairn-worker-1", State: "exited", Status: "Exited (137) 1 hour ago", Project: "cairn"})
	h.docker.set("cairn-migrate-1", model.ContainerState{Name: "cairn-migrate-1", State: "exited", Status: "Exited (0) 4 days ago", Project: "cairn", ComposeService: "migrate"})
	h.docker.set("old-api", model.ContainerState{Name: "old-api", State: "exited", Status: "Exited (1) 5 days ago"})
	h.docker.set("naru-caddy", model.ContainerState{Name: "naru-caddy", Running: true, State: "running"})
	h.docker.details = map[string]model.ContainerDetails{"cairn-web-1": {Env: []string{"DB_PASSWORD=hunter2", "TZ=Asia/Seoul"}, Binds: []string{"/srv/up:/up:ro"},
		NamedVolumes: []string{"cairn_cache:/cache"}, Health: "healthy", Restarts: 2}}

	_, _, page := h.get("/containers/cairn-web-1")
	for _, want := range []string{"DB_PASSWORD", "TZ", "/srv/up → /up (ro)", "cairn_cache (volume) → /cache", "cairn-web-1 listening on :80", `action="/containers/cairn-web-1/restart"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the detail is missing %q", want)
		}
	}
	if strings.Contains(page, "hunter2") || strings.Contains(page, "Asia/Seoul") {
		t.Fatal("env values never reach the page")
	}
	if _, _, page := h.get("/containers/cairn-web-1?q=script"); !strings.Contains(page, "alert(1)") || strings.Contains(page, "listening on") || strings.Contains(page, "<script>alert") {
		t.Fatal("log search filters, and log text is escaped")
	}
	if code, _, _ := h.get("/containers/nope"); code != http.StatusNotFound {
		t.Fatal("unknown container")
	}

	if code, loc, _ := h.post("/containers/cairn-web-1/restart", nil); code != http.StatusSeeOther || !strings.Contains(loc, "ok=restart") {
		t.Fatalf("%d %s", code, loc)
	}
	if _, loc, _ := h.post("/containers/naru-caddy/stop", nil); !strings.Contains(loc, "err=protected") {
		t.Fatalf("the web server is protected: %s", loc)
	}
	if _, loc, _ := h.post("/containers/cairn-web-1/remove", url.Values{"confirm": {"cairn-web-1"}}); !strings.Contains(loc, "err=running") {
		t.Fatalf("a running one is not removed: %s", loc)
	}
	if _, loc, _ := h.post("/containers/old-api/remove", url.Values{"confirm": {"old"}}); !strings.Contains(loc, "err=confirm") {
		t.Fatalf("the name must match: %s", loc)
	}
	if _, loc, _ := h.post("/containers/old-api/remove", url.Values{"confirm": {"old-api"}}); !strings.Contains(loc, "ok=removed") {
		t.Fatalf("%s", loc)
	}
	if _, loc, _ := h.post("/containers/old-api/explode", nil); !strings.Contains(loc, "err=action") {
		t.Fatalf("%s", loc)
	}

	h.docker.mu.Lock()
	h.docker.acts = nil
	h.docker.mu.Unlock()
	if _, loc, _ := h.post("/containers/projects/cairn/start", nil); !strings.Contains(loc, "ok=project-start") {
		t.Fatalf("%s", loc)
	}
	h.docker.mu.Lock()
	acts := strings.Join(h.docker.acts, ",")
	h.docker.mu.Unlock()
	if acts != "start cairn-worker-1" {
		t.Fatalf("only the stopped worker starts — the finished migration is not rerun: %s", acts)
	}

	// 고른 것들에 한 번에 — 보호된 것이 섞이면 그것만 막히고 알린다
	h.docker.mu.Lock()
	h.docker.acts = nil
	h.docker.mu.Unlock()
	if _, loc, _ := h.post("/containers", url.Values{"action": {"restart"}, "c": {"cairn-web-1", "naru-caddy"}, "back": {"?state=running"}}); !strings.Contains(loc, "/containers?state=running&err=protected") {
		t.Fatalf("%s", loc)
	}
	if _, loc, _ := h.post("/containers", url.Values{"action": {"stop"}, "c": {"cairn-web-1"}}); loc != "/containers?ok=selected-stop" {
		t.Fatalf("%s", loc)
	}
	if code, _, page := h.post("/containers", url.Values{"action": {"remove"}, "c": {"cairn-worker-1"}}); code != http.StatusOK || !strings.Contains(page, "컨테이너 1개를 지울까요?") {
		t.Fatalf("removing goes through a confirm page: %d", code)
	}
	if _, loc, _ := h.post("/containers", url.Values{"action": {"start"}, "back": {"//evil.example"}}); loc != "/containers?err=none" {
		t.Fatalf("nothing selected, and back stays on this page: %s", loc)
	}
	h.docker.mu.Lock()
	acts = strings.Join(h.docker.acts, ",")
	h.docker.mu.Unlock()
	if acts != "restart cairn-web-1,stop cairn-web-1" {
		t.Fatalf("%s", acts)
	}
	// 찾기 · 거르기
	_, _, list := h.get("/containers?state=stopped")
	if !strings.Contains(list, "cairn-web-1") || strings.Contains(list, `href="/containers/naru-caddy"`) {
		t.Fatal("stopped only (cairn-web-1 was just stopped)")
	}
	if _, _, list := h.get("/containers?q=migrate"); !strings.Contains(list, "cairn-migrate-1") || strings.Contains(list, `href="/containers/cairn-web-1"`) {
		t.Fatal("search by name")
	}

	// 관리 기록
	_, _, settings := h.get("/settings")
	for _, want := range []string{"cairn-web-1 컨테이너를 다시 시작했어요", "old-api 컨테이너를 지웠어요", "cairn 묶음을 켰어요", "cairn-web-1 컨테이너를 멈췄어요"} {
		if !strings.Contains(settings, want) {
			t.Errorf("the activity log is missing %q", want)
		}
	}
}

func TestStoragePageDeletesOneAtATime(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	_, _, page := h.get("/storage")
	for _, want := range []string{"old/app:1", "0123456789ab", "안 씀", "컨테이너 1개가 씀", "shop_dbdata", "left_over", "안 쓰는 이미지", "890.0 MB"} {
		if !strings.Contains(page, want) {
			t.Errorf("the storage page is missing %q", want)
		}
	}
	if strings.Contains(page, `action="/storage/images/sha256:fedcba9876543210/delete"`) || strings.Contains(page, `action="/storage/volumes/shop_dbdata/delete"`) {
		t.Fatal("used ones have no delete form")
	}
	if _, loc, _ := h.post("/storage/volumes/left_over/delete", url.Values{"confirm": {"left"}}); !strings.Contains(loc, "err=confirm") {
		t.Fatalf("%s", loc)
	}
	if _, loc, _ := h.post("/storage/volumes/left_over/delete", url.Values{"confirm": {"left_over"}}); !strings.Contains(loc, "ok=volume-removed") {
		t.Fatalf("%s", loc)
	}
	if _, loc, _ := h.post("/storage/volumes/shop_dbdata/delete", url.Values{"confirm": {"shop_dbdata"}}); !strings.Contains(loc, "err=inuse") {
		t.Fatalf("Docker refuses a volume in use: %s", loc)
	}
	if _, loc, _ := h.post("/storage/images/sha256:0123456789abcdef/delete", url.Values{"confirm": {"0123456789ab"}}); !strings.Contains(loc, "ok=image-removed") {
		t.Fatalf("%s", loc)
	}
}

func TestAServiceCanJoinOtherNetworks(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.docker.set("cairn-db-1", model.ContainerState{Name: "cairn-db-1", Running: true, State: "running", Ports: []model.Port{5432},
		Project: "cairn", Networks: []model.DockerNetwork{{Name: "cairn_default", UserDefined: true}}})

	// 칸은 저장소·이미지에만 있고, 이 서버의 네트워크를 제안한다
	_, _, page := h.get("/services/new?kind=image")
	if !strings.Contains(page, `name="networks"`) || !strings.Contains(page, `<option value="cairn_default">`) {
		t.Fatal("the networks field suggests networks other containers are on")
	}
	for _, kind := range []string{"static", "external"} {
		if _, _, page := h.get("/services/new?kind=" + kind); strings.Contains(page, `name="networks"`) {
			t.Fatalf("%s runs no container — no networks field", kind)
		}
	}
	if code, _, body := h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}, "networks": {"host"}}); code != http.StatusBadRequest || !strings.Contains(body, "bridge·host·none") {
		t.Fatalf("%d — the default networks are refused", code)
	}

	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}, "networks": {"cairn_default"}})
	h.app.wait()
	if spec := h.docker.specOf("naru-app-1"); strings.Join(spec.ExtraNetworks, " ") != "cairn_default" {
		t.Fatalf("the first container already joins it: %+v", spec)
	}
	_, _, page = h.get("/services/1")
	if !strings.Contains(page, `name="networks" value="cairn_default"`) {
		t.Fatal("the settings show it")
	}
	form := url.Values{"source": {"me/app"}, "networks": {"cairn_default"}}
	if _, loc, _ := h.post("/services/1/settings", form); strings.Contains(loc, "redeploy") {
		t.Fatalf("unchanged means no redeploy: %q", loc)
	}
	form.Set("networks", "cairn_default, db-net")
	if _, loc, _ := h.post("/services/1/settings", form); !strings.Contains(loc, "ok=redeploy") {
		t.Fatalf("changed networks need a redeploy: %q", loc)
	}
	if s := h.service(1); s.Networks != "cairn_default db-net" {
		t.Fatalf("%q", s.Networks)
	}
}

// ── 빌드 타깃 ──────────────

func TestBuildTargetField(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	if _, _, page := h.get("/services/new?kind=repo"); !strings.Contains(page, `name="build_target"`) {
		t.Fatal("repositories have the field")
	}
	if _, _, page := h.get("/services/new?kind=image"); strings.Contains(page, `name="build_target"`) {
		t.Fatal("images build nothing")
	}
	if code, _, body := h.post("/services/new", url.Values{"kind": {"repo"}, "source": {"https://github.com/me/shop"}, "build_target": {"-web"}}); code != http.StatusBadRequest || !strings.Contains(body, "단계 이름") {
		t.Fatalf("%d", code)
	}
	h.post("/services/new", url.Values{"kind": {"repo"}, "source": {"https://github.com/me/shop"}, "build_target": {"Web"}})
	h.app.wait()
	if s := h.service(1); s.BuildTarget != "web" {
		t.Fatalf("%q", s.BuildTarget)
	}
	if _, _, page := h.get("/services/1"); !strings.Contains(page, `name="build_target" value="web"`) {
		t.Fatal("the settings show it")
	}
	form := url.Values{"source": {"https://github.com/me/shop"}, "build_target": {"web"}}
	if _, loc, _ := h.post("/services/1/settings", form); strings.Contains(loc, "redeploy") {
		t.Fatal("unchanged")
	}
	form.Set("build_target", "server")
	if _, loc, _ := h.post("/services/1/settings", form); !strings.Contains(loc, "ok=redeploy") {
		t.Fatal("a new target needs a redeploy")
	}
}

// [포트 바꾸기]는 포트만 바꾼다 — 다른 네트워크·빌드 타깃은 그대로.
func TestSettingThePortKeepsTheOtherSettings(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}, "networks": {"shop_default"}})
	h.app.wait()
	if code, _, _ := h.post("/services/1/port", url.Values{"port": {"8080"}}); code != http.StatusSeeOther {
		t.Fatalf("%d", code)
	}
	if s := h.service(1); s.Port != 8080 || s.Networks != "shop_default" {
		t.Fatalf("%+v", s)
	}
}

// ── 서버 화면 · 관리 기록 ──────────────

func TestServerPageAndAdminLog(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()

	// 로그인 — 실패도 남는다 (적은 이름 그대로, 비밀번호는 남기지 않는다)
	h.post("/logout", nil)
	h.post("/login", url.Values{"username": {"admin"}, "password": {"wrong-password"}})
	if code, _, _ := h.post("/login", url.Values{"username": {"admin"}, "password": {"longenough"}}); code != http.StatusSeeOther {
		t.Fatal("signed in again")
	}
	_, _, page := h.get("/")
	if !strings.Contains(page, `href="/server"`) {
		t.Fatal("the menu has the server page")
	}
	if _, _, page := h.get("/server"); !strings.Contains(page, "아직 기록이 없어요") {
		t.Fatal("nothing recorded yet")
	}

	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}})
	h.app.wait()
	h.post("/services/1/deploy", nil)
	h.app.wait()
	if code, _, _ := h.post("/services/1/settings", url.Values{"source": {"me/app"}, "port": {"99999"}}); code != http.StatusBadRequest {
		t.Fatal("a refused change")
	}
	h.post("/services/1/settings", url.Values{"source": {"me/app"}, "env": {"SECRET_TOKEN=s3cr3t"}})
	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/old"}, "name": {"old"}})
	h.app.wait()
	h.post("/services/2/delete", url.Values{"confirm": {"old"}})
	h.app.wait()

	_, _, page = h.get("/settings")
	audit := page[strings.Index(page, `id="audit"`):]
	audit = audit[:strings.Index(audit, "</section>")]
	for _, want := range []string{"로그인 실패", "로그인", "로그아웃", "app 서비스를 만들었어요", "app 배포를 시작했어요", "app 설정을 바꿨어요",
		"old 서비스를 지웠어요", "<b>admin</b>", "127.0.0.1"} {
		if !strings.Contains(audit, want) {
			t.Errorf("the admin log is missing %q", want)
		}
	}
	if strings.Contains(audit, "s3cr3t") || strings.Contains(audit, "SECRET_TOKEN") || strings.Contains(audit, "wrong-password") || strings.Contains(audit, "99999") {
		t.Fatal("values and refused changes are not logged")
	}
	if strings.Count(audit, "app 설정을 바꿨어요") != 1 {
		t.Fatal("only the change that went through")
	}

	// 서버 화면 — 기록이 있으면 그래프, 서비스별, 일어난 일(성공한 배포는 Naru가 본 일로)
	now := time.Now()
	m := store.NewMetrics(h.app.db)
	for i := 0; i < 30; i++ {
		m.Add(ctx, now.Add(-time.Duration(i)*time.Minute), map[model.ServiceID]model.MetricPoint{
			0: {CPU: float64(10 + i), MemUsed: 2 << 30, MemTotal: 8 << 30, DiskUsed: 40 << 30, DiskTotal: 100 << 30, NetIn: uint64(1000 * (30 - i)), NetOut: uint64(9000 * (30 - i))},
			1: {CPU: 2, MemUsed: 120 << 20, MemTotal: 8 << 30},
		})
	}
	var server string
	for i := 0; i < 100; i++ {
		_, _, server = h.get("/server?range=1h")
		if strings.Contains(server, "app 배포 #2 성공") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{`aria-current="true">1시간`, `<polyline class="ln cpu" points="0.0,`, `<polyline class="ln out" points="`,
		`<a href="/services/1">app</a>`, "120.0 MB", "app 배포 #2 성공", "최고 39%"} {
		if !strings.Contains(server, want) {
			t.Errorf("the server page is missing %q", want)
		}
	}
	if strings.Contains(server, "로그인 실패") {
		t.Fatal("admin actions stay on the settings page")
	}
	if _, _, odd := h.get("/server?range=forever"); !strings.Contains(odd, `aria-current="true">24시간`) {
		t.Fatal("an unknown period is a day")
	}
}

// ── 로그 보관 · 찾기 · 받기 ──────────────

func TestLogsOfEarlierDeploymentsAreKeptAndSearchable(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.post("/services/new", url.Values{"kind": {"image"}, "source": {"me/app"}, "domain": {"app.example.com"}})
	h.app.wait()
	h.post("/services/1/deploy", nil)
	h.app.wait()

	_, _, page := h.get("/services/1/logs")
	if !strings.Contains(page, `<option value="1" >#1 · `) || !strings.Contains(page, "#2 지금 도는 것") {
		t.Fatal("the first deployment's output was kept when it was taken down, and can be picked")
	}
	if !strings.Contains(h.lastDeployLog(1), "naru-app-1의 앱 출력 2줄을 보관했어요") {
		t.Fatal(h.lastDeployLog(1))
	}
	_, _, page = h.get("/services/1/logs?deploy=1")
	if !strings.Contains(page, "#1 배포의 보관된 앱 출력이에요") || !strings.Contains(page, "naru-app-1 listening on :80") || strings.Contains(page, "naru-app-2 listening") {
		t.Fatal("the kept output of #1, not the running one")
	}

	// 찾기 — 앱 출력과 요청에서
	_, _, page = h.get("/services/1/logs?q=" + url.QueryEscape("LISTENING"))
	if !strings.Contains(page, "naru-app-2 listening on :80") || strings.Contains(page, "alert(1)") {
		t.Fatal("only matching lines, case-insensitive")
	}
	if _, _, page = h.get("/services/1/logs?q=nothing-like-this"); !strings.Contains(page, "든 줄이 없어요") {
		t.Fatal("says when nothing matches")
	}
	if _, _, page = h.get("/services/1/logs?q=" + url.QueryEscape(`"><script>`)); strings.Contains(page, `"><script>`) {
		t.Fatal("the search text is escaped")
	}

	// 받기 — 글자 파일로
	res, err := h.client.Get(h.srv.URL + "/services/1/logs.txt?deploy=1&q=listening")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), `filename="app-1-logs.txt"`) ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || !strings.Contains(string(body), "stdout naru-app-1 listening on :80") {
		t.Fatalf("%d %v\n%s", res.StatusCode, res.Header, body)
	}

	// 서비스를 지우면 보관한 것도
	h.post("/services/1/delete", url.Values{"confirm": {"app"}})
	h.app.wait()
	if _, err := os.Stat(filepath.Join(h.app.cfg.DataDir, "logs", "app")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kept logs go with the service: %v", err)
	}
}

func (h *harness) lastDeployLog(id model.ServiceID) string {
	deps, _ := h.history().Recent(ctx, id, 1)
	if len(deps) == 0 {
		return ""
	}
	d, _ := h.history().Get(ctx, deps[0].ID)
	return d.Log
}

// ── 저장소로 옮기기 (넘겨받기) ──────────────

func TestMovingARunningContainerToARepository(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	user := func(n string) model.DockerNetwork { return model.DockerNetwork{Name: n, UserDefined: true} }
	h.docker.set("cairn-web-1", model.ContainerState{Name: "cairn-web-1", Running: true, State: "running", Status: "Up 3 days", Ports: []model.Port{3000},
		Project: "cairn", Networks: []model.DockerNetwork{user("cairn_default"), user("naru-net")}})
	h.docker.set("lonely", model.ContainerState{Name: "lonely", Running: true, State: "running", Networks: []model.DockerNetwork{user("x")}})
	h.docker.mu.Lock()
	h.docker.details = map[string]model.ContainerDetails{
		"cairn-web-1": {
			Env:      []string{"DATABASE_URL=postgres://u:hunter2@cairn-db-1/app", "PATH=/usr/bin", "NODE_ENV=production", "CERT=line one\nline two"},
			ImageEnv: []string{"PATH=/usr/bin"},
			Binds:    []string{"/srv/uploads:/app/uploads"}, NamedVolumes: []string{"cairn_cache:/cache"},
			Source: "https://github.com/me/cairn",
		},
		"lonely": {Env: []string{"A=1"}},
	}
	h.docker.mu.Unlock()

	// 지금: 외부 연결로 주소를 붙여 쓰고 있다 (웹서버 설정도 있다)
	h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"cairn-web-1:3000"}, "domain": {"cairn.example.com"}, "name": {"cairn"}})
	if code, _, _ := h.post("/services/1/web", url.Values{"headers": {"X-Frame-Options: DENY"}}); code != http.StatusSeeOther {
		t.Fatalf("%d", code)
	}
	if _, _, page := h.get("/containers/cairn-web-1"); !strings.Contains(page, `href="/services/new?kind=repo&amp;adopt=cairn-web-1"`) || strings.Contains(page, "hunter2") {
		t.Fatal("a running container has the move button, linked or not — and its env values stay off the page")
	}

	// 화면: 읽은 값으로 채워져 있고, 환경 변수는 이름만
	_, _, form := h.get("/services/new?kind=repo&adopt=cairn-web-1")
	for _, want := range []string{`name="adopt" value="cairn-web-1"`, `name="source" value=""`, `<span class="mono">https://github.com/me/cairn</span>`, `name="port" value="3000"`,
		`name="networks" value="cairn_default"`, "/srv/uploads:/app/uploads", "cairn_cache:/cache",
		`name="env_from" value="DATABASE_URL" checked`, `name="env_from" value="NODE_ENV" checked`, `name="env_from" value="PATH" >`,
		`name="take_over" value="1" checked`, "cairn.example.com", "<details class=\"more\" open>"} {
		if !strings.Contains(form, want) {
			t.Errorf("the form is missing %q", want)
		}
	}
	if strings.Contains(form, "hunter2") || strings.Contains(form, "cairn-db-1/app") || strings.Contains(form, "line one") {
		t.Fatal("values never reach the screen")
	}
	// Naru의 것이나 없는 것은 넘겨받을 수 없다 — 보통의 새 서비스 화면이 된다. 보내는 서비스가 없으면 옮길 주소도 없다
	if _, _, form := h.get("/services/new?kind=repo&adopt=naru-caddy"); strings.Contains(form, `name="adopt"`) {
		t.Fatal("only containers Naru did not start can be taken over")
	}
	if _, _, form := h.get("/services/new?kind=repo&adopt=lonely"); !strings.Contains(form, `name="adopt" value="lonely"`) || strings.Contains(form, `name="take_over"`) {
		t.Fatal("without a service sending there, there are no addresses to move")
	}

	// 여러 줄 값은 가져오지 않는다 — 말하고, 고른 것은 그대로 다시 보여 준다
	adopt := url.Values{"kind": {"repo"}, "source": {"https://github.com/me/cairn"}, "name": {"cairn-app"}, "adopt": {"cairn-web-1"},
		"port": {"3000"}, "networks": {"cairn_default"}, "env": {"NODE_ENV=staging"}, "take_over": {"1"}}
	adopt["env_from"] = []string{"DATABASE_URL", "CERT"}
	code, _, body := h.post("/services/new", adopt)
	if code != http.StatusBadRequest || !strings.Contains(body, "여러 줄 값은") || !strings.Contains(body, `name="env_from" value="CERT" checked`) ||
		strings.Contains(body, `name="env_from" value="NODE_ENV" checked`) || strings.Contains(body, "hunter2") {
		t.Fatalf("%d", code)
	}
	if all, _ := h.services().List(ctx); len(all) != 1 {
		t.Fatal("nothing was created")
	}

	// 만들기: 값은 서버 안에서만 복사된다
	adopt["env_from"] = []string{"DATABASE_URL", "NODE_ENV", "NOT_THERE"}
	code, loc, _ := h.post("/services/new", adopt)
	if code != http.StatusSeeOther || strings.Contains(loc, "hunter2") {
		t.Fatalf("%d %q", code, loc)
	}
	h.app.wait()
	sec, _ := h.secretsOf().Get(ctx, 2)
	if got := strings.Join(sec.Env.List(), "|"); got != "DATABASE_URL=postgres://u:hunter2@cairn-db-1/app|NODE_ENV=production|NODE_ENV=staging" {
		t.Fatalf("the chosen ones come first, what was typed wins: %q", got)
	}
	spec := h.docker.specOf("naru-cairn-app-1")
	if !slices.Contains(spec.Env, "DATABASE_URL=postgres://u:hunter2@cairn-db-1/app") || strings.Join(spec.ExtraNetworks, " ") != "cairn_default" {
		t.Fatalf("%+v", spec)
	}

	// 배포가 성공해서 주소와 웹서버 설정이 옮겨 왔다. 옛 컨테이너와 옛 서비스는 그대로 있다
	if old, fresh := h.service(1), h.service(2); len(old.Domains) != 0 || len(fresh.Domains) != 1 || fresh.Domains[0].Domain.String() != "cairn.example.com" || fresh.TakesOver != 0 {
		t.Fatalf("old=%v new=%v mark=%d", old.Domains, fresh.Domains, fresh.TakesOver)
	}
	if ws, _ := store.NewWebSettings(h.app.db).Get(ctx, 2); len(ws.Headers) != 1 || ws.Headers[0].Name != "X-Frame-Options" {
		t.Fatalf("%+v", ws)
	}
	h.waitFor("the address goes to the new container", func(cfg string) bool {
		return strings.Contains(cfg, "naru-cairn-app:3000") && !strings.Contains(cfg, "cairn-web-1:3000")
	})
	deps, _ := h.history().Recent(ctx, 2, 5)
	if dep, _ := h.history().Get(ctx, deps[0].ID); !strings.Contains(dep.Log, "주소 1개와 웹서버 설정을 이 서비스로 옮겼어요") || strings.Contains(dep.Log, "hunter2") {
		t.Fatalf("%s", dep.Log)
	}
	h.docker.mu.Lock()
	still := h.docker.states["cairn-web-1"].Running
	h.docker.mu.Unlock()
	if !still {
		t.Fatal("the old container is never stopped")
	}
	if _, _, page := h.get("/services/2"); strings.Contains(page, "옮겨 와요") {
		t.Fatal("the notice is gone once the move has happened")
	}

	// 화면이 보낸 표시만으로는 남의 서비스 주소를 옮길 수 없다 — 그 컨테이너로 보내는 서비스가 있어야 한다
	h.post("/services/new", url.Values{"kind": {"repo"}, "source": {"https://github.com/me/x"}, "name": {"sneaky"}, "adopt": {"lonely"}, "take_over": {"1"}})
	h.app.wait()
	if s := h.service(3); s.TakesOver != 0 || len(h.service(2).Domains) != 1 {
		t.Fatalf("%+v", s)
	}
}

// ── 다른 네트워크의 컨테이너 · 이 서버의 포트 ──────────────

func TestReachingContainersOnOtherNetworks(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{watchEvery: 20 * time.Millisecond})
	h.signedIn()
	h.docker.mu.Lock()
	h.docker.nets = map[string][]model.DockerNetwork{
		"grafana": {{Name: "monitor_default", UserDefined: true}},
		"couchdb": {{Name: "bridge"}},
	}
	h.docker.mu.Unlock()
	h.docker.set("grafana", model.ContainerState{Name: "grafana", Running: true, State: "running", Ports: []model.Port{3000}})
	h.docker.set("couchdb", model.ContainerState{Name: "couchdb", Running: true, State: "running", Ports: []model.Port{5984}})

	// 외부 연결 칸에 이 서버의 다른 컨테이너를 제안한다 (웹서버·Naru 자신은 빼고)
	_, _, page := h.get("/services/new?kind=external")
	if !strings.Contains(page, `<option value="grafana:3000">`) || !strings.Contains(page, `<option value="couchdb:5984">`) || strings.Contains(page, `value="naru-caddy`) {
		t.Fatal("the external field suggests this server's other containers")
	}

	h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"grafana:3000"}, "domain": {"grafana.example.com"}})
	h.waitFor("grafana is routed by name", func(cfg string) bool { return strings.Contains(cfg, "grafana:3000") })
	for i := 0; i < 300 && len(h.docker.joined()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if fmt.Sprint(h.docker.joined()) != "[monitor_default<-naru-caddy]" {
		t.Fatalf("the web server joins grafana's network: %v", h.docker.joined())
	}

	// 기본 bridge에만 있는 컨테이너와 닿지 않는 이 서버의 포트는 이유를 말한다
	h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"couchdb:5984"}, "domain": {"couch.example.com"}})
	h.docker.listen("host.docker.internal", 1)
	h.post("/services/new", url.Values{"kind": {"external"}, "upstream": {"localhost:5000"}, "domain": {"nas.example.com"}})
	var body string
	for i := 0; i < 300; i++ {
		if _, _, body = h.get("/"); strings.Contains(body, "couchdb 컨테이너에 이름으로 닿을 수 없어요") && strings.Contains(body, "5000 포트에 웹서버가 닿지 못해요") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(body, "docker network connect naru-net couchdb") || !strings.Contains(body, "172.17.0.1:5000:5000") {
		t.Fatalf("home explains both:\n%s", body)
	}
}

// ── CPU·메모리 (M6-4) ─────────────────────────

func TestServiceUsageOnHomeAndServicePage(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{watchEvery: 20 * time.Millisecond})
	h.signedIn()
	id := h.createService(model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "naru-blog", Port: 80}, model.ServiceSecrets{})
	store.NewLiveStates(h.app.db).Save(ctx, id, model.LiveState{Alias: "naru-blog", Instance: "naru-blog-1", Port: 80})
	h.docker.set("naru-blog-1", model.ContainerState{Name: "naru-blog-1", Running: true, State: "running"})

	var body string
	for i := 0; i < 300; i++ {
		if _, _, body = h.get("/"); strings.Contains(body, "svc-usage") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(body, "3.2%") || !strings.Contains(body, "150.0 MB") {
		t.Fatalf("the home card shows CPU and memory:\n%s", body)
	}
	_, _, page := h.get(fmt.Sprintf("/services/%d", id))
	for _, want := range []string{"3.2%", "서버 전체 기준", "150.0 MB", "2.0 GB"} {
		if !strings.Contains(page, want) {
			t.Errorf("service page is missing %q", want)
		}
	}

	// 직접 멈추면 사용량은 보이지 않는다
	h.post(fmt.Sprintf("/services/%d/stop", id), nil)
	if _, _, page := h.get(fmt.Sprintf("/services/%d", id)); strings.Contains(page, "서버 전체 기준") {
		t.Fatal("a stopped service shows no usage")
	}
}

func TestAFailedDeployIsReported(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	rcv := newReceiver(t)
	h.post("/settings/alerts", url.Values{"url": {rcv.srv.URL + "/hook"}})
	// 저장소에 없는 빌드 경로 — 배포가 확실히 실패한다
	id := h.createService(model.NewService{Name: name("app"), Kind: model.KindRepo, Source: "https://github.com/me/app", Branch: "main", BuildPath: "no/such/folder"}, model.ServiceSecrets{})
	h.post(fmt.Sprintf("/services/%d/deploy", id), nil)
	h.app.wait()
	if m := rcv.waitFor(t, "deploy.failed"); m["service"] != "app" || m["level"] != "problem" {
		t.Fatalf("%v", m)
	}
}

// ── 로그 (M6-2) ─────────────────────────────

func TestLogsPageMixesAppOutputAndRequests(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	id := h.createService(model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "naru-blog", Port: 80}, model.ServiceSecrets{})
	store.NewLiveStates(h.app.db).Save(ctx, id, model.LiveState{Alias: "naru-blog", Instance: "naru-blog-1", Port: 80})
	h.post(fmt.Sprintf("/services/%d/domains", id), url.Values{"domain": {"blog.example.com"}, "https": {"1"}})
	// 웹서버가 남긴 접근 로그 (Docker 방식: <데이터>/caddy/data/access.log)
	logDir := filepath.Join(h.app.cfg.DataDir, "caddy", "data")
	os.MkdirAll(logDir, 0o755)
	os.WriteFile(filepath.Join(logDir, "access.log"), []byte(
		`{"logger":"http.log.access.access","ts":1791633602.0,"request":{"host":"blog.example.com","method":"GET","uri":"/posts?page=2"},"status":200,"duration":0.012}`+"\n"+
			`{"logger":"http.log.access.access","ts":1791633602.5,"request":{"host":"other.example.com","method":"GET","uri":"/x"},"status":404,"duration":0.001}`+"\n"), 0o644)

	code, _, body := h.get(fmt.Sprintf("/services/%d/logs", id))
	if code != http.StatusOK {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{"naru-blog-1 listening on :80", "/posts?page=2", "12ms", "&lt;script&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page is missing %q", want)
		}
	}
	if strings.Contains(body, "other.example.com") || strings.Contains(body, "<script>alert") {
		t.Fatal("only this service's requests, and app output is escaped")
	}
	if i, j := strings.Index(body, "listening on"), strings.Index(body, "/posts"); i < 0 || j < i {
		t.Fatal("in time order")
	}
	if _, _, body := h.get(fmt.Sprintf("/services/%d/logs?show=request", id)); strings.Contains(body, "listening on") || !strings.Contains(body, "/posts") {
		t.Fatal("request filter")
	}
	if _, _, body := h.get(fmt.Sprintf("/services/%d", id)); !strings.Contains(body, fmt.Sprintf("/services/%d/logs", id)) {
		t.Fatal("the service page links to its logs")
	}
	if code, _, _ := h.get("/services/999/logs"); code != http.StatusNotFound {
		t.Fatal("unknown service")
	}
	h.waitFor("the web server logs this service's requests only", func(cfg string) bool {
		return strings.Contains(cfg, `"filename": "/data/access.log"`) && strings.Contains(cfg, `"blog.example.com": [`) && strings.Contains(cfg, `"skip_unmapped_hosts": true`)
	})
}

// ── 웹훅 ─────────────────────────────────

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *harness) hook(id model.ServiceID, body string, headers map[string]string) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/hooks/%d", h.srv.URL, id), strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestWebhooks(t *testing.T) {
	h := newHarness(t, "")
	id := h.createService(model.NewService{Name: name("blog"), Kind: model.KindRepo, Source: "https://github.com/me/blog", Branch: "main", AutoDeploy: true},
		model.ServiceSecrets{WebhookSecret: "s3cret"})
	push := `{"ref":"refs/heads/main"}`
	deploys := func() int {
		all, _ := h.history().Recent(ctx, id, 100)
		return len(all)
	}

	if code, _ := h.hook(id, push, nil); code != http.StatusUnauthorized {
		t.Fatalf("unsigned → 401, got %d", code)
	}
	if code, _ := h.hook(id, push, map[string]string{"X-Hub-Signature-256": sign("wrong", []byte(push))}); code != http.StatusUnauthorized {
		t.Fatal("wrong secret → 401")
	}
	if code, _ := h.hook(id, `{"ref":"refs/heads/evil"}`, map[string]string{"X-Hub-Signature-256": sign("s3cret", []byte(push))}); code != http.StatusUnauthorized {
		t.Fatal("a signature for another body does not count")
	}
	if code, _ := h.hook(id, "{}", map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": sign("s3cret", []byte("{}"))}); code != http.StatusOK {
		t.Fatal("ping → pong")
	}
	other := `{"ref":"refs/heads/feature"}`
	if code, body := h.hook(id, other, map[string]string{"X-Hub-Signature-256": sign("s3cret", []byte(other))}); code != http.StatusOK || !strings.Contains(body, "ignored") {
		t.Fatalf("other branches are acknowledged and ignored: %d %s", code, body)
	}
	if deploys() != 0 {
		t.Fatal("nothing deployed yet")
	}
	h.hold()
	if code, body := h.hook(id, push, map[string]string{"X-Hub-Signature-256": sign("s3cret", []byte(push))}); code != http.StatusAccepted || !strings.Contains(body, "deploying") {
		t.Fatalf("a signed push to main deploys: %d %s", code, body)
	}
	if code, body := h.hook(id, push, map[string]string{"X-Gitlab-Token": "s3cret"}); code != http.StatusAccepted || !strings.Contains(body, "another will follow") {
		t.Fatalf("GitLab token works too; pushes during a deploy are queued: %d %s", code, body)
	}
	if code, _ := h.hook(id, push, map[string]string{}); code != http.StatusUnauthorized {
		t.Fatal("still 401 without a signature")
	}
	if code, _ := h.hook(id, push+"", map[string]string{"X-Unused": "x"}); code != http.StatusUnauthorized {
		t.Fatal("unknown headers do not count")
	}
	if s := h.service(int64(id)); s.HookLog.At.IsZero() || s.HookLog.Result != "queued" {
		t.Fatalf("the last receipt is recorded: %+v", s.HookLog)
	}
	h.release()
	h.app.wait()
	if deploys() != 2 {
		t.Fatalf("pushes during a deploy collapse into one more deploy, got %d", deploys())
	}
	if code, _ := h.hook(999, push, nil); code != http.StatusNotFound {
		t.Fatal("unknown service → 404")
	}
	big := strings.Repeat("x", 1<<20+10)
	if code, _ := h.hook(id, big, map[string]string{"X-Gitlab-Token": "s3cret"}); code != http.StatusRequestEntityTooLarge {
		t.Fatal("bodies are limited")
	}
	q := url.Values{"secret": {"s3cret"}}
	if code, _ := h.hook(id, "{}", nil); code != http.StatusUnauthorized {
		t.Fatal("no secret")
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/hooks/%d?%s", h.srv.URL, id, q.Encode()), strings.NewReader("{}"))
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	h.app.wait()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("registries that cannot sign use ?secret=: %d", res.StatusCode)
	}
}

// ── 복구 명령이 쓰는 조립 ────────────────────────

func TestOpeningForAShellCommandLeavesTheRunningServerAlone(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work", "blog-7")
	os.MkdirAll(work, 0o755)
	cfg := Config{DataDir: dir, CaddySocket: "/run/caddy/admin.sock", SelfUpstream: "naru:8080"}
	first, err := OpenWith(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Outside{Clock: system.Clock{}, Random: system.Random{}, Sender: &fakeCaddy{}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := store.NewServices(first.db).Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindImage})
	did, _ := store.NewDeployments(first.db).Start(ctx, id, model.ReasonManual)
	first.Close()

	shell, err := OpenWith(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Outside{Clock: system.Clock{}, Random: system.Random{}, Sender: &fakeCaddy{}})
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	if _, err := os.Stat(work); err != nil {
		t.Fatal("a shell command must not clear the server's work folders")
	}
	if d, _ := store.NewDeployments(shell.db).Get(ctx, did); d.Status != model.DeployRunning {
		t.Fatal("a shell command must not close the server's running deploys")
	}
}

// fakeCompose는 docker compose다 — up하면 web(3000)·worker와 끝난 migrate가 떠 있다.
type fakeCompose struct{ docker *fakeDocker }

func (fakeCompose) Build(_ context.Context, p model.ComposeProject, _ io.Writer) error {
	if _, err := os.Stat(filepath.Join(p.Dir, "Dockerfile")); err != nil {
		return errors.New("the code is in the work folder")
	}
	return nil
}

func (f *fakeCompose) Up(_ context.Context, p model.ComposeProject, _ io.Writer) error {
	for svc, st := range map[string]model.ContainerState{
		"web":     {State: "running", Running: true, Status: "Up 1 second", Ports: []model.Port{3000}, DependsOn: "migrate:service_completed_successfully:false"},
		"worker":  {State: "running", Running: true, Status: "Up 1 second"},
		"migrate": {State: "exited", Status: "Exited (0) 2 seconds ago"},
	} {
		st.Name, st.Project, st.ComposeService = p.Container(svc), p.Name, svc
		f.docker.set(st.Name, st)
	}
	return nil
}
func (fakeCompose) Switch(context.Context, model.ComposeProject, bool) error { return nil }
func (fakeCompose) Down(context.Context, model.ComposeProject) error         { return nil }

// Compose: 화면에서 만들고, 주소는 web:3000으로만 나간다. 다른 컨테이너 목록에는 Naru가 띄운 compose가 안 보인다.
func TestComposeFromTheScreen(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{compose: true})
	h.signedIn()
	_, _, page := h.get("/services/new?kind=compose")
	if !strings.Contains(page, `name="compose_project"`) || !strings.Contains(page, `name="target_service"`) {
		t.Fatal("the compose form")
	}
	code, loc, body := h.post("/services/new", url.Values{"kind": {"compose"}, "source": {"https://github.com/me/shop"},
		"domain": {"shop.example.com"}, "target_service": {"web"}, "target_port": {"3000"}, "compose_project": {"shop"},
		"env_file": {"DB_PASSWORD=pw"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/services/1/deploys/") {
		t.Fatalf("%d %q %s", code, loc, body)
	}
	s := h.service(1)
	if s.Kind != model.KindCompose || s.Project() != "shop" || s.Domains[0].Target != (model.DomainTarget{Service: "web", Port: 3000}) {
		t.Fatalf("%+v", s)
	}
	h.waitFor("the address goes to web:3000 only", func(cfg string) bool {
		return strings.Contains(cfg, "shop.example.com") && strings.Contains(cfg, "shop-web-1:3000")
	})
	if strings.Contains(h.caddy.last(), "shop-worker-1") {
		t.Fatal("the worker is not public")
	}
	if j := strings.Join(h.docker.joined(), ","); !strings.Contains(j, "naru-net<-shop-web-1") || strings.Contains(j, "worker") {
		t.Fatalf("only the addressed service joins the app network: %s", j)
	}
	if b, err := os.ReadFile(filepath.Join(h.app.cfg.HostData, "compose", "shop", ".env")); err != nil || string(b) != "DB_PASSWORD=pw\n" {
		t.Fatalf(".env in the work folder: %q %v", b, err)
	}

	_, _, page = h.get("/services/1")
	for _, want := range []string{"compose가 띄운 것", `<span class="mono">worker</span>`, "끝남 (0)", `action="/services/1/domains/`, "저장소에서 온 파일만 지워요"} {
		if !strings.Contains(page, want) {
			t.Errorf("service page shows %q", want)
		}
	}
	deps, _ := h.history().Recent(ctx, 1, 1)
	dep, _ := h.history().Get(ctx, deps[0].ID)
	if dep.Status != model.DeploySuccess || strings.Contains(dep.Log, "DB_PASSWORD") || !strings.Contains(dep.Log, ".env 파일 1줄") {
		t.Fatalf("the deploy log counts the .env lines, never shows them:\n%s", dep.Log)
	}
	_, _, home := h.get("/")
	if strings.Contains(home, "shop-worker-1") {
		t.Fatal("a compose project Naru runs is not an 'other container'")
	}

	// 주소의 대상을 바꾼다
	code, loc, _ = h.post(fmt.Sprintf("/services/1/domains/%d/target", s.Domains[0].ID), url.Values{"target_service": {"web"}, "target_port": {"3001"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "ok=target") {
		t.Fatalf("%d %q", code, loc)
	}
	h.waitFor("the new target port", func(cfg string) bool { return strings.Contains(cfg, "shop-web-1:3001") })
	code, _, body = h.post(fmt.Sprintf("/services/1/domains/%d/target", s.Domains[0].ID), url.Values{"target_service": {"web"}, "target_port": {""}})
	if code != http.StatusBadRequest || !strings.Contains(body, "compose의 서비스 이름") {
		t.Fatalf("%d", code)
	}
}

// NARU_HOST_DATA가 없으면 Compose는 만들 수 없다 — compose의 상대 경로를 서버에서 찾지 못한다.
func TestComposeNeedsTheHostDataPath(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	code, _, body := h.post("/services/new", url.Values{"kind": {"compose"}, "source": {"https://github.com/me/shop"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "NARU_HOST_DATA") {
		t.Fatalf("%d", code)
	}
}

// 여러 개 지우기는 확인 화면을 거친다 — 글자가 맞아야, 돌고 있는 것은 고를 때만, 보호된 것은 건너뛴다.
func TestRemovingSeveralGoesThroughAConfirmPage(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.docker.set("old-api", model.ContainerState{Name: "old-api", State: "exited", Status: "Exited (1) 5 days ago"})
	h.docker.set("test-redis", model.ContainerState{Name: "test-redis", Running: true, State: "running", Status: "Up 1 hour"})
	h.docker.set("naru-caddy", model.ContainerState{Name: "naru-caddy", Running: true, State: "running", Status: "Up 1 day"})
	pick := url.Values{"action": {"remove"}, "c": {"old-api", "test-redis", "naru-caddy"}}

	code, _, page := h.post("/containers", pick)
	for _, want := range []string{"컨테이너 2개를 지울까요?", "웹서버라 건너뛰어요", "돌고 있어요", "돌고 있는 1개는 멈추고 지우기", "이름 없는 볼륨도 같이 지우기",
		`name="c" value="old-api"`, `name="c" value="test-redis"`, "확인을 위해 지우기를 입력하세요"} {
		if !strings.Contains(page, want) {
			t.Errorf("confirm page is missing %q", want)
		}
	}
	if code != http.StatusOK || strings.Contains(page, `name="c" value="naru-caddy"`) {
		t.Fatalf("%d — the web server is not carried on", code)
	}
	if code, _, page := h.post("/containers/remove", url.Values{"c": {"old-api"}, "confirm": {"지우"}}); code != http.StatusBadRequest || !strings.Contains(page, "확인 글자가 맞지 않아요") {
		t.Fatalf("%d", code)
	}
	if _, loc, _ := h.post("/containers/remove", url.Values{"c": {"old-api", "test-redis", "naru-caddy"}, "confirm": {"지우기"}}); loc != "/containers?ok=removed-some" {
		t.Fatalf("the running one waits, the web server is skipped: %s", loc)
	}
	if _, loc, _ := h.post("/containers/remove", url.Values{"c": {"test-redis"}, "confirm": {"delete"}, "stop": {"1"}, "volumes": {"1"}}); loc != "/containers?ok=removed-many" {
		t.Fatalf("%s", loc)
	}
	h.docker.mu.Lock()
	acts := strings.Join(h.docker.acts, ",")
	h.docker.mu.Unlock()
	if acts != "remove old-api,stop test-redis,remove test-redis +volumes" {
		t.Fatalf("%s", acts)
	}
	if _, _, settings := h.get("/settings"); !strings.Contains(settings, "old-api, test-redis, naru-caddy 컨테이너를 지웠어요") {
		t.Fatal("the activity log names what was picked")
	}
}

func TestNetworksPageAndAttaching(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	h.docker.set("cairn-web-1", model.ContainerState{Name: "cairn-web-1", Running: true, State: "running", Project: "cairn"})
	h.docker.set("naru-caddy", model.ContainerState{Name: "naru-caddy", Running: true, State: "running"})

	_, _, page := h.get("/networks")
	for _, want := range []string{"Docker 기본", "Naru 앱 네트워크", "cairn_default", "cairn 묶음", `href="/containers/cairn-web-1"`, `aria-current="page">`} {
		if !strings.Contains(page, want) {
			t.Errorf("networks page is missing %q", want)
		}
	}
	if code, _, page := h.post("/networks", url.Values{"name": {"host"}}); code != http.StatusBadRequest || !strings.Contains(page, "bridge·host·none은 쓸 수 없어요") {
		t.Fatalf("%d", code)
	}
	if _, loc, _ := h.post("/networks", url.Values{"name": {"db-only"}, "internal": {"1"}}); loc != "/networks?ok=network-created" {
		t.Fatalf("%s", loc)
	}
	if code, _, page := h.post("/networks", url.Values{"name": {"db-only"}}); code != http.StatusBadRequest || !strings.Contains(page, "이미 있어요") {
		t.Fatalf("%d", code)
	}
	if _, loc, _ := h.post("/networks/naru-net/delete", url.Values{"confirm": {"naru-net"}}); !strings.Contains(loc, "err=protected") {
		t.Fatalf("the app network stays: %s", loc)
	}
	if _, loc, _ := h.post("/networks/cairn_default/delete", url.Values{"confirm": {"cairn_default"}}); !strings.Contains(loc, "err=inuse") {
		t.Fatalf("%s", loc)
	}

	// 상세에서 붙이기·떼기
	_, _, detail := h.get("/containers/cairn-web-1")
	if !strings.Contains(detail, `<option value="db-only">`) || !strings.Contains(detail, `action="/containers/cairn-web-1/networks"`) {
		t.Fatal("the detail offers networks to attach")
	}
	if _, loc, _ := h.post("/containers/cairn-web-1/networks", url.Values{"network": {"db-only"}, "alias": {"api"}}); !strings.Contains(loc, "ok=attached") {
		t.Fatalf("%s", loc)
	}
	if _, loc, _ := h.post("/containers/cairn-web-1/networks", url.Values{"network": {"db-only"}, "alias": {"bad alias"}}); !strings.Contains(loc, "err=alias") {
		t.Fatalf("%s", loc)
	}
	if _, loc, _ := h.post("/containers/naru-caddy/networks", url.Values{"network": {"db-only"}}); !strings.Contains(loc, "err=protected") {
		t.Fatalf("%s", loc)
	}
	if !strings.Contains(strings.Join(h.docker.joined(), ","), "db-only<-cairn-web-1@api") {
		t.Fatal("joined with the alias")
	}
	if _, loc, _ := h.post("/networks/db-only/delete", url.Values{"confirm": {"db-only"}}); loc != "/networks?ok=network-removed" {
		t.Fatalf("%s", loc)
	}
	_, _, settings := h.get("/settings")
	for _, want := range []string{"db-only 네트워크를 만들었어요", "cairn-web-1 → db-only 네트워크에 붙였어요", "db-only 네트워크를 지웠어요"} {
		if !strings.Contains(settings, want) {
			t.Errorf("activity log is missing %q", want)
		}
	}
}

// 사이드바와 테마 — 고른 테마는 쿠키로 기억하고 html에 처음부터 적는다 (깜빡이지 않게). 자동이면 시스템 설정.
func TestSidebarAndTheme(t *testing.T) {
	h := newHarness(t, "")
	h.signedIn()
	_, _, page := h.get("/containers")
	for _, want := range []string{`class="side"`, `<span class="wm-name">나루</span>`, `href="/networks"`, `href="/storage"`, `href="/containers" aria-current="page"`,
		`href="/theme/dark?back=%2fcontainers"`, `<title>컨테이너 · 나루</title>`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page, "data-theme=") {
		t.Fatal("auto by default — no theme attribute")
	}
	if code, loc, _ := h.get("/theme/dark?back=/server"); code != http.StatusSeeOther || loc != "/server" {
		t.Fatalf("%d %s", code, loc)
	}
	if _, _, page := h.get("/server"); !strings.Contains(page, `data-theme="dark"`) || !strings.Contains(page, `href="/theme/dark?back=%2fserver" aria-current="true"`) {
		t.Fatal("dark is remembered")
	}
	if _, loc, _ := h.get("/theme/light?back=//evil.example"); loc != "/" {
		t.Fatalf("back stays on this site: %s", loc)
	}
	h.get("/theme/auto?back=/")
	if _, _, page := h.get("/"); strings.Contains(page, "data-theme=") {
		t.Fatal("auto clears it")
	}
	if _, _, page := h.get("/lang/en?back=/"); true {
		_ = page
	}
	if _, _, page := h.get("/"); !strings.Contains(page, `<span class="wm-name">Naru</span>`) {
		t.Fatal("the English screens say Naru")
	}
}
