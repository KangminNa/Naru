// Package views는 화면에 보여줄 서비스 모습을 모은다 (CQRS의 읽기 쪽).
// 고치는 일은 하지 않는다. 비밀은 화면에 필요한 것(웹훅 시크릿, 토큰이 있는지)만 꺼낸다 — 토큰 원문은 담지 않는다.
package views

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// Parts는 화면 모으기가 쓰는 것들이다.
type Parts struct {
	Services   contract.ServiceReader
	Secrets    contract.SecretStore
	History    contract.DeployHistoryReader
	Containers contract.ContainerWatcher
	Kinds      contract.KindLookup
	Admin      contract.AdminDomainSetting
	Deployer   contract.Deployer
	Certs      contract.CertificateReader
	Clock      contract.Clock
	Web        contract.WebSettingsStore
	AppLogs    contract.ContainerLogReader
	Access     contract.AccessLogReader
	Watch      contract.HealthWatcher // 지켜보기가 찾은 문제 — 여기서는 읽기만 한다
	Archive    contract.LogArchive    // 보관된 지난 배포의 앱 출력 (없으면 지난 배포를 고를 수 없다)
	HTTPSPort  int                    // 바깥에서 본 HTTPS 포트 — 443이 아니면 웹훅 주소에 붙인다 (로컬 개발)
}

type serviceViewer struct {
	p   Parts
	log *slog.Logger
}

func NewServiceViewer(p Parts, log *slog.Logger) contract.ServiceViewer { return serviceViewer{p, log} }

// containers는 모든 컨테이너 상태다. Docker를 읽지 못하면 nil이다.
func (v serviceViewer) containers(ctx context.Context) model.ContainerStates {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	all, err := v.p.Containers.All(ctx)
	if err != nil {
		v.log.Warn("docker unreachable", "err", err)
		return nil
	}
	if all == nil {
		all = model.ContainerStates{}
	}
	return all
}

func (v serviceViewer) Home(ctx context.Context) (model.HomeView, error) {
	all, err := v.p.Services.List(ctx)
	if err != nil {
		return model.HomeView{}, err
	}
	type entry struct {
		s     model.Service
		tools contract.KindTools
	}
	var list []entry
	needDocker := false
	for _, s := range all {
		tools, ok := v.p.Kinds.Find(s.Kind)
		if !ok {
			continue
		}
		needDocker = needDocker || tools.Destination.Find(s).Container
		list = append(list, entry{s, tools})
	}
	var containers model.ContainerStates
	var h model.HomeView
	if needDocker {
		containers = v.containers(ctx)
		h.DockerDown = containers == nil
	}
	snap := v.p.Watch.Snapshot()
	for _, e := range list {
		h.Cards = append(h.Cards, model.ServiceCard{
			ID: e.s.ID, Name: e.s.Name.String(), Kind: e.s.Kind, Domain: e.s.PrimaryDomain(),
			Status: e.tools.Status.Read(e.s, containers), Usage: usageOf(snap, e.s),
		})
	}
	h.Findings, h.Checked = snap.Findings, !snap.CheckedAt.IsZero()
	return h, nil
}

func (v serviceViewer) Detail(ctx context.Context, id model.ServiceID) (model.ServiceView, error) {
	s, err := v.p.Services.Get(ctx, id)
	if err != nil {
		return model.ServiceView{}, err
	}
	tools, ok := v.p.Kinds.Find(s.Kind)
	if !ok {
		return model.ServiceView{}, model.ErrNotFound
	}
	to := tools.Destination.Find(s)
	var containers model.ContainerStates
	if to.Container {
		containers = v.containers(ctx)
	}
	sec, err := v.p.Secrets.Get(ctx, id)
	if err != nil {
		return model.ServiceView{}, err
	}
	view := model.ServiceView{
		Service: s, Status: tools.Status.Read(s, containers), Target: to.Address, Container: to.Container,
		Deployable: tools.Builder != nil, Deploying: v.p.Deployer.IsDeploying(id),
		Form: model.ServiceForm{
			Source: s.Source, Branch: s.Branch, BuildPath: s.BuildPath, BuildTarget: s.BuildTarget, Folder: s.Folder, External: s.External, Port: s.Port,
			AutoDeploy: s.AutoDeploy, EnvText: sec.Env.Text(), EnvFileText: sec.EnvFile.Text(), Volumes: sec.Volumes.Text(), Networks: s.Networks, HasToken: sec.GitToken != "",
		},
	}
	if to.Folder != "" {
		view.Target = to.Folder
	}
	certs, _ := v.p.Certs.Read(ctx) // 못 읽으면 "아직 없음"으로 보인다
	now := v.p.Clock.Now()
	for _, d := range s.Domains {
		dv := model.DomainView{Domain: d}
		if d.HTTPS {
			dv.Certificate = certs.For(d.Domain, now)
		}
		view.Domains = append(view.Domains, dv)
	}
	if view.Web, err = v.webView(ctx, s); err != nil {
		return model.ServiceView{}, err
	}
	snap := v.p.Watch.Snapshot()
	for _, f := range snap.Findings {
		if f.Service == id {
			view.Findings = append(view.Findings, f)
		}
	}
	view.Usage = usageOf(snap, s)
	if view.Deployable {
		view.Deploys, _ = v.p.History.Recent(ctx, id, 10)
		view.LiveID = s.Live.LiveDeployment()
		view.Webhook.Secret = sec.WebhookSecret
		if domain, _ := v.p.Admin.Get(ctx); !domain.IsZero() {
			host := domain.String()
			if v.p.HTTPSPort != 0 && v.p.HTTPSPort != 443 {
				host = fmt.Sprintf("%s:%d", host, v.p.HTTPSPort)
			}
			view.Webhook.URL = fmt.Sprintf("https://%s/hooks/%d", host, id)
		}
	}
	return view, nil
}

// usageOf는 지켜보기가 읽은 CPU·메모리다. 직접 멈춘 서비스는 마지막 값이 남아 있어도 보이지 않는다.
func usageOf(snap model.WatchSnapshot, s model.Service) *model.ResourceUsage {
	u, ok := snap.Usage[s.ID]
	if !ok || s.Live.Stopped {
		return nil
	}
	return &u
}

// webView는 화면에 보일 웹서버 설정이다 — 비밀번호 해시 대신 "있음"만, 경로 대상은 서비스 이름으로.
func (v serviceViewer) webView(ctx context.Context, s model.Service) (model.WebSettingsView, error) {
	w, err := v.p.Web.Get(ctx, s.ID)
	if err != nil {
		return model.WebSettingsView{}, err
	}
	all, err := v.p.Services.List(ctx)
	if err != nil {
		return model.WebSettingsView{}, err
	}
	names := map[model.ServiceID]string{}
	for _, other := range all {
		names[other.ID] = other.Name.String()
	}
	out := model.WebSettingsView{
		Headers: w.Headers, AllowFrom: w.AllowFrom, LoginUser: w.Login.User, HasPassword: w.Login.Hash != "",
		Maintenance: w.Maintenance, Advanced: w.Advanced,
	}
	for _, p := range w.Paths {
		target := p.External.String()
		if p.Service != 0 {
			target = names[p.Service]
		}
		out.Paths = append(out.Paths, model.PathRouteInput{Prefix: p.Prefix, Target: target, StripPrefix: p.StripPrefix})
	}
	// 다른 서비스가 경로별 연결로 이 서비스에 보내고 있으면 알려준다 — 지우기 전에 알아야 한다
	for _, other := range all {
		if other.ID == s.ID {
			continue
		}
		ow, err := v.p.Web.Get(ctx, other.ID)
		if err != nil {
			continue
		}
		for _, p := range ow.Paths {
			if p.Service == s.ID {
				out.SentHereBy = append(out.SentHereBy, other.Name.String())
				break
			}
		}
	}
	return out, nil
}

// logLines만큼 로그 화면에 보여준다.
const (
	logLines    = 200  // 화면에 보이는 줄
	searchLines = 5000 // 찾거나 받을 때 읽는 줄
)

// Logs는 앱 출력과 받은 요청을 시간순으로 합친다. 한쪽을 못 읽어도 다른 쪽은 보여준다.
func (v serviceViewer) Logs(ctx context.Context, id model.ServiceID, q model.LogQuery) (model.LogsView, error) {
	s, err := v.p.Services.Get(ctx, id)
	if err != nil {
		return model.LogsView{}, err
	}
	tools, ok := v.p.Kinds.Find(s.Kind)
	if !ok {
		return model.LogsView{}, model.ErrNotFound
	}
	view := model.LogsView{Service: s, Filter: q.Show, Query: q.Text, Live: s.Live.LiveDeployment(),
		HasApp: tools.Destination.Find(s).Container, HasRequests: len(s.Domains) > 0}
	if view.HasApp && v.p.Archive != nil {
		view.Archives, _ = v.p.Archive.List(s.Name)
	}
	// 찾거나 받을 때는 더 많은 줄에서 — 화면에는 맞는 것의 마지막 200줄
	read, show := logLines, logLines
	if q.Text != "" || q.Download {
		read = searchLines
	}
	if q.Download {
		show = searchLines
	}
	var lines []model.LogLine
	archived := q.Deployment != 0 && q.Deployment != view.Live
	switch {
	case archived:
		// 지난 배포 — 보관한 앱 출력만 (요청 기록은 배포와 묶이지 않는다)
		view.Deployment, view.HasRequests = q.Deployment, false
		if v.p.Archive == nil {
			view.AppError = model.ErrNotFound.Error()
			break
		}
		app, err := v.p.Archive.Read(s.Name, q.Deployment)
		if err != nil {
			view.AppError = err.Error()
		}
		lines = append(lines, app...)
	case view.HasApp && q.Show != model.LogsRequests && s.Live.CurrentContainer() != "":
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		app, err := v.p.AppLogs.Recent(lctx, s.Live.CurrentContainer(), read)
		cancel()
		if err != nil {
			view.AppError = err.Error()
		}
		lines = append(lines, app...)
	}
	if !archived && view.HasRequests && q.Show != model.LogsApp {
		var hosts []string
		for _, d := range s.Domains {
			hosts = append(hosts, d.Domain.String())
		}
		reqs, err := v.p.Access.Recent(ctx, hosts, read)
		if err != nil {
			view.RequestError = err.Error()
		}
		lines = append(lines, reqs...)
	}
	matched := lines[:0]
	for _, l := range lines {
		if q.Matches(l) {
			matched = append(matched, l)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].At.Before(matched[j].At) })
	if len(matched) > show {
		matched = matched[len(matched)-show:]
	}
	view.Lines = matched
	return view, nil
}

func (v serviceViewer) Deployment(ctx context.Context, id model.ServiceID, did model.DeploymentID) (model.DeploymentView, error) {
	s, err := v.p.Services.Get(ctx, id)
	if err != nil {
		return model.DeploymentView{}, err
	}
	d, err := v.p.History.Get(ctx, did)
	if err != nil || d.ServiceID != id {
		return model.DeploymentView{}, model.ErrNotFound
	}
	return model.DeploymentView{Service: s, Deployment: d, Steps: stepsOf(d)}, nil
}

// stepsOf는 배포 기록의 "▶ 한국어 / English" 줄에서 단계를 읽는다.
func stepsOf(d model.Deployment) []model.StepView {
	var steps []model.StepView
	for _, line := range strings.Split(d.Log, "\n") {
		label, ok := strings.CutPrefix(line, "▶ ")
		if !ok {
			continue
		}
		ko, en, _ := strings.Cut(label, " / ")
		steps = append(steps, model.StepView{Ko: ko, En: en, State: "done"})
	}
	if n := len(steps); n > 0 {
		switch d.Status {
		case model.DeployRunning:
			steps[n-1].State = "doing"
		case model.DeployFailed:
			steps[n-1].State = "failed"
		}
	}
	return steps
}
