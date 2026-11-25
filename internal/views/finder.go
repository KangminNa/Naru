package views

import (
	"context"
	"log/slog"
	"net"
	"slices"
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
	Inspector  contract.ContainerInspector
	Hide       []string // 보여 주지 않을 컨테이너 (Naru 자신과 웹서버)
	AppNetwork string   // 앱 네트워크 — Naru가 어차피 붙이므로 "다른 네트워크"로 채우지 않는다
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
	projects := map[string]bool{}        // Naru가 다루는 compose 프로젝트 — 그 컨테이너들은 "다른 것"이 아니다
	for _, s := range services {
		skip[s.Live.CurrentContainer()] = true
		if s.ComposeProject != "" || s.Live.Alias == s.Project() {
			projects[s.Project()] = true
		}
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
		if skip[name] || st.Service != 0 || (st.Project != "" && projects[st.Project]) {
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

// Seed는 그 컨테이너를 저장소 서비스로 옮길 때 화면에 채울 값이다. 환경 변수는 이름만 꺼낸다 — 값은 화면으로 가지 않는다.
// Naru가 띄우지 않은 컨테이너만 된다. 없으면 model.ErrNotFound.
func (f containerFinder) Seed(ctx context.Context, name string) (model.ContainerSeed, error) {
	others, err := f.Others(ctx)
	if err != nil {
		return model.ContainerSeed{}, err
	}
	i := slices.IndexFunc(others, func(c model.OtherContainer) bool { return c.Name == name })
	if i < 0 {
		return model.ContainerSeed{}, model.ErrNotFound
	}
	c := others[i]
	seed := model.ContainerSeed{Name: c.Name, Linked: c.Linked, LinkedName: c.LinkedName}
	if len(c.Ports) > 0 {
		seed.Port = c.Ports[0]
	}
	var nets []string
	for _, n := range c.Networks {
		if n != f.p.AppNetwork {
			nets = append(nets, n)
		}
	}
	seed.Networks = strings.Join(nets, " ")
	if c.Linked != 0 {
		if s, err := f.p.Services.Get(ctx, c.Linked); err == nil {
			for _, d := range s.Domains {
				seed.LinkedDomains = append(seed.LinkedDomains, d.Domain.String())
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	details, err := f.p.Inspector.Inspect(cctx, name)
	if err != nil {
		return model.ContainerSeed{}, err
	}
	var binds []string
	for _, b := range details.Binds {
		if _, err := model.ParseVolumes(b); err == nil {
			binds = append(binds, b)
		}
	}
	seed.Volumes, seed.NamedVolumes = strings.Join(binds, "\n"), details.NamedVolumes
	if _, err := model.ParseRepoURL(details.Source); err == nil {
		seed.Repo = details.Source
	}
	for _, l := range details.Env {
		if k, _, ok := strings.Cut(l, "="); ok {
			seed.Env = append(seed.Env, model.SeedEnv{Name: k, FromImage: slices.Contains(details.ImageEnv, l)})
		}
	}
	sort.Slice(seed.Env, func(i, j int) bool { return seed.Env[i].Name < seed.Env[j].Name })
	return seed, nil
}

// externalHost는 외부 연결 주소의 호스트다 ("https://name:3000" → "name"). 주소가 없거나 모양이 다르면 빈 값.
func externalHost(external string) string {
	host, _, err := net.SplitHostPort(strings.TrimPrefix(external, "https://"))
	if err != nil {
		return ""
	}
	return host
}
