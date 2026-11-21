package views

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// FinderParts는 다른 컨테이너 찾기가 쓰는 것들이다.
type FinderParts struct {
	Services   contract.ServiceReader
	Containers contract.ContainerWatcher
	Hide       []string // 보여 주지 않을 컨테이너 (Naru 자신과 웹서버)
}

// containerFinder는 Naru가 띄우지 않은 컨테이너를 찾는다. 읽기만 한다 — 남의 컨테이너를 끄거나 지우는 길이 없다.
type containerFinder struct {
	p   FinderParts
	log *slog.Logger
}

func NewContainerFinder(p FinderParts, log *slog.Logger) contract.ContainerFinder {
	return containerFinder{p, log}
}

// Others는 Naru 자신·웹서버·Naru가 띄운 것(라벨이 있거나 서비스가 지금 쓰는 것)을 뺀 나머지다.
// compose 묶음이 있는 것 먼저(묶음 이름순), 그다음 따로 띄운 것 — 안에서는 이름순. Docker를 읽지 못하면 비어 있다.
func (f containerFinder) Others(ctx context.Context) ([]model.OtherContainer, error) {
	services, err := f.p.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, h := range f.p.Hide {
		skip[h] = true
	}
	linked := map[string]model.Service{} // 컨테이너 이름 → 그리로 보내는 외부 연결 서비스
	for _, s := range services {
		skip[s.Live.CurrentContainer()] = true
		if host := externalHost(s.External); host != "" {
			if _, dup := linked[host]; !dup {
				linked[host] = s
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	all, err := f.p.Containers.All(cctx)
	if err != nil {
		f.log.Warn("docker unreachable", "err", err)
		return nil, nil
	}
	var out []model.OtherContainer
	for name, st := range all {
		if skip[name] || st.Service != 0 {
			continue
		}
		c := model.OtherContainer{Name: name, Image: st.Image, Running: st.State == "running", Status: st.Status, Ports: st.Ports, Project: st.Project}
		for _, n := range st.Networks {
			if n.UserDefined {
				c.Networks = append(c.Networks, n.Name)
			}
		}
		c.OnlyBridge = c.Running && len(c.Networks) == 0
		if s, ok := linked[name]; ok {
			c.Linked, c.LinkedName = s.ID, s.Name.String()
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Project == "") != (b.Project == "") {
			return a.Project != ""
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})
	return out, nil
}

// Choices는 외부 연결 칸에 제안할, 돌고 있는 다른 컨테이너다 ("이름:포트", 이름순).
func (f containerFinder) Choices(ctx context.Context) ([]string, error) {
	others, err := f.Others(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range others {
		if c.Running {
			out = append(out, c.Upstream())
		}
	}
	sort.Strings(out)
	return out, nil
}

// externalHost는 외부 연결 주소의 호스트다 ("https://name:3000" → "name"). 주소가 없거나 모양이 다르면 빈 값.
func externalHost(external string) string {
	host, _, err := net.SplitHostPort(strings.TrimPrefix(external, "https://"))
	if err != nil {
		return ""
	}
	return host
}
