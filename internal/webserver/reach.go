package webserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// thisServerHost는 웹서버 컨테이너에서 본 "이 서버"다 — 컨테이너가 아니다.
const thisServerHost = "host.docker.internal"

type webServerReach struct {
	joiner contract.NetworkJoiner
	self   string // 웹서버 컨테이너 이름
	apps   string // 앱 네트워크 — 떼지 않는다
	log    *slog.Logger

	mu     sync.Mutex
	joined map[string]bool // 이 실행에서 웹서버를 붙인 네트워크 — 뗄 수 있는 것은 이것뿐이다
}

// NewWebServerReach는 웹서버(self)가 지도의 연결 대상 컨테이너에 닿게 한다.
// 다른 사용자 정의 네트워크의 컨테이너면 웹서버를 그 네트워크에 붙인다. 앱 네트워크(apps)에서는 떼지 않는다.
func NewWebServerReach(joiner contract.NetworkJoiner, self, apps string, log *slog.Logger) contract.WebServerReach {
	return &webServerReach{joiner: joiner, self: self, apps: apps, log: log, joined: map[string]bool{}}
}

func (r *webServerReach) Ensure(ctx context.Context, m model.SiteMap) []model.ReachProblem {
	r.mu.Lock()
	defer r.mu.Unlock()
	targets := containerTargets(m)
	if len(targets) == 0 && len(r.joined) == 0 {
		return nil
	}
	mine, err := r.joiner.NetworksOf(ctx, r.self)
	if err != nil {
		// Docker에 닿지 못하면 무엇이 컨테이너인지조차 알 수 없다 — 문제라고 단정하지 않는다
		r.log.Warn("web server container unreadable — other networks are left as they are", "container", r.self, "err", err)
		return nil
	}
	on := map[string]bool{}
	for _, n := range mine {
		on[n.Name] = true
	}
	needed := map[string]bool{}
	var problems []model.ReachProblem
	for _, host := range targets {
		nets, err := r.joiner.NetworksOf(ctx, host)
		if errors.Is(err, model.ErrNotFound) {
			continue // 컨테이너가 아니다 — NAS 같은 바깥 주소
		}
		if err != nil {
			problems = append(problems, model.ReachProblem{Host: host, Why: "docker"})
			continue
		}
		var userDefined []string
		inApps := false
		for _, n := range nets {
			inApps = inApps || n.Name == r.apps
			if n.UserDefined {
				userDefined = append(userDefined, n.Name)
			}
		}
		if inApps {
			continue // 이미 함께 있다
		}
		if len(userDefined) == 0 {
			problems = append(problems, model.ReachProblem{Host: host, Why: "defaultbridge"})
			continue
		}
		pick := userDefined[0]
		for _, n := range userDefined {
			if on[n] {
				pick = n
				break
			}
		}
		needed[pick] = true
		if on[pick] {
			continue
		}
		if err := r.joiner.Join(ctx, pick, r.self); err != nil {
			r.log.Warn("could not join the web server to a network", "network", pick, "for", host, "err", err)
			problems = append(problems, model.ReachProblem{Host: host, Why: "docker"})
			continue
		}
		r.log.Info("web server joined a network to reach a container", "network", pick, "container", host)
		r.joined[pick], on[pick] = true, true
	}
	for n := range r.joined {
		if needed[n] || n == r.apps {
			continue
		}
		if err := r.joiner.Leave(ctx, n, r.self); err != nil {
			r.log.Warn("could not leave a network", "network", n, "err", err)
			continue
		}
		r.log.Info("web server left a network it no longer needs", "network", n)
		delete(r.joined, n)
	}
	return problems
}

// containerTargets는 지도의 바깥 연결 대상 중 컨테이너 이름일 수 있는 것이다 (IP와 "이 서버"는 빼고, 이름순).
// Naru가 띄운 컨테이너(Container)와 정적 사이트(Folder)는 앱 네트워크에 있으니 보지 않는다.
func containerTargets(m model.SiteMap) []string {
	seen := map[string]bool{}
	add := func(d model.Destination) {
		if d.Container || d.Folder != "" || d.Address == "" {
			return
		}
		host, _, err := net.SplitHostPort(strings.TrimPrefix(d.Address, "https://"))
		if err != nil || host == thisServerHost || net.ParseIP(host) != nil {
			return
		}
		seen[host] = true
	}
	for _, s := range m.Sites {
		add(s.Destination)
		for _, p := range s.Settings.Paths {
			add(p.Destination)
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
