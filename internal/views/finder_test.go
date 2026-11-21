package views

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KangminNa/naru/internal/model"
	"github.com/KangminNa/naru/internal/store"
)

type someContainers struct {
	noContainers
	all model.ContainerStates
	err error
}

func (c someContainers) All(context.Context) (model.ContainerStates, error) { return c.all, c.err }

func TestOtherContainersAreTheOnesNaruDidNotStart(t *testing.T) {
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
	services.Create(ctx, model.NewService{Name: name("old"), Kind: model.KindImage, Source: "me/old", Alias: "shelf-old"}) // v1에서 온 것 — 라벨이 없다
	cairn, _ := services.Create(ctx, model.NewService{Name: name("cairn"), Kind: model.KindExternal, External: "cairn-web-1:3000"})

	user := func(n string) model.DockerNetwork { return model.DockerNetwork{Name: n, UserDefined: true} }
	running := func(n string) model.ContainerState {
		return model.ContainerState{Name: n, Running: true, State: "running", Status: "Up 3 days"}
	}
	with := func(st model.ContainerState, f func(*model.ContainerState)) model.ContainerState { f(&st); return st }
	all := model.ContainerStates{
		"naru":        running("naru"),
		"naru-caddy":  running("naru-caddy"),
		"naru-blog-3": with(running("naru-blog-3"), func(s *model.ContainerState) { s.Service = blog }),
		"naru-blog-4": with(running("naru-blog-4"), func(s *model.ContainerState) { s.Service = blog }), // 배포 중인 새 것
		"shelf-old":   running("shelf-old"),
		"cairn-web-1": with(running("cairn-web-1"), func(s *model.ContainerState) {
			s.Project, s.Image, s.Ports = "cairn", "cairn-web", []model.Port{3000}
			s.Networks = []model.DockerNetwork{{Name: "bridge"}, user("cairn_default"), user("shelf-net")}
		}),
		"cairn-db-1": with(running("cairn-db-1"), func(s *model.ContainerState) {
			s.Project, s.Ports, s.Networks = "cairn", []model.Port{5432}, []model.DockerNetwork{user("cairn_default")}
		}),
		"couchdb": with(running("couchdb"), func(s *model.ContainerState) {
			s.Ports, s.Networks = []model.Port{5984}, []model.DockerNetwork{{Name: "bridge"}}
		}),
		"old-api": {Name: "old-api", State: "exited", Status: "Exited (0) 5 days ago", Networks: []model.DockerNetwork{{Name: "bridge"}}},
		"aaa":     with(running("aaa"), func(s *model.ContainerState) { s.Networks = []model.DockerNetwork{user("x")} }),
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := NewContainerFinder(FinderParts{Services: services, Containers: someContainers{all: all}, Hide: []string{"naru-caddy", "naru"}}, quiet)

	others, err := f.Others(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	by := map[string]model.OtherContainer{}
	for _, c := range others {
		names = append(names, c.Name)
		by[c.Name] = c
	}
	if got := strings.Join(names, " "); got != "cairn-db-1 cairn-web-1 aaa couchdb old-api" {
		t.Fatalf("compose projects first, then the rest by name — never Naru's own: %s", got)
	}
	web := by["cairn-web-1"]
	if web.Project != "cairn" || web.Linked != cairn || web.LinkedName != "cairn" || strings.Join(web.Networks, " ") != "cairn_default shelf-net" || web.OnlyBridge || web.Upstream() != "cairn-web-1:3000" {
		t.Fatalf("%+v", web)
	}
	if !by["couchdb"].OnlyBridge || by["old-api"].OnlyBridge || by["old-api"].Running || by["aaa"].Upstream() != "aaa:" {
		t.Fatalf("only a running container on no user-defined network is flagged: %+v %+v", by["couchdb"], by["old-api"])
	}

	choices, _ := f.Choices(ctx)
	if got := strings.Join(choices, " "); got != "aaa: cairn-db-1:5432 cairn-web-1:3000 couchdb:5984" {
		t.Fatalf("running ones only, by name: %s", got)
	}

	down := NewContainerFinder(FinderParts{Services: services, Containers: someContainers{err: errors.New("docker is down")}}, quiet)
	if others, err := down.Others(ctx); err != nil || len(others) != 0 {
		t.Fatalf("an unreachable Docker means an empty list, not an error: %v %v", others, err)
	}
}
