package webserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/KangminNa/naru/internal/model"
)

type fakeJoiner struct {
	nets  map[string][]model.DockerNetwork // 컨테이너 → 네트워크
	down  bool
	calls []string
}

func (f *fakeJoiner) NetworksOf(_ context.Context, c string) ([]model.DockerNetwork, error) {
	if f.down {
		return nil, errors.New("docker: connection refused")
	}
	n, ok := f.nets[c]
	if !ok {
		return nil, model.ErrNotFound
	}
	return n, nil
}
func (f *fakeJoiner) Join(_ context.Context, net, c string) error {
	f.calls = append(f.calls, "join "+net)
	f.nets[c] = append(f.nets[c], model.DockerNetwork{Name: net, UserDefined: true})
	return nil
}
func (f *fakeJoiner) Leave(_ context.Context, net, c string) error {
	f.calls = append(f.calls, "leave "+net)
	var keep []model.DockerNetwork
	for _, n := range f.nets[c] {
		if n.Name != net {
			keep = append(keep, n)
		}
	}
	f.nets[c] = keep
	return nil
}

func user(names ...string) []model.DockerNetwork {
	var out []model.DockerNetwork
	for _, n := range names {
		out = append(out, model.DockerNetwork{Name: n, UserDefined: n != "bridge" && n != "host"})
	}
	return out
}

func mapTo(addrs ...string) model.SiteMap {
	var m model.SiteMap
	for i, a := range addrs {
		m.Sites = append(m.Sites, model.Site{Hosts: []string{fmt.Sprintf("s%d.example.com", i)}, Destination: model.Destination{Address: a}})
	}
	return m
}

func TestReachJoinsOtherNetworksAndLeavesWhatItJoined(t *testing.T) {
	j := &fakeJoiner{nets: map[string][]model.DockerNetwork{
		"naru-caddy": user("shelf-net", "extra"),
		"couchdb":    user("db_default"),
		"grafana":    user("monitor_default", "shelf-net"),
		"legacy":     user("bridge"),
		"hostnet":    user("host"),
	}}
	r := NewWebServerReach(j, "naru-caddy", "shelf-net", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	m := mapTo("couchdb:5984", "https://grafana:3000", "nas.local:5000", "192.168.0.20:80", "host.docker.internal:8080", "legacy:80", "hostnet:9000")
	m.Sites = append(m.Sites, model.Site{Hosts: []string{"app.example.com"}, Destination: model.Destination{Address: "naru-app:80", Container: true},
		Settings: model.SiteSettings{Paths: []model.SitePath{{Destination: model.Destination{Address: "couchdb:5984"}}}}})
	problems := r.Ensure(ctx, m)
	if fmt.Sprint(j.calls) != "[join db_default]" {
		t.Fatalf("only the other user-defined network is joined (grafana is already on the app network): %v", j.calls)
	}
	sort.Slice(problems, func(a, b int) bool { return problems[a].Host < problems[b].Host })
	if fmt.Sprint(problems) != "[{hostnet defaultbridge} {legacy defaultbridge}]" {
		t.Fatalf("containers without name resolution are reported, not joined: %v", problems)
	}

	j.calls = nil
	if r.Ensure(ctx, m); len(j.calls) != 0 {
		t.Fatalf("already joined — nothing to do: %v", j.calls)
	}

	j.calls = nil
	r.Ensure(ctx, mapTo("nas.local:5000"))
	if fmt.Sprint(j.calls) != "[leave db_default]" {
		t.Fatalf("leaves only what it joined — never the app network, never a network it found the web server on: %v", j.calls)
	}

	// 웹서버 컨테이너가 새로 만들어져 연결이 풀리면 다시 붙인다
	j.calls = nil
	r.Ensure(ctx, mapTo("couchdb:5984"))
	j.nets["naru-caddy"] = user("shelf-net")
	r.Ensure(ctx, mapTo("couchdb:5984"))
	if fmt.Sprint(j.calls) != "[join db_default join db_default]" {
		t.Fatalf("%v", j.calls)
	}

	j.down = true
	if got := r.Ensure(ctx, mapTo("couchdb:5984")); len(got) != 0 {
		t.Fatalf("without Docker nothing can be told apart — no problems claimed: %v", got)
	}
}
