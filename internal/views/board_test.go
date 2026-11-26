package views

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KangminNa/naru/internal/model"
	"github.com/KangminNa/naru/internal/store"
)

type switchLog struct{ did []string }

func (s *switchLog) TurnOff(_ context.Context, n string) error {
	s.did = append(s.did, "stop "+n)
	return nil
}
func (s *switchLog) TurnOn(_ context.Context, n string) error {
	s.did = append(s.did, "start "+n)
	return nil
}
func (s *switchLog) Restart(_ context.Context, n string) error {
	s.did = append(s.did, "restart "+n)
	return nil
}
func (s *switchLog) RemoveStopped(_ context.Context, n string) error {
	s.did = append(s.did, "remove "+n)
	return nil
}

type fixedLogs []model.LogLine

func (l fixedLogs) Recent(context.Context, string, int) ([]model.LogLine, error) { return l, nil }

type fixedUsage map[string]model.ResourceUsage

func (u fixedUsage) Latest(n string) (model.ResourceUsage, bool) { v, ok := u[n]; return v, ok }

func TestContainerBoardGuardsWhatNaruOwns(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "naru.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	services := store.NewServices(db)
	name := func(s string) model.ServiceName { n, _ := model.ParseServiceName(s); return n }
	blog, _ := services.Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog", Alias: "naru-blog"})
	store.NewLiveStates(db).Save(ctx, blog, model.LiveState{Alias: "naru-blog", Instance: "naru-blog-3"})
	shop, _ := services.Create(ctx, model.NewService{Name: name("shop"), Kind: model.KindCompose, Source: "https://github.com/me/shop", ComposeProject: "shop"})
	store.NewLiveStates(db).Save(ctx, shop, model.LiveState{Alias: "shop", Instance: "shop-web-1", Port: 3000})
	dn, _ := model.ParseDomainName("shop.example.com")
	store.NewDomains(db).Add(ctx, shop, model.DomainInput{Domain: dn, Target: model.DomainTarget{Service: "web", Port: 3000}})

	st := func(n, state, status, project, svc string) model.ContainerState {
		return model.ContainerState{Name: n, State: state, Running: state == "running", Status: status, Project: project, ComposeService: svc}
	}
	with := func(c model.ContainerState, deps string) model.ContainerState { c.DependsOn = deps; return c }
	all := model.ContainerStates{
		"naru":            st("naru", "running", "Up 1 day", "naru", ""),
		"naru-caddy":      st("naru-caddy", "running", "Up 1 day", "naru", ""),
		"naru-blog-3":     st("naru-blog-3", "running", "Up 2 hours", "", ""),
		"shop-web-1":      st("shop-web-1", "running", "Up 1 hour", "shop", "web"),
		"shop-worker-1":   st("shop-worker-1", "running", "Up 1 hour", "shop", "worker"),
		"cairn-web-1":     with(st("cairn-web-1", "running", "Up 3 days (unhealthy)", "cairn", "web"), "migrate:service_completed_successfully:false"),
		"cairn-migrate-1": st("cairn-migrate-1", "exited", "Exited (0) 3 days ago", "cairn", "migrate"),
		"cairn-worker-1":  st("cairn-worker-1", "exited", "Exited (1) 1 hour ago", "cairn", "worker"),
		"loose":           st("loose", "exited", "Exited (1) 9 days ago", "", ""),
	}
	sw := &switchLog{}
	b := NewContainerBoard(BoardParts{
		Services: services, Containers: someContainers{all: all},
		Inspector: someDetails{"cairn-web-1": {Env: []string{"DB_PASSWORD=hunter2", "TZ=Asia/Seoul", "TZ=UTC"}, Binds: []string{"/srv/a:/a:ro"},
			NamedVolumes: []string{"cairn_db:/var/lib/db"}, Health: "unhealthy", HealthLog: "connection refused"}},
		Logs:   fixedLogs{{Text: "listening on 3000"}, {Text: "ERROR db timeout"}},
		Switch: sw, Sweeper: sw, Usage: fixedUsage{"cairn-web-1": {CPUPercent: 1.5, MemUsed: 100 << 20}},
		Self: "naru", WebServer: "naru-caddy",
	})

	groups, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	locked := map[string]string{}
	rows := map[string]model.ContainerRow{}
	for _, g := range groups {
		names = append(names, g.Project)
		locked[g.Project] = g.Locked
		for _, r := range g.Rows {
			rows[r.Name] = r
		}
	}
	if strings.Join(names, ",") != "cairn,naru,shop," {
		t.Fatalf("projects by name, standalone last: %v", names)
	}
	if locked["naru"] != model.LockedNaru || locked["shop"] != model.LockedService || locked["cairn"] != "" || locked[""] != "" {
		t.Fatalf("%v", locked)
	}
	if r := rows["shop-web-1"]; r.Service != shop || strings.Join(r.Domains, ",") != "shop.example.com" || r.Locked != model.LockedService {
		t.Fatalf("the compose address goes to web only: %+v", r)
	}
	if r := rows["shop-worker-1"]; len(r.Domains) != 0 || r.Service != shop {
		t.Fatalf("%+v", r)
	}
	if r := rows["naru-blog-3"]; r.Service != blog || r.Locked != model.LockedService {
		t.Fatalf("%+v", r)
	}
	if r := rows["cairn-web-1"]; r.Health != "unhealthy" || r.Usage == nil || r.Usage.CPUPercent != 1.5 || r.Locked != "" {
		t.Fatalf("%+v", r)
	}
	if !rows["cairn-migrate-1"].Ended || rows["cairn-worker-1"].Ended {
		t.Fatal("exit 0 is a finished one-off, exit 1 is not")
	}

	for name, want := range map[string]error{"naru": model.ErrProtected, "naru-caddy": model.ErrProtected, "naru-blog-3": model.ErrProtected,
		"shop-web-1": model.ErrProtected, "nope": model.ErrNotFound} {
		if err := b.Act(ctx, name, model.ActRestart); !errors.Is(err, want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := b.Act(ctx, "cairn-web-1", model.ActRemove); !errors.Is(err, model.ErrRunning) {
		t.Fatalf("%v", err)
	}
	if err := b.ActProject(ctx, "shop", model.ActStop); !errors.Is(err, model.ErrProtected) {
		t.Fatalf("a Naru compose project is handled on its service page: %v", err)
	}
	if err := b.ActProject(ctx, "naru", model.ActRestart); !errors.Is(err, model.ErrProtected) {
		t.Fatalf("%v", err)
	}
	if err := b.ActProject(ctx, "", model.ActStop); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("standalone ones are not a project: %v", err)
	}
	sw.did = nil
	b.Act(ctx, "cairn-web-1", model.ActRestart)
	b.Act(ctx, "loose", model.ActRemove)
	b.ActProject(ctx, "cairn", model.ActStart)
	b.ActProject(ctx, "cairn", model.ActStop)
	if got := strings.Join(sw.did, ","); got != "restart cairn-web-1,remove loose,start cairn-worker-1,stop cairn-web-1" {
		t.Fatalf("one-offs that finished stay finished: %s", got)
	}

	v, err := b.Detail(ctx, "cairn-web-1", "error")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.EnvNames, ",") != "DB_PASSWORD,TZ" || v.HealthLog != "connection refused" || v.Project != "cairn" {
		t.Fatalf("%+v", v)
	}
	if strings.Join(v.Mounts, "|") != "/srv/a → /a (ro)|cairn_db (volume) → /var/lib/db" {
		t.Fatalf("%v", v.Mounts)
	}
	if len(v.Logs) != 1 || v.Logs[0].Text != "ERROR db timeout" {
		t.Fatalf("search is case-insensitive: %+v", v.Logs)
	}
	if strings.Contains(strings.Join(v.EnvNames, " ")+v.HealthLog, "hunter2") {
		t.Fatal("no values")
	}
}
