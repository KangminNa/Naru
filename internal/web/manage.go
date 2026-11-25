package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// ── 서비스 추가 ─────────────────────────────

type newServiceScreen struct {
	Kinds      []string
	Form       serviceForm
	Containers []string // 외부 연결 칸 자동완성 — 이 서버의 다른 컨테이너
	Networks   []string // "다른 네트워크" 칸 자동완성 — 다른 컨테이너들이 붙어 있는 네트워크
	Adopt      *adoptView
}

// adoptView는 "저장소로 옮기기" — 돌고 있는 컨테이너에서 읽은 것 중 칸에 채우지 못하는 부분이다.
type adoptView struct {
	Seed     model.ContainerSeed
	Env      []adoptEnv
	TakeOver bool
}

type adoptEnv struct {
	Name      string
	FromImage bool
	Checked   bool
}

// adoptOf는 넘겨받을 컨테이너를 다시 읽는다. chosen이 nil이면 처음 여는 것 — 이미지가 정한 것만 빼고 고른다.
// 읽지 못하면(없어졌거나 Naru의 것이면) nil — 화면은 보통의 새 서비스 화면이 된다.
func (s *Server) adoptOf(r *http.Request, name string, chosen []string, first, takeOver bool) *adoptView {
	if name == "" {
		return nil
	}
	seed, err := s.d.Finder.Seed(r.Context(), name)
	if err != nil {
		return nil
	}
	a := &adoptView{Seed: seed, TakeOver: seed.Linked != 0 && (first || takeOver)}
	for _, e := range seed.Env {
		a.Env = append(a.Env, adoptEnv{Name: e.Name, FromImage: e.FromImage, Checked: (first && !e.FromImage) || slices.Contains(chosen, e.Name)})
	}
	return a
}

// suggestions는 폼의 자동완성이다 — 돌고 있는 다른 컨테이너("이름:포트")와 그것들이 붙은 네트워크. 못 읽으면 제안만 없다.
func (s *Server) suggestions(r *http.Request) (containers, networks []string) {
	others, _ := s.d.Finder.Others(r.Context())
	for _, c := range others {
		if c.Running {
			containers = append(containers, c.Upstream())
		}
		for _, n := range c.Networks {
			if !slices.Contains(networks, n) {
				networks = append(networks, n)
			}
		}
	}
	slices.Sort(containers)
	slices.Sort(networks)
	return containers, networks
}

func kindNames() []string {
	var out []string
	for _, k := range model.AllKinds() {
		out = append(out, string(k))
	}
	return out
}

func (s *Server) newServicePage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	f := serviceForm{Branch: "main"}
	if k, err := model.ParseKindName(r.URL.Query().Get("kind")); err == nil {
		f.Kind = string(k)
	}
	// 홈의 [주소 붙이기]가 연결 대상을 채워서 연다 — 모양이 맞는 것만 받는다
	if up := strings.TrimSpace(r.URL.Query().Get("upstream")); up != "" && len(up) <= 200 {
		if _, err := model.ParseExternalAddress(up); err == nil || strings.HasSuffix(up, ":") {
			f.Upstream = up
		}
	}
	// 홈의 [저장소로 옮기기] — 그 컨테이너에서 읽은 값으로 칸을 채운다
	adopt := s.adoptOf(r, strings.TrimSpace(r.URL.Query().Get("adopt")), nil, true, false)
	if adopt != nil {
		// 저장소 주소는 채우지 않는다 — 이미지 라벨은 남이 만든 이미지의 저장소일 수 있다. 힌트로만 보여 준다.
		f.Adopt, f.Volumes, f.Networks = adopt.Seed.Name, adopt.Seed.Volumes, adopt.Seed.Networks
		if adopt.Seed.Port != 0 {
			f.Port = strconv.Itoa(int(adopt.Seed.Port))
		}
	}
	choices, networks := s.suggestions(r)
	s.render(w, r, http.StatusOK, "new_service", view{Nav: "home", User: &u,
		Data: newServiceScreen{Kinds: kindNames(), Form: f, Containers: choices, Networks: networks, Adopt: adopt}})
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	f := readServiceForm(r)
	fail := func(err error) {
		f.Token = "" // 토큰은 다시 그리지 않는다
		choices, networks := s.suggestions(r)
		s.render(w, r, http.StatusBadRequest, "new_service", view{Nav: "home", User: &u, Err: errKeyOf(err), ErrLine: errLineOf(err),
			Data: newServiceScreen{Kinds: kindNames(), Form: f, Containers: choices, Networks: networks,
				Adopt: s.adoptOf(r, f.Adopt, f.EnvFrom, false, f.TakeOver)}})
	}
	kind, err := model.ParseKindName(f.Kind)
	if err != nil {
		fail(err)
		return
	}
	in, notes, err := f.input(kind)
	if err != nil {
		fail(err)
		return
	}
	// 주소를 옮겨 올 서비스는 화면이 보낸 번호가 아니라, 그 컨테이너로 보내는 서비스를 다시 찾아서 정한다
	if f.Adopt != "" && f.TakeOver {
		if seed, err := s.d.Finder.Seed(r.Context(), f.Adopt); err == nil {
			in.TakeOver = seed.Linked
		}
	}
	id, did, err := s.d.Launcher.Launch(r.Context(), in)
	if id == 0 {
		if _, isInput := model.AsInputError(err); !isInput {
			s.d.Log.Error("create service", "err", err)
		}
		fail(err)
		return
	}
	s.d.Log.Info("service created", "service", id, "kind", f.Kind, "by", u.Username.String())
	if err != nil || did == 0 {
		redirect(w, r, withEnvFix(fmt.Sprintf("/services/%d", id), notes))
		return
	}
	redirect(w, r, withEnvFix(fmt.Sprintf("/services/%d/deploys/%d", id, did), notes))
}

// ── 서비스 하나: 동작 ───────────────────────

// detailFor는 경로의 {id} 서비스 화면 값을 읽는다. 없으면 404를 그리고 false.
func (s *Server) detailFor(w http.ResponseWriter, r *http.Request) (model.ServiceView, bool) {
	id := pathID(r, "id")
	if id > 0 {
		v, err := s.d.Viewer.Detail(r.Context(), model.ServiceID(id))
		if err == nil {
			return v, true
		}
		if !errors.Is(err, model.ErrNotFound) {
			s.d.Log.Error("get service", "id", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return v, false
		}
	}
	s.render(w, r, http.StatusNotFound, "notfound", view{})
	return model.ServiceView{}, false
}

func (s *Server) deployNow(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	id := v.Service.ID
	did, err := s.d.Deployer.Deploy(r.Context(), id, model.ReasonManual)
	switch {
	case errors.Is(err, model.ErrQueued):
		redirect(w, r, fmt.Sprintf("/services/%d?ok=queued", id))
	case err != nil:
		redirect(w, r, fmt.Sprintf("/services/%d", id))
	default:
		redirect(w, r, fmt.Sprintf("/services/%d/deploys/%d", id, did))
	}
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	id := v.Service.ID
	did, err := s.d.Deployer.RollBack(r.Context(), id, model.DeploymentID(pathID(r, "did")))
	switch {
	case errors.Is(err, model.ErrBusy):
		redirect(w, r, fmt.Sprintf("/services/%d?ok=queued", id))
	case err != nil:
		redirect(w, r, fmt.Sprintf("/services/%d?err=rollback#deploys", id))
	default:
		redirect(w, r, fmt.Sprintf("/services/%d/deploys/%d", id, did))
	}
}

func (s *Server) stopService(w http.ResponseWriter, r *http.Request)  { s.runtimeAction(w, r, true) }
func (s *Server) startService(w http.ResponseWriter, r *http.Request) { s.runtimeAction(w, r, false) }

func (s *Server) runtimeAction(w http.ResponseWriter, r *http.Request, stop bool) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	sv := v.Service
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var err error
	flash := "started"
	if stop {
		err, flash = s.d.Control.Stop(ctx, sv.ID), "stopped"
	} else {
		err = s.d.Control.Start(ctx, sv.ID)
	}
	if err != nil {
		s.d.Log.Warn("start/stop", "service", sv.Name.String(), "err", err)
		redirect(w, r, fmt.Sprintf("/services/%d?err=docker", sv.ID))
		return
	}
	s.d.Log.Info("service "+flash, "service", sv.Name.String(), "by", u.Username.String())
	redirect(w, r, fmt.Sprintf("/services/%d?ok=%s", sv.ID, flash))
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	f := readServiceForm(r)
	in, notes, err := f.input(v.Service.Kind)
	if err == nil {
		in.Domain = model.DomainName{} // 설정 폼에는 주소가 없다 — 주소는 따로 붙이고 뗀다
		var redeploy bool
		if redeploy, err = s.d.Editor.Update(r.Context(), v.Service.ID, in); err == nil {
			flash := "saved"
			if redeploy {
				flash = "redeploy"
			}
			redirect(w, r, withEnvFix(fmt.Sprintf("/services/%d?ok=%s", v.Service.ID, flash), notes)+"#settings")
			return
		}
	}
	status := http.StatusBadRequest
	if _, isInput := model.AsInputError(err); !isInput {
		s.d.Log.Error("save settings", "service", v.Service.Name.String(), "err", err)
		status = http.StatusInternalServerError
	}
	page := s.serviceView(r, u, v, "settings", &f)
	page.Err, page.ErrLine = errKeyOf(err), errLineOf(err)
	s.render(w, r, status, "service", page)
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	domain, err := model.ParseDomainName(r.FormValue("domain"))
	if err == nil {
		err = s.d.Editor.AddDomain(r.Context(), v.Service.ID, model.DomainInput{Domain: domain, HTTPS: r.FormValue("https") == "1"})
	}
	if err != nil {
		status := http.StatusBadRequest
		if _, isInput := model.AsInputError(err); !isInput {
			status = http.StatusInternalServerError
		}
		s.renderService(w, r, u, v, status, "domain", errKeyOf(err), nil)
		return
	}
	redirect(w, r, fmt.Sprintf("/services/%d?ok=domain-added#domains", v.Service.ID))
}

func (s *Server) removeDomain(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	s.d.Editor.RemoveDomain(r.Context(), v.Service.ID, model.DomainID(pathID(r, "did")))
	redirect(w, r, fmt.Sprintf("/services/%d?ok=domain-removed#domains", v.Service.ID))
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	sv := v.Service
	if strings.TrimSpace(r.FormValue("confirm")) != sv.Name.String() {
		s.renderService(w, r, u, v, http.StatusBadRequest, "delete", "err.confirm", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.d.Control.Remove(ctx, sv.ID); err != nil {
		key := "err.internal"
		if errors.Is(err, model.ErrBusy) {
			key = "err.busy"
		}
		s.renderService(w, r, u, v, http.StatusConflict, "delete", key, nil)
		return
	}
	s.d.Log.Warn("service deleted", "service", sv.Name.String(), "by", u.Username.String())
	redirect(w, r, "/?ok=deleted")
}

// ── 배포 하나 ─────────────────────────────

type deployScreen struct {
	Service    model.Service
	Deployment model.Deployment
	Steps      []stepView
}

type stepView struct {
	Label string
	State string // done | doing | failed
}

func (s *Server) deployPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	dv, err := s.d.Viewer.Deployment(r.Context(), model.ServiceID(pathID(r, "id")), model.DeploymentID(pathID(r, "did")))
	if err != nil {
		s.render(w, r, http.StatusNotFound, "notfound", view{})
		return
	}
	lang := langOf(r)
	d := deployScreen{Service: dv.Service, Deployment: dv.Deployment}
	for _, st := range dv.Steps {
		label := st.Ko
		if lang == "en" && st.En != "" {
			label = st.En
		}
		d.Steps = append(d.Steps, stepView{Label: label, State: st.State})
	}
	v := view{Nav: "home", User: &u, Data: d, EnvFixes: envFixesOf(r)}
	if dv.Deployment.Status == model.DeployRunning {
		v.Refresh = 2 // 진행 중에는 2초마다 새로 그린다
	}
	s.render(w, r, http.StatusOK, "deploy", v)
}

// ── 로그 ──────────────────────────────────

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	lv, err := s.d.Viewer.Logs(r.Context(), model.ServiceID(pathID(r, "id")), model.ParseLogQuery(q.Get("show"), q.Get("deploy"), q.Get("q")))
	if err != nil {
		s.render(w, r, http.StatusNotFound, "notfound", view{})
		return
	}
	s.render(w, r, http.StatusOK, "logs", view{Nav: "home", User: &u, Data: lv})
}

// logsText는 로그 화면에서 고른 것을 글자 파일로 준다 (받기). 화면보다 많은 줄(5,000줄까지).
func (s *Server) logsText(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	q := r.URL.Query()
	query := model.ParseLogQuery(q.Get("show"), q.Get("deploy"), q.Get("q"))
	query.Download = true
	lv, err := s.d.Viewer.Logs(r.Context(), model.ServiceID(pathID(r, "id")), query)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := lv.Service.Name.String()
	if lv.Deployment != 0 {
		name += fmt.Sprintf("-%d", lv.Deployment)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`-logs.txt"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for _, l := range lv.Lines {
		if l.Source == model.LogRequest {
			fmt.Fprintf(w, "%s request %s %s %d %dms\n", l.At.UTC().Format(time.RFC3339), l.Method, l.Path, l.Status, l.Took.Milliseconds())
		} else {
			fmt.Fprintf(w, "%s %s %s\n", l.At.UTC().Format(time.RFC3339), l.Stream, l.Text)
		}
	}
}

// ── 서비스 화면 그리기 ──────────────────────

type serviceScreen struct {
	Service       model.Service
	Card          card
	Target        string
	Container     bool // 멈추기·켜기가 된다
	Deployable    bool
	Deploys       []model.Deployment
	Latest        *model.Deployment
	LiveID        model.DeploymentID
	Running       bool
	WebhookURL    string
	WebhookSecret string
	HasToken      bool
	Domains       []domainRow
	Settings      serviceForm
	Web           webForm
	WebWarnings   []string            // 적용됐지만 알릴 것 (고급 칸에서 버린 지시어)
	NginxSkipped  []model.SkippedLine // nginx에서 옮기지 못한 줄
	WebRefused    string              // 웹서버가 거절한 이유 (그 말 그대로)
	Form          string              // 오류가 난 폼
	Findings      []finding           // 지켜보기가 찾은 이 서비스의 문제
	Containers    []string            // 외부 연결 칸 자동완성 (외부 연결 서비스만)
	Networks      []string            // "다른 네트워크" 칸 자동완성 (컨테이너 서비스만)
	Usage         *model.ResourceUsage
}

func (s *Server) servicePage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	sv, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	v := s.serviceView(r, u, sv, "", nil)
	v.OK = okKeys[r.URL.Query().Get("ok")]
	v.EnvFixes = envFixesOf(r)
	if e := r.URL.Query().Get("err"); e != "" {
		v.Err = errKeys[e]
	}
	s.render(w, r, http.StatusOK, "service", v)
}

func (s *Server) renderService(w http.ResponseWriter, r *http.Request, u model.Account, sv model.ServiceView, status int, form, errKey string, f *serviceForm) {
	v := s.serviceView(r, u, sv, form, f)
	v.Err = errKey
	s.render(w, r, status, "service", v)
}

func (s *Server) serviceView(r *http.Request, u model.Account, sv model.ServiceView, form string, f *serviceForm) view {
	sc := model.ServiceCard{ID: sv.Service.ID, Name: sv.Service.Name.String(), Kind: sv.Service.Kind, Domain: sv.Service.PrimaryDomain(), Status: sv.Status}
	d := serviceScreen{
		Service: sv.Service, Card: cardOf(sc), Target: sv.Target, Container: sv.Container, Deployable: sv.Deployable,
		Deploys: sv.Deploys, LiveID: sv.LiveID, Running: sv.Deploying, Form: form,
		WebhookURL: sv.Webhook.URL, WebhookSecret: sv.Webhook.Secret, HasToken: sv.Form.HasToken,
		Domains: s.domainRows(r, sv.Domains), Web: webFormOf(sv.Web), Usage: sv.Usage,
	}
	if sv.Service.External != "" || sv.Container {
		d.Containers, d.Networks = s.suggestions(r)
	}
	for _, f := range sv.Findings {
		row := findingOf(f)
		row.Name = "" // 이 서비스 화면 안이다
		d.Findings = append(d.Findings, row)
	}
	if len(d.Deploys) > 0 {
		d.Latest = &d.Deploys[0]
	}
	if f != nil {
		d.Settings = *f
	} else {
		d.Settings = formFrom(sv.Form)
	}
	return view{Nav: "home", User: &u, Data: d}
}

// 리다이렉트 뒤 ?err= 로 받는 값도 이 목록으로만 바꾼다.
var errKeys = map[string]string{"rollback": "err.rollback", "docker": "err.docker"}

// setPort는 진단의 [포트를 N으로 바꾸기]다 — 다시 배포하지 않고 웹서버만 새 포트로 보낸다.
func (s *Server) setPort(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	v, ok := s.detailFor(w, r)
	if !ok {
		return
	}
	id := v.Service.ID
	p, err := model.ParsePort(r.FormValue("port"))
	if err == nil {
		err = s.d.Editor.SetPort(r.Context(), id, p)
	}
	if err != nil {
		s.d.Log.Warn("set port", "service", v.Service.Name.String(), "err", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	redirect(w, r, fmt.Sprintf("/services/%d?ok=port", id))
}
