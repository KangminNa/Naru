package deploy

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/events"
	"github.com/KangminNa/naru/internal/files"
	"github.com/KangminNa/naru/internal/kinds"
	"github.com/KangminNa/naru/internal/model"
	"github.com/KangminNa/naru/internal/store"
)

var ctx = context.Background()

// fakeDocker는 메모리 안의 Docker다. 컨테이너가 "듣는지"는 listens로 정한다.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	images     map[string]model.ImageDetails // ref 또는 ID → 이미지
	builds     []string
	packed     map[string]string // 마지막 빌드 묶음의 이름 → 내용
	removedImg []string
	listens    func(name string) bool // nil이면 모두 듣는다
	exits      func(name string) bool // 시작하자마자 죽는가
	loops      func(name string) bool // 죽고 Docker가 계속 다시 띄우는가 (재시작 정책)
	target     string                 // 마지막 빌드의 타깃
	joined     []string               // "네트워크 컨테이너"
}

func (f *fakeDocker) NetworksOf(context.Context, string) ([]model.DockerNetwork, error) {
	return nil, nil
}
func (f *fakeDocker) Leave(context.Context, string, string) error { return nil }
func (f *fakeDocker) Join(_ context.Context, network, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[name]; !ok {
		return errors.New("no such container")
	}
	f.joined = append(f.joined, network+" "+name)
	return nil
}

type fakeContainer struct {
	spec    model.ContainerSpec
	running bool
	exited  bool
	// compose가 띄운 것만
	project, service, image string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]*fakeContainer{}, images: map[string]model.ImageDetails{}}
}

func (f *fakeDocker) Build(_ context.Context, folder io.Reader, tag, target string, log io.Writer) (model.ImageID, error) {
	f.mu.Lock()
	f.target = target
	f.mu.Unlock()
	packed := map[string]string{}
	tr := tar.NewReader(folder)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		packed[h.Name] = string(body)
	}
	io.Copy(io.Discard, folder)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.packed = packed
	id := "sha256:built-" + tag
	f.builds = append(f.builds, tag)
	f.images[id] = model.ImageDetails{ID: model.ImageID(id)}
	fmt.Fprintln(log, "Successfully built")
	return model.ImageID(id), nil
}

func (f *fakeDocker) Pull(_ context.Context, ref model.ImageRef, _ io.Writer) (model.ImageDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.images[ref.String()]
	if !ok {
		return img, errors.New("pull access denied")
	}
	f.images[string(img.ID)] = img
	return img, nil
}

func (f *fakeDocker) Exists(_ context.Context, id model.ImageID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.images[string(id)]
	return ok
}

func (f *fakeDocker) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(name, ":") {
		f.removedImg = append(f.removedImg, name)
		return nil
	}
	delete(f.containers, name)
	return nil
}

func (f *fakeDocker) Start(_ context.Context, s model.ContainerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[s.Name]; ok {
		return errors.New("name in use")
	}
	c := &fakeContainer{spec: s, running: true}
	if f.exits != nil && f.exits(s.Name) {
		c.running, c.exited = false, true
	}
	f.containers[s.Name] = c
	return nil
}

func (f *fakeDocker) TurnOff(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		c.running = false
	}
	return nil
}

func (f *fakeDocker) TurnOn(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		c.running = true
	}
	return nil
}

// All은 compose가 띄운 컨테이너만 보여 준다 (다른 테스트는 All을 쓰지 않는다).
func (f *fakeDocker) All(context.Context) (model.ContainerStates, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := model.ContainerStates{}
	for n, c := range f.containers {
		if c.project != "" {
			out[n] = model.ContainerState{Name: n, Running: c.running, State: map[bool]string{true: "running", false: "exited"}[c.running],
				Project: c.project, ComposeService: c.service, ImageID: c.image}
		}
	}
	return out, nil
}

func (f *fakeDocker) One(_ context.Context, name string) (model.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return model.ContainerState{}, errors.New("no such container")
	}
	if c.exited {
		return model.ContainerState{Name: name, State: "exited", ExitCode: 1}, nil
	}
	if f.loops != nil && f.loops(name) {
		return model.ContainerState{Name: name, Running: true, State: "running", Restarts: 3}, nil
	}
	return model.ContainerState{Name: name, Running: c.running, State: map[bool]string{true: "running", false: "created"}[c.running]}, nil
}

func (f *fakeDocker) Logs(context.Context, string, int) (string, error) {
	return "Error: Cannot find module 'express'\n", nil
}

// Recent는 그 컨테이너가 찍은 줄인 척한다 (ContainerLogReader).
func (f *fakeDocker) Recent(_ context.Context, name string, n int) ([]model.LogLine, error) {
	return []model.LogLine{{At: time.Unix(1795000000, 0), Source: model.LogApp, Stream: "stdout", Text: name + " says hi"}}, nil
}

func (f *fakeDocker) BelongingTo(_ context.Context, id model.ServiceID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, c := range f.containers {
		if c.spec.Service == id {
			out = append(out, n)
		}
	}
	return out, nil
}

// Answers는 컨테이너가 실행 중이고 듣고 있으면 성공한다. 응답 확인은 컨테이너 이름으로 온다.
func (f *fakeDocker) Answers(_ context.Context, name string, _ model.Port) error {
	f.mu.Lock()
	c := f.containers[name]
	listens := f.listens
	f.mu.Unlock()
	if c == nil || !c.running || (listens != nil && !listens(name)) {
		return errors.New("connection refused")
	}
	return nil
}

func (f *fakeDocker) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.containers {
		out = append(out, n)
	}
	return out
}

// fakeSync는 웹서버 맞추기다. 맞출 때 어떤 컨테이너가 살아 있었는지 기록한다 — 옛 것이 그때까지 살아 있어야 한다.
type fakeSync struct {
	docker *fakeDocker
	fail   error
	seen   [][]string
	on     func() // 맞출 때마다 부른다 — 그 순간의 상태를 볼 때
}

func (s *fakeSync) SyncNow(context.Context) error {
	s.seen = append(s.seen, s.docker.names())
	if s.on != nil {
		s.on()
	}
	return s.fail
}
func (s *fakeSync) Status() model.WebServerStatus { return model.WebServerStatus{} }

// fakeGit은 정해 둔 파일을 "내려받는다".
type fakeGit struct {
	files  map[string]string
	commit model.Commit
	asked  []string // 정해서 받은 커밋 (되돌리기)
}

func (g *fakeGit) Download(_ context.Context, from model.CodeSource, into string, _ io.Writer) (model.Commit, error) {
	if from.Commit != "" {
		g.asked = append(g.asked, from.Commit)
		os.MkdirAll(into, 0o755)
		return model.Commit{Hash: from.Commit, Message: "old"}, nil
	}
	os.MkdirAll(into, 0o755)
	for name, body := range g.files {
		p := filepath.Join(into, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	return g.commit, nil
}

type rig struct {
	t        *testing.T
	d        contract.Deployer
	wait     func()
	control  contract.ServiceControl
	docker   *fakeDocker
	git      *fakeGit
	db       *store.DB
	services store.Services
	history  store.Deployments
	sites    string
	changes  int
	sync     *fakeSync
	logs     files.LogArchives
	compose  *fakeCompose
	projects files.ProjectFolderSet
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "naru.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := &rig{t: t, docker: newFakeDocker(), db: db, services: store.NewServices(db), history: store.NewDeployments(db),
		sites: filepath.Join(dir, "sites"), git: &fakeGit{files: map[string]string{}, commit: model.Commit{Hash: "a1b2c3d4e5f6", Message: "fix: things"}}}
	bus := events.NewBus()
	bus.Subscribe(func(e model.Event) {
		if _, ok := e.(model.SiteMapChanged); ok {
			r.changes++
		}
	})
	r.sync = &fakeSync{docker: r.docker}
	siteFiles := files.NewSiteFolders(r.sites)
	r.logs = files.NewLogArchives(filepath.Join(dir, "logs"))
	secrets := store.NewSecrets(db)
	r.projects = files.NewProjectFolders(filepath.Join(dir, "compose"))
	r.compose = &fakeCompose{docker: r.docker, projects: r.projects}
	lookup := kinds.NewLookup(kinds.Tools{
		Compose: r.compose, Joiner: r.docker, Projects: r.projects, ComposeOn: true, ComposeTimeout: 300 * time.Millisecond,
		Work: files.NewTempFolders(filepath.Join(dir, "work")), Code: r.git, Secrets: secrets,
		Dockerfile: files.DockerfileReader{}, Packer: files.TarPacker{}, Builder: r.docker, Puller: r.docker, Images: r.docker,
		Starter: r.docker, Remover: r.docker, Switch: r.docker, Watcher: r.docker, Ports: r.docker, Files: siteFiles,
		AppLogs: r.docker, Archive: r.logs,
		Network: "naru-net", ReadyTimeout: 300 * time.Millisecond, PollEvery: 10 * time.Millisecond,
	})
	lock := NewMemoryDeployLock()
	r.d, r.wait = NewDeployer(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), Parts{
		Lock: lock, History: r.history, Past: r.history, Logs: NewDeployLog(r.history), Services: r.services,
		Live: store.NewLiveStates(db), Kinds: lookup, Cleaner: NewOldVersionCleaner(r.history, r.docker, siteFiles), Events: bus, Sync: r.sync,
		Mover: store.NewDomainMoves(db),
	})
	r.control = NewServiceControl(ControlParts{
		Services: r.services, Store: r.services, Live: store.NewLiveStates(db), Lock: lock, Switch: r.docker, Remover: r.docker,
		Watcher: r.docker, Images: r.docker, Files: siteFiles, Past: r.history, Events: bus, Logs: r.logs, Kinds: lookup,
	})
	return r
}

func (r *rig) create(s model.NewService, env string) model.ServiceID {
	r.t.Helper()
	id, err := r.services.Create(ctx, s)
	if err != nil {
		r.t.Fatal(err)
	}
	e, _, err := model.ParseEnvVars(env)
	if err != nil {
		r.t.Fatal(err)
	}
	store.NewSecrets(r.db).Set(ctx, id, model.ServiceSecrets{Env: e})
	return id
}

func (r *rig) deploy(id model.ServiceID) model.Deployment {
	r.t.Helper()
	did, err := r.d.Deploy(ctx, id, model.ReasonManual)
	if err != nil {
		r.t.Fatal(err)
	}
	r.wait()
	dep, _ := r.history.Get(ctx, did)
	return dep
}

func (r *rig) get(id model.ServiceID) model.Service {
	s, _ := r.services.Get(ctx, id)
	return s
}

func name(s string) model.ServiceName {
	n, _ := model.ParseServiceName(s)
	return n
}

func TestImageDeployDetectsThePortAndJoinsTheAlias(t *testing.T) {
	r := newRig(t)
	r.docker.images["traefik/whoami"] = model.ImageDetails{ID: "sha256:who", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("whoami"), Kind: model.KindImage, Source: "traefik/whoami"}, "GREETING=hi\n# note\n")

	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess || dep.Image != "sha256:who" {
		t.Fatalf("%+v\n%s", dep, dep.Log)
	}
	s := r.get(id)
	if s.Port != 80 || s.Live.Alias != "naru-whoami" || s.Live.Instance != fmt.Sprintf("naru-whoami-%d", dep.ID) {
		t.Fatalf("%+v", s)
	}
	c := r.docker.containers[s.Live.Instance]
	if c.spec.Aliases[0] != "naru-whoami" || c.spec.Network != "naru-net" || c.spec.Service != id {
		t.Fatalf("the new container answers to the service name: %+v", c.spec)
	}
	if len(c.spec.Env) != 1 || c.spec.Env[0] != "GREETING=hi" {
		t.Fatalf("env: %v", c.spec.Env)
	}
	if r.changes == 0 {
		t.Fatal("the web server is told")
	}
}

func TestRedeployReplacesOnlyAfterTheNewOneAnswers(t *testing.T) {
	r := newRig(t)
	r.docker.images["traefik/whoami"] = model.ImageDetails{ID: "sha256:who", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("whoami"), Kind: model.KindImage, Source: "traefik/whoami"}, "")
	first := r.deploy(id)
	second := r.deploy(id)
	if second.Status != model.DeploySuccess {
		t.Fatal(second.Log)
	}
	names := r.docker.names()
	if len(names) != 1 || names[0] != fmt.Sprintf("naru-whoami-%d", second.ID) {
		t.Fatalf("only the new container remains: %v (first was %d)", names, first.ID)
	}
	// 내린 옛 컨테이너의 앱 출력은 남는다 — Docker는 컨테이너와 함께 지운다
	kept, err := r.logs.Read(name("whoami"), first.ID)
	if err != nil || len(kept) != 1 || kept[0].Text != fmt.Sprintf("naru-whoami-%d says hi", first.ID) {
		t.Fatalf("%+v %v", kept, err)
	}
	if !strings.Contains(second.Log, "앱 출력 1줄을 보관했어요") {
		t.Fatalf("%s", second.Log)
	}
}

func TestAFailingNewVersionLeavesTheOldOneServing(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v1", Ports: []model.Port{3000}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	good := r.deploy(id)
	oldName := r.get(id).Live.Instance

	r.docker.exits = func(name string) bool { return name != oldName }
	bad := r.deploy(id)
	if bad.Status != model.DeployFailed || !strings.Contains(bad.Log, "Cannot find module") || !strings.Contains(bad.Log, "종료 코드 1") {
		t.Fatalf("a crashing container fails the deploy and shows its output:\n%s", bad.Log)
	}
	names := r.docker.names()
	if len(names) != 1 || names[0] != oldName || r.get(id).Live.Instance != oldName {
		t.Fatalf("the old version keeps serving: %v (good #%d)", names, good.ID)
	}

	// 죽고 Docker가 다시 띄우는 앱은 시간 초과까지 기다리지 않는다
	r.docker.exits = nil
	r.docker.loops = func(name string) bool { return name != oldName }
	r.docker.listens = func(name string) bool { return name == oldName }
	start := time.Now()
	looping := r.deploy(id)
	if looping.Status != model.DeployFailed || !strings.Contains(looping.Log, "앱이 시작하자마자 죽어요 — Docker가 3번 다시 띄웠어요") ||
		strings.Contains(looping.Log, "다른 포트를 듣고") || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("a crash loop fails at once, without the wrong-port hint (%s):\n%s", time.Since(start), looping.Log)
	}
	r.docker.loops = nil
	silent := r.deploy(id)
	if silent.Status != model.DeployFailed || !strings.Contains(silent.Log, "포트 3000") {
		t.Fatalf("a container that never answers fails too:\n%s", silent.Log)
	}
	if r.get(id).Live.Instance != oldName {
		t.Fatal("still the old one")
	}
}

func TestAdoptingAV1Container(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/blog"] = model.ImageDetails{ID: "sha256:b", Ports: []model.Port{3000}}
	r.docker.containers["shelf-blog"] = &fakeContainer{running: true} // v1이 띄운 것, 라벨 없음
	id := r.create(model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "shelf-blog", Port: 3000}, "")

	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess {
		t.Fatal(dep.Log)
	}
	s := r.get(id)
	if s.Live.Alias != "shelf-blog" || s.Live.Instance != fmt.Sprintf("shelf-blog-%d", dep.ID) {
		t.Fatalf("the service keeps answering as shelf-blog: %+v", s)
	}
	if _, ok := r.docker.containers["shelf-blog"]; ok {
		t.Fatal("the v1 container is retired once the new one answers")
	}
	if r.docker.containers[s.Live.Instance].spec.Aliases[0] != "shelf-blog" {
		t.Fatal("alias keeps the web server's target unchanged")
	}
}

func TestRepoDeployBuildsWithThePortFromTheDockerfile(t *testing.T) {
	r := newRig(t)
	r.git.files = map[string]string{"web/Dockerfile": "FROM node:20\nEXPOSE 3000\n", "web/server.js": "x"}
	id := r.create(model.NewService{Name: name("blog"), Kind: model.KindRepo, Source: "https://github.com/me/blog", Branch: "main", BuildPath: "web"}, "")
	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess || dep.Commit != "a1b2c3d4e5f6" || dep.Message != "fix: things" {
		t.Fatalf("%+v\n%s", dep, dep.Log)
	}
	if len(r.docker.builds) != 1 || r.docker.builds[0] != model.ImageTag(name("blog"), dep.ID) || r.get(id).Port != 3000 {
		t.Fatalf("builds=%v port=%d", r.docker.builds, r.get(id).Port)
	}

	r.git.files = map[string]string{"README.md": "no dockerfile here"}
	id2 := r.create(model.NewService{Name: name("plain"), Kind: model.KindRepo, Source: "https://github.com/me/plain"}, "")
	if dep := r.deploy(id2); dep.Status != model.DeployFailed || !strings.Contains(dep.Log, "정적 사이트") {
		t.Fatalf("a repo without Dockerfile points to static sites:\n%s", dep.Log)
	}
}

// 저장소의 .env는 보통 git에 없다 — .env 파일 칸의 내용이 빌드 폴더의 .env가 된다. 기록에는 줄 수만.
func TestRepoBuildGetsTheEnvFile(t *testing.T) {
	r := newRig(t)
	r.git.files = map[string]string{"web/Dockerfile": "FROM node:20\nCOPY . .\nEXPOSE 3000\n", "web/.dockerignore": ".env\n", "web/.env": "FROM_REPO=1\n"}
	id := r.create(model.NewService{Name: name("blog"), Kind: model.KindRepo, Source: "https://github.com/me/blog", BuildPath: "web"}, "")
	envFile, _ := model.ParseEnvFile("NEXT_PUBLIC_API=https://api.example.com\nSECRET_KEY=s3cr3t")
	sec, _ := store.NewSecrets(r.db).Get(ctx, id)
	sec.EnvFile = envFile
	store.NewSecrets(r.db).Set(ctx, id, sec)

	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess {
		t.Fatalf("%+v\n%s", dep, dep.Log)
	}
	if got := r.docker.packed[".env"]; got != "NEXT_PUBLIC_API=https://api.example.com\nSECRET_KEY=s3cr3t\n" {
		t.Fatalf("the build folder's .env is the one from Naru: %q", got)
	}
	if !strings.Contains(dep.Log, ".env 파일 2줄") || strings.Contains(dep.Log, "s3cr3t") {
		t.Fatalf("the log says how many lines, never what:\n%s", dep.Log)
	}

	// 비어 있으면 저장소 그대로 (.dockerignore가 뺀 것은 빠진다)
	sec.EnvFile = model.EnvFile{}
	store.NewSecrets(r.db).Set(ctx, id, sec)
	if dep := r.deploy(id); dep.Status != model.DeploySuccess || strings.Contains(dep.Log, ".env 파일") {
		t.Fatalf("%s", dep.Log)
	}
	if _, ok := r.docker.packed[".env"]; ok {
		t.Fatal("without the box the repo is packed as it is")
	}
}

// Dockerfile에 앱이 여럿 — 타깃이 없으면 맨 마지막을 빌드하며 다른 단계를 알려 주고, 있으면 그것을, 없는 이름이면 빌드 전에 실패.
func TestBuildTarget(t *testing.T) {
	r := newRig(t)
	r.git.files = map[string]string{"Dockerfile": "FROM node:24 AS deps\nFROM deps AS web\nEXPOSE 3000\nFROM node:24-slim AS egress\nEXPOSE 3128\n"}
	id := r.create(model.NewService{Name: name("shop"), Kind: model.KindRepo, Source: "https://github.com/me/shop"}, "")
	dep := r.deploy(id)
	if r.docker.target != "" || !strings.Contains(dep.Log, "Dockerfile에 단계가 3개예요 — 맨 마지막(egress)을 빌드해요") || !strings.Contains(dep.Log, "deps, web, egress") || r.get(id).Port != 3128 {
		t.Fatalf("%q port=%d\n%s", r.docker.target, r.get(id).Port, dep.Log)
	}

	web := r.create(model.NewService{Name: name("shop-web"), Kind: model.KindRepo, Source: "https://github.com/me/shop", BuildTarget: "web"}, "")
	if dep := r.deploy(web); dep.Status != model.DeploySuccess || r.docker.target != "web" || r.get(web).Port != 3000 || !strings.Contains(dep.Log, "빌드 타깃: web") {
		t.Fatalf("%q port=%d\n%s", r.docker.target, r.get(web).Port, dep.Log)
	}

	typo := r.create(model.NewService{Name: name("shop-typo"), Kind: model.KindRepo, Source: "https://github.com/me/shop", BuildTarget: "wbe"}, "")
	builds := len(r.docker.builds)
	if dep := r.deploy(typo); dep.Status != model.DeployFailed || !strings.Contains(dep.Log, `빌드 타깃 "wbe"가 Dockerfile에 없어요 — 있는 단계: deps, web, egress`) || len(r.docker.builds) != builds {
		t.Fatalf("%s", dep.Log)
	}
}

func TestBuildImagesArePruned(t *testing.T) {
	r := newRig(t)
	r.git.files = map[string]string{"Dockerfile": "FROM x\nEXPOSE 80\n"}
	id := r.create(model.NewService{Name: name("blog"), Kind: model.KindRepo, Source: "https://github.com/me/blog"}, "")
	var deps []model.Deployment
	for i := 0; i < 5; i++ {
		deps = append(deps, r.deploy(id))
	}
	removed := strings.Join(r.docker.removedImg, " ")
	tag := func(i int) string { return model.ImageTag(name("blog"), deps[i].ID) + " " }
	removed += " "
	if !strings.Contains(removed, tag(0)) || !strings.Contains(removed, tag(1)) || strings.Contains(removed, tag(2)) || strings.Contains(removed, tag(4)) {
		t.Fatalf("keep the last %d builds for rollback, drop older: %v", keepImages, r.docker.removedImg)
	}
}

func TestRollbackReusesTheOldImage(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v1", Ports: []model.Port{3000}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	v1 := r.deploy(id)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v2", Ports: []model.Port{3000}}
	r.deploy(id)

	did, err := r.d.RollBack(ctx, id, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.wait()
	back, _ := r.history.Get(ctx, did)
	if back.Status != model.DeploySuccess || back.Image != "sha256:v1" || back.Reason != model.ReasonRollBack {
		t.Fatalf("%+v\n%s", back, back.Log)
	}
	if r.docker.containers[r.get(id).Live.Instance].spec.Image != "sha256:v1" {
		t.Fatal("the restored container runs the old image")
	}
	if _, err := r.d.RollBack(ctx, id, 9999); !errors.Is(err, model.ErrCannotRollBack) {
		t.Fatal("unknown deployments cannot be restored")
	}

	delete(r.docker.images, "sha256:v1")
	did, _ = r.d.RollBack(ctx, id, v1.ID)
	r.wait()
	if gone, _ := r.history.Get(ctx, did); gone.Status != model.DeployFailed || !strings.Contains(gone.Log, "이미지가 지워졌어요") {
		t.Fatalf("a deleted image cannot come back:\n%s", gone.Log)
	}
}

func TestStaticSitePublishesFilesWithoutAContainer(t *testing.T) {
	r := newRig(t)
	r.git.files = map[string]string{"site/index.html": "<h1>hi</h1>", "site/app.css": "body{}", "README.md": "x"}
	id := r.create(model.NewService{Name: name("landing"), Kind: model.KindStatic, Source: "https://github.com/me/landing", Folder: "site"}, "")
	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess {
		t.Fatal(dep.Log)
	}
	s := r.get(id)
	if s.Live.Release != fmt.Sprint(dep.ID) {
		t.Fatalf("release %q", s.Live.Release)
	}
	if _, err := os.Stat(filepath.Join(r.sites, "landing", s.Live.Release, "index.html")); err != nil {
		t.Fatal("files are published")
	}
	if _, err := os.Stat(filepath.Join(r.sites, "landing", s.Live.Release, "README.md")); err == nil {
		t.Fatal("only the chosen folder is published")
	}
	if len(r.docker.names()) != 0 || r.changes == 0 {
		t.Fatal("no container; the web server is told")
	}

	r.git.files = map[string]string{"site/about.html": "no index"}
	if bad := r.deploy(id); bad.Status != model.DeployFailed || !strings.Contains(bad.Log, "index.html") {
		t.Fatalf("a folder without index.html is refused:\n%s", bad.Log)
	}
	if r.get(id).Live.Release != s.Live.Release {
		t.Fatal("a failed deploy keeps the live files")
	}

	did, _ := r.d.RollBack(ctx, id, dep.ID)
	r.wait()
	back, _ := r.history.Get(ctx, did)
	if back.Status != model.DeploySuccess || r.get(id).Live.Release != fmt.Sprint(did) || back.Commit != "a1b2c3d4e5f6" {
		t.Fatalf("static rollback publishes the old files as a new release: %+v\n%s", back, back.Log)
	}
	if body, _ := os.ReadFile(filepath.Join(r.sites, "landing", fmt.Sprint(did), "index.html")); string(body) != "<h1>hi</h1>" {
		t.Fatal("the old files are what is live")
	}
	// 되돌린 배포에서 다시 되돌릴 수도 있다
	again, _ := r.d.RollBack(ctx, id, did)
	r.wait()
	if d, _ := r.history.Get(ctx, again); d.Status != model.DeploySuccess {
		t.Fatal("a restored release can be restored again")
	}
}

func TestADeployWhileBusyRunsOnceMoreAfterwards(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	gate := make(chan struct{})
	r.docker.listens = func(string) bool {
		<-gate
		return true
	}
	if _, err := r.d.Deploy(ctx, id, model.ReasonPush); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.d.Deploy(ctx, id, model.ReasonPush); !errors.Is(err, model.ErrQueued) {
			t.Fatalf("got %v", err)
		}
	}
	if !r.d.IsDeploying(id) {
		t.Fatal("deploying")
	}
	if _, err := r.d.RollBack(ctx, id, 1); !errors.Is(err, model.ErrCannotRollBack) && !errors.Is(err, model.ErrBusy) {
		t.Fatalf("got %v", err)
	}
	if err := r.control.Remove(ctx, id); !errors.Is(err, model.ErrBusy) {
		t.Fatal("a service is not removed while deploying")
	}
	close(gate)
	r.wait()
	all, _ := r.history.Recent(ctx, id, 10)
	if len(all) != 2 {
		t.Fatalf("pushes during a deploy collapse into one more deploy, got %d deploys", len(all))
	}
}

func TestExternalServicesHaveNothingToDeploy(t *testing.T) {
	r := newRig(t)
	id := r.create(model.NewService{Name: name("nas"), Kind: model.KindExternal, External: "host.docker.internal:5000"}, "")
	if _, err := r.d.Deploy(ctx, id, model.ReasonManual); !errors.Is(err, model.ErrNothingToDeploy) {
		t.Fatalf("got %v", err)
	}
}

func TestRemoveDeletesEverything(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	r.deploy(id)
	r.deploy(id)
	if l, _ := r.logs.List(name("app")); len(l) != 1 {
		t.Fatalf("the first container's output was kept: %v", l)
	}
	if err := r.control.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(r.docker.names()) != 0 {
		t.Fatal("containers are removed")
	}
	if _, err := r.services.Get(ctx, id); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("the service is gone")
	}
	if l, _ := r.logs.List(name("app")); len(l) != 0 {
		t.Fatal("and its kept output")
	}
}

func TestStopMarksTheServiceStopped(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	r.deploy(id)
	r.control.Stop(ctx, id)
	if s := r.get(id); !s.Live.Stopped || r.docker.containers[s.Live.Instance].running || s.Port != 80 {
		t.Fatalf("stopped on purpose: %+v", s)
	}
	r.control.Start(ctx, id)
	if r.get(id).Live.Stopped {
		t.Fatal("started again")
	}
}

// 서비스의 "다른 네트워크"는 새 컨테이너에 그대로 간다 — 되돌릴 때도.
func TestTheNewContainerAlsoJoinsTheOtherNetworks(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app", Networks: "cairn_default db-net"}, "")
	if dep := r.deploy(id); dep.Status != model.DeploySuccess {
		t.Fatalf("%s", dep.Log)
	}
	r.docker.mu.Lock()
	spec := r.docker.containers["naru-app-1"].spec
	r.docker.mu.Unlock()
	if strings.Join(spec.ExtraNetworks, " ") != "cairn_default db-net" || spec.Network != "naru-net" {
		t.Fatalf("%+v", spec)
	}
}

// 넘겨받기: 새 것이 응답한 뒤에만 옛 서비스의 주소를 옮긴다. 실패한 배포는 아무것도 옮기지 않는다.
func TestAddressesMoveOnlyAfterTheNewServiceAnswers(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	old := r.create(model.NewService{Name: name("cairn"), Kind: model.KindExternal, External: "cairn-web-1:3000"}, "")
	dn, _ := model.ParseDomainName("cairn.example.com")
	store.NewDomains(r.db).Add(ctx, old, model.DomainInput{Domain: dn, HTTPS: true})
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app", TakesOver: old}, "")

	r.docker.listens = func(string) bool { return false } // 새 것이 응답하지 않는다
	if dep := r.deploy(id); dep.Status != model.DeployFailed {
		t.Fatalf("%s", dep.Log)
	}
	if len(r.get(old).Domains) != 1 || len(r.get(id).Domains) != 0 || r.get(id).TakesOver != old {
		t.Fatal("a failed deploy moves nothing and keeps the mark for the next try")
	}

	r.docker.listens = nil
	movedBeforeSync := false
	r.sync.on = func() { movedBeforeSync = len(r.get(id).Domains) == 1 }
	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess || !strings.Contains(dep.Log, "주소 1개와 웹서버 설정을 이 서비스로 옮겼어요") {
		t.Fatalf("%s", dep.Log)
	}
	if len(r.get(old).Domains) != 0 || len(r.get(id).Domains) != 1 || r.get(id).TakesOver != 0 {
		t.Fatalf("old=%v new=%v", r.get(old).Domains, r.get(id).Domains)
	}
	if !movedBeforeSync {
		t.Fatal("the web server is synced after the move, so one sync sends the address to the new service")
	}
	if dep := r.deploy(id); strings.Contains(dep.Log, "옮겼어요") {
		t.Fatal("it moves once")
	}
}

func TestEnvAndVolumes(t *testing.T) {
	if _, _, err := model.ParseEnvVars("A=1\nbad\n"); err == nil {
		t.Fatal("a line without = is refused on input")
	}
	env := model.StoredEnvVars("A=1\n# comment\n\nB = two words\n=nokey\nC=x=y\nbad\n").List()
	if strings.Join(env, "|") != "A=1|B= two words|C=x=y" {
		t.Fatalf("stored text keeps the good lines: %q", env)
	}
	for _, ok := range []string{"/srv/data:/data", "/srv/data:/data:ro"} {
		if _, err := model.ParseVolumes(ok); err != nil {
			t.Errorf("%s", ok)
		}
	}
	for _, bad := range []string{"data:/data", "/a:/b:zz", "/a/../etc:/x", "/a"} {
		if _, err := model.ParseVolumes(bad); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
}

// 웹서버를 맞추기 전에 옛 것을 내리지 않는다 — 맞추기가 실패하면 둘 다 남긴다 (아래 테스트).
func TestTheOldVersionServesUntilTheWebServerPointsAtTheNewOne(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	first := r.deploy(id)
	r.sync.seen = nil
	second := r.deploy(id)
	if second.Status != model.DeploySuccess || len(r.sync.seen) != 1 {
		t.Fatalf("%+v syncs=%d", second, len(r.sync.seen))
	}
	if strings.Join(r.sync.seen[0], " ") == fmt.Sprintf("naru-app-%d", second.ID) {
		t.Fatal("the old container must still be running when the web server is switched")
	}
	if names := r.docker.names(); len(names) != 1 || names[0] != fmt.Sprintf("naru-app-%d", second.ID) {
		t.Fatalf("after the switch the old one is retired: %v (first #%d)", names, first.ID)
	}
}

func TestIfTheWebServerCannotSwitchBothVersionsStay(t *testing.T) {
	r := newRig(t)
	r.docker.images["me/app"] = model.ImageDetails{ID: "sha256:v", Ports: []model.Port{80}}
	id := r.create(model.NewService{Name: name("app"), Kind: model.KindImage, Source: "me/app"}, "")
	r.deploy(id)
	r.sync.fail = errors.New("caddy: dial unix /run/naru/caddy.sock: connect: no such file")
	dep := r.deploy(id)
	if dep.Status != model.DeploySuccess || !strings.Contains(dep.Log, "남겨") {
		t.Fatalf("the new version runs, the log says the old one is kept:\n%s", dep.Log)
	}
	if len(r.docker.names()) != 2 {
		t.Fatalf("both stay until the web server is reachable: %v", r.docker.names())
	}
	r.sync.fail = nil
	third := r.deploy(id)
	if names := r.docker.names(); len(names) != 1 || names[0] != fmt.Sprintf("naru-app-%d", third.ID) {
		t.Fatalf("the next deploy cleans up everything older: %v", names)
	}
}

// fakeCompose는 docker compose다. up은 services를 <프로젝트>-<서비스>-1로 (다시) 띄운다 — 이미지는 빌드마다 바뀐다.
type fakeCompose struct {
	docker   *fakeDocker
	projects files.ProjectFolderSet
	services []string
	builds   int
	calls    []string // "build <폴더의 커밋>", "up", "stop", "start", "down"
}

func (f *fakeCompose) Build(_ context.Context, p model.ComposeProject, log io.Writer) error {
	f.builds++
	body, _ := os.ReadFile(filepath.Join(p.Dir, ".naru-commit"))
	f.calls = append(f.calls, "build "+strings.TrimSpace(string(body)))
	fmt.Fprintln(log, "Built")
	return nil
}

func (f *fakeCompose) Up(_ context.Context, p model.ComposeProject, log io.Writer) error {
	f.calls = append(f.calls, "up")
	f.docker.mu.Lock()
	defer f.docker.mu.Unlock()
	for _, svc := range f.services {
		f.docker.containers[p.Container(svc)] = &fakeContainer{running: true, project: p.Name, service: svc,
			image: fmt.Sprintf("sha256:%s-%d", svc, f.builds)}
	}
	return nil
}

func (f *fakeCompose) Switch(_ context.Context, p model.ComposeProject, on bool) error {
	f.calls = append(f.calls, map[bool]string{true: "start", false: "stop"}[on])
	f.docker.mu.Lock()
	defer f.docker.mu.Unlock()
	for _, c := range f.docker.containers {
		if c.project == p.Name {
			c.running = on
		}
	}
	return nil
}

func (f *fakeCompose) Down(_ context.Context, p model.ComposeProject) error {
	f.calls = append(f.calls, "down")
	f.docker.mu.Lock()
	defer f.docker.mu.Unlock()
	for n, c := range f.docker.containers {
		if c.project == p.Name {
			delete(f.docker.containers, n)
		}
	}
	return nil
}

// Compose: 통째로 빌드하고 띄운 뒤, 주소가 가리키는 web:3000이 응답하면 성공이다. 나머지(worker)는 밖으로 나가지 않는다.
func TestComposeDeploysTheWholeFileAndWaitsForTheAddressedService(t *testing.T) {
	r := newRig(t)
	r.compose.services = []string{"web", "worker"}
	id := r.create(model.NewService{Name: name("shop"), Kind: model.KindCompose, Source: "https://github.com/me/shop", Branch: "main"}, "")
	ef, _ := model.ParseEnvFile("DB_PASSWORD=pw\n")
	store.NewSecrets(r.db).Set(ctx, id, model.ServiceSecrets{EnvFile: ef})
	dn, _ := model.ParseDomainName("shop.example.com")
	store.NewDomains(r.db).Add(ctx, id, model.DomainInput{Domain: dn, HTTPS: true, Target: model.DomainTarget{Service: "web", Port: 3000}})

	first := r.deploy(id)
	if first.Status != model.DeploySuccess || !strings.Contains(first.Log, "web:3000 응답함") || !strings.Contains(first.Log, ".env 파일 1줄") {
		t.Fatalf("%+v\n%s", first, first.Log)
	}
	s := r.get(id)
	if s.Live.Alias != "naru-shop" || s.Live.Instance != "naru-shop-web-1" || s.Live.Port != 3000 {
		t.Fatalf("the addressed service is the face of it: %+v", s.Live)
	}
	dir := r.projects.Dir(s.Name)
	if info, err := os.Stat(filepath.Join(dir, ".env")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf(".env sits in the work folder, owner-only: %v", err)
	}
	if strings.Join(r.compose.calls, ",") != "build a1b2c3d4e5f6,up" {
		t.Fatalf("%v", r.compose.calls)
	}
	if strings.Join(r.docker.joined, ",") != "naru-net naru-shop-web-1" {
		t.Fatalf("only the addressed service joins the app network: %v", r.docker.joined)
	}

	// 다시 배포 — 새 이미지로 바뀐 것의 지난 이미지는 지운다
	r.git.commit = model.Commit{Hash: "b2c3d4e5f6a7", Message: "feat"}
	second := r.deploy(id)
	if second.Status != model.DeploySuccess {
		t.Fatal(second.Log)
	}
	if !slices.Contains(r.docker.removedImg, "sha256:web-1") || !slices.Contains(r.docker.removedImg, "sha256:worker-1") {
		t.Fatalf("old images go: %v", r.docker.removedImg)
	}

	// 되돌리기 — 작업 폴더가 다른 커밋이면 그 커밋을 다시 받아 빌드한다
	r.compose.calls = nil
	did, err := r.d.RollBack(ctx, id, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.wait()
	back, _ := r.history.Get(ctx, did)
	if back.Status != model.DeploySuccess || back.Commit != "a1b2c3d4e5f6" || len(r.git.asked) != 1 || r.git.asked[0] != "a1b2c3d4e5f6" {
		t.Fatalf("%+v %v\n%s", back, r.git.asked, back.Log)
	}
	if strings.Join(r.compose.calls, ",") != "build a1b2c3d4e5f6,up" {
		t.Fatalf("%v", r.compose.calls)
	}

	// 주소 대상이 응답하지 않으면 실패하고 그 출력을 보여 준다
	r.docker.listens = func(n string) bool { return n != "naru-shop-web-1" }
	bad := r.deploy(id)
	if bad.Status != model.DeployFailed || !strings.Contains(bad.Log, "web:3000") || !strings.Contains(bad.Log, "Cannot find module") {
		t.Fatalf("%s", bad.Log)
	}
	r.docker.listens = nil

	// 멈추기·켜기·지우기는 프로젝트를 통째로 — 볼륨은 compose down이 남긴다(-v 없음)
	if err := r.control.Stop(ctx, id); err != nil || !r.get(id).Live.Stopped || r.docker.containers["naru-shop-worker-1"].running {
		t.Fatalf("stop: %v", err)
	}
	if err := r.control.Start(ctx, id); err != nil || r.get(id).Live.Stopped || !r.docker.containers["naru-shop-worker-1"].running {
		t.Fatalf("start: %v", err)
	}
	if err := r.control.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(r.docker.names()) != 0 || r.compose.calls[len(r.compose.calls)-1] != "down" {
		t.Fatalf("down: %v %v", r.docker.names(), r.compose.calls)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the work folder goes")
	}
}
