package views

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// BoardParts는 컨테이너 화면이 쓰는 것들이다.
type BoardParts struct {
	Services   contract.ServiceReader
	Containers contract.ContainerWatcher
	Inspector  contract.ContainerInspector
	Logs       contract.ContainerLogReader
	Switch     contract.ContainerSwitch
	Sweeper    contract.ContainerSweeper
	Usage      contract.UsageBoard // 없으면 사용량을 비워 둔다
	Self       string              // Naru 자신의 컨테이너 이름
	WebServer  string              // 웹서버 컨테이너 이름
}

// containerBoard는 이 서버의 모든 컨테이너를 보여 주고, 다시 시작·멈추기·켜기·멈춘 것 지우기를 한다.
// Naru 자신·웹서버·Naru 서비스의 것은 건드리지 않는다 — 서비스는 서비스 화면에서 (멈춤 표시·배포 기록과 어긋나지 않게).
type containerBoard struct{ p BoardParts }

func NewContainerBoard(p BoardParts) contract.ContainerBoard { return containerBoard{p} }

const logLinesShown = 500

var lockRank = map[string]int{"": 0, model.LockedService: 1, model.LockedWebServer: 2, model.LockedNaru: 3}

// List는 compose 묶음별(이름순, 따로 띄운 것은 끝)로, 안에서는 이름순이다.
func (b containerBoard) List(ctx context.Context) ([]model.ContainerGroup, error) {
	rows, err := b.rows(ctx)
	if err != nil {
		return nil, err
	}
	byProject := map[string]*model.ContainerGroup{}
	var order []string
	for _, r := range rows {
		g, ok := byProject[r.project]
		if !ok {
			g = &model.ContainerGroup{Project: r.project}
			byProject[r.project] = g
			order = append(order, r.project)
		}
		g.Rows = append(g.Rows, r.ContainerRow)
		if r.Running() {
			g.Running++
		}
		if lockRank[r.Locked] > lockRank[g.Locked] {
			g.Locked = r.Locked // 가장 무거운 이유 — Naru 자신 > 웹서버 > 서비스
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, c := order[i], order[j]
		if (a == "") != (c == "") {
			return a != ""
		}
		return a < c
	})
	out := make([]model.ContainerGroup, 0, len(order))
	for _, p := range order {
		g := byProject[p]
		sort.Slice(g.Rows, func(i, j int) bool { return g.Rows[i].Name < g.Rows[j].Name })
		if p == "" {
			g.Locked = "" // 따로 띄운 것은 묶음이 아니다 — 묶음 동작이 없다
		}
		out = append(out, *g)
	}
	return out, nil
}

// Detail은 컨테이너 하나다. 환경 변수는 이름만 꺼낸다 — 값은 화면으로 가지 않는다.
func (b containerBoard) Detail(ctx context.Context, name, query string) (model.ContainerView, error) {
	r, err := b.find(ctx, name)
	if err != nil {
		return model.ContainerView{}, err
	}
	v := model.ContainerView{Row: r.ContainerRow, Project: r.project, Query: strings.TrimSpace(query)}
	ictx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if d, err := b.p.Inspector.Inspect(ictx, name); err == nil {
		v.Started, v.Restarts, v.HealthLog = d.Started, d.Restarts, d.HealthLog
		if d.Health != "" {
			v.Row.Health = d.Health
		}
		v.EnvNames = envNames(d.Env)
		for _, m := range d.Binds {
			v.Mounts = append(v.Mounts, mountLine(m, false))
		}
		for _, m := range d.NamedVolumes {
			v.Mounts = append(v.Mounts, mountLine(m, true))
		}
	}
	lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
	defer lcancel()
	if lines, err := b.p.Logs.Recent(lctx, name, logLinesShown); err == nil {
		v.Logs = filterLines(lines, v.Query)
	}
	return v, nil
}

// Act는 컨테이너 하나에 동작한다.
func (b containerBoard) Act(ctx context.Context, name string, a model.ContainerAction) error {
	r, err := b.find(ctx, name)
	if err != nil {
		return err
	}
	if r.Locked != "" {
		return model.ErrProtected
	}
	switch a {
	case model.ActRestart:
		return b.p.Switch.Restart(ctx, name)
	case model.ActStop:
		return b.p.Switch.TurnOff(ctx, name)
	case model.ActStart:
		return b.p.Switch.TurnOn(ctx, name)
	case model.ActRemove:
		if r.Running() || r.State == "restarting" {
			return model.ErrRunning
		}
		return b.p.Sweeper.RemoveStopped(ctx, name)
	}
	return model.InputError{Field: "action", Code: "action"}
}

// ActProject는 compose 묶음에 동작한다. 다시 시작·멈추기는 돌고 있는 것만, 켜기는 멈춘 것 중 한 번 돌고 끝난 것(마이그레이션 등)을 뺀 것만.
func (b containerBoard) ActProject(ctx context.Context, project string, a model.ContainerAction) error {
	if a == model.ActRemove {
		return model.InputError{Field: "action", Code: "action"}
	}
	groups, err := b.List(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.Project != project || project == "" {
			continue
		}
		if g.Locked != "" {
			return model.ErrProtected
		}
		var errs []error
		for _, r := range g.Rows {
			var err error
			switch {
			case a == model.ActRestart && r.Running():
				err = b.p.Switch.Restart(ctx, r.Name)
			case a == model.ActStop && r.Running():
				err = b.p.Switch.TurnOff(ctx, r.Name)
			case a == model.ActStart && !r.Running() && !r.Ended:
				err = b.p.Switch.TurnOn(ctx, r.Name)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			}
		}
		return errors.Join(errs...)
	}
	return model.ErrNotFound
}

type boardRow struct {
	model.ContainerRow
	project string
}

func (b containerBoard) find(ctx context.Context, name string) (boardRow, error) {
	rows, err := b.rows(ctx)
	if err != nil {
		return boardRow{}, err
	}
	for _, r := range rows {
		if r.Name == name {
			return r, nil
		}
	}
	return boardRow{}, model.ErrNotFound
}

// rows는 모든 컨테이너를 한 줄씩 — 누구의 것인지, 어느 주소가 보내는지, 막는 이유까지.
func (b containerBoard) rows(ctx context.Context) ([]boardRow, error) {
	services, err := b.p.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	owner := map[string]model.Service{}    // 컨테이너 이름 → 그것을 띄운 Naru 서비스
	projects := map[string]model.Service{} // Naru가 다루는 compose 프로젝트
	linked := map[string]model.Service{}   // 컨테이너 이름 → 그리로 보내는 외부 연결 서비스
	byID := map[model.ServiceID]model.Service{}
	for _, s := range services {
		byID[s.ID] = s
		if n := s.Live.CurrentContainer(); n != "" {
			owner[n] = s
		}
		if s.ComposeProject != "" || s.Live.Alias == s.Project() {
			projects[s.Project()] = s
		}
		if host := externalHost(s.External); host != "" {
			if _, dup := linked[host]; !dup {
				linked[host] = s
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	all, err := b.p.Containers.All(cctx)
	if err != nil {
		return nil, err
	}
	out := make([]boardRow, 0, len(all))
	for name, st := range all {
		r := model.ContainerRow{Name: name, Image: st.Image, State: st.State, Status: st.Status, Health: model.HealthOf(st.Status),
			Ports: st.Published, Exposed: st.Ports, Ended: all.Finished(name)}
		for _, n := range st.Networks {
			if n.UserDefined {
				r.Networks = append(r.Networks, n.Name)
			}
		}
		r.OnlyBridge = r.Running() && len(r.Networks) == 0
		if b.p.Usage != nil && r.Running() {
			if u, ok := b.p.Usage.Latest(name); ok {
				r.Usage = &u
			}
		}
		s, mine := owner[name]
		if !mine && st.Service != 0 {
			s, mine = byID[st.Service]
			if !mine { // 지운 서비스가 남긴 것 — 그래도 Naru의 것이다
				r.Service, r.Locked = st.Service, model.LockedService
			}
		}
		if !mine && st.Project != "" {
			s, mine = projects[st.Project]
		}
		switch {
		case name == b.p.Self:
			r.Locked = model.LockedNaru
		case name == b.p.WebServer:
			r.Locked = model.LockedWebServer
		case mine:
			r.Service, r.ServiceName, r.Locked = s.ID, s.Name.String(), model.LockedService
			r.Domains = domainsTo(s, st)
		}
		if l, ok := linked[name]; ok {
			r.Linked, r.LinkedName = l.ID, l.Name.String()
			for _, d := range l.Domains {
				r.Domains = append(r.Domains, d.Domain.String())
			}
		}
		out = append(out, boardRow{r, st.Project})
	}
	return out, nil
}

// domainsTo는 Naru 서비스의 주소 중 이 컨테이너로 오는 것이다 — Compose는 주소마다 대상이 다르다.
func domainsTo(s model.Service, st model.ContainerState) []string {
	var out []string
	for _, d := range s.Domains {
		if d.Target.IsZero() || d.Target.Service == st.ComposeService {
			if s.Kind == model.KindCompose && d.Target.IsZero() {
				continue
			}
			out = append(out, d.Domain.String())
		}
	}
	return out
}

// envNames는 "KEY=value"들에서 이름만 (이름순, 한 번씩).
func envNames(env []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// mountLine은 "/서버경로:/컨테이너경로[:ro]" 또는 "이름:/경로"를 화면 한 줄로.
func mountLine(m string, volume bool) string {
	parts := strings.Split(m, ":")
	if len(parts) < 2 {
		return m
	}
	line := parts[0]
	if volume {
		line += " (volume)"
	}
	line += " → " + parts[1]
	if len(parts) > 2 && parts[2] == "ro" {
		line += " (ro)"
	}
	return line
}

// filterLines는 q가 든 줄만 (대소문자 무시). q가 비면 그대로.
func filterLines(lines []model.LogLine, q string) []model.LogLine {
	if q == "" {
		return lines
	}
	q = strings.ToLower(q)
	var out []model.LogLine
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l.Text), q) {
			out = append(out, l)
		}
	}
	return out
}

// ── 이미지 · 볼륨 ─────────────────────────

// StorageParts는 이미지·볼륨 화면이 쓰는 것들이다.
type StorageParts struct {
	Disk    contract.DiskUsageReader
	Images  contract.ImageCleaner
	Volumes contract.VolumeRemover
}

type storageBoard struct{ p StorageParts }

func NewStorageBoard(p StorageParts) contract.StorageBoard { return storageBoard{p} }

// Disk는 이미지·볼륨이다 — 안 쓰는 것을 위로, 그 안에서 큰 것부터.
func (b storageBoard) Disk(ctx context.Context) (model.StorageView, error) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d, err := b.p.Disk.DiskUsage(dctx)
	if err != nil {
		return model.StorageView{}, err
	}
	v := model.StorageView{Disk: d, DockerReadable: true}
	sort.SliceStable(v.Disk.Images, func(i, j int) bool { return (v.Disk.Images[i].UsedBy == 0) && v.Disk.Images[j].UsedBy != 0 })
	sort.SliceStable(v.Disk.Volumes, func(i, j int) bool { return (v.Disk.Volumes[i].UsedBy == 0) && v.Disk.Volumes[j].UsedBy != 0 })
	for _, i := range d.Images {
		if i.UsedBy == 0 {
			v.UnusedImages += i.Size
		}
	}
	for _, vol := range d.Volumes {
		if vol.UsedBy == 0 {
			v.UnusedVolumes += vol.Size
		}
	}
	return v, nil
}

// RemoveImage는 이미지 하나를 ID로 지운다. 강제하지 않는다 — 쓰는 컨테이너가 있으면 Docker가 거절한다 (model.ErrInUse).
func (b storageBoard) RemoveImage(ctx context.Context, id model.ImageID) error {
	return b.p.Images.Remove(ctx, string(id))
}

func (b storageBoard) RemoveVolume(ctx context.Context, name string) error {
	return b.p.Volumes.RemoveVolume(ctx, name)
}
