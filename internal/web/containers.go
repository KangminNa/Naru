package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// ── 컨테이너 화면 — 이 서버의 모든 컨테이너 ─────────────

type containersScreen struct {
	Groups      []model.ContainerGroup // 찾기·거르기를 거친 것
	Total       int
	Running     int
	Stopped     int // 멈춘 것 (한 번 돌고 잘 끝난 것은 빼고)
	Sick        int // 상태 검사 실패
	CPU         float64
	Mem         uint64
	Shown       int
	Query       string
	State       string // "" · running · stopped · problem
	Unreachable bool   // Docker를 읽지 못했다
}

// containerStates는 거르기 칩이다 — 쿼리에서 받는 값은 이것뿐.
var containerStates = map[string]bool{"": true, "running": true, "stopped": true, "problem": true}

func (s *Server) containersPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	d := containersScreen{Query: strings.TrimSpace(q.Get("q")), State: q.Get("state")}
	if !containerStates[d.State] {
		d.State = ""
	}
	groups, err := s.d.Containers.List(r.Context())
	if err != nil {
		s.d.Log.Warn("containers", "err", err)
		d.Unreachable = true
	}
	needle := strings.ToLower(d.Query)
	for _, g := range groups {
		kept := g
		kept.Rows = nil
		for _, c := range g.Rows {
			d.Total++
			stopped := !c.Running() && !c.Ended
			sick := c.Health == "unhealthy" || c.State == "restarting"
			switch {
			case c.Running():
				d.Running++
			case stopped:
				d.Stopped++
			}
			if sick {
				d.Sick++
			}
			if c.Usage != nil {
				d.CPU += c.Usage.CPUPercent
				d.Mem += c.Usage.MemUsed
			}
			match := needle == "" || strings.Contains(strings.ToLower(c.Name), needle) || strings.Contains(strings.ToLower(c.Image), needle) ||
				strings.Contains(strings.ToLower(g.Project), needle)
			switch d.State {
			case "running":
				match = match && c.Running()
			case "stopped":
				match = match && stopped
			case "problem":
				match = match && (sick || stopped)
			}
			if match {
				kept.Rows = append(kept.Rows, c)
			}
		}
		if len(kept.Rows) > 0 {
			d.Shown += len(kept.Rows)
			d.Groups = append(d.Groups, kept)
		}
	}
	s.render(w, r, http.StatusOK, "containers", view{Nav: "containers", User: &u, OK: okKeys[r.URL.Query().Get("ok")],
		Err: errKeys[r.URL.Query().Get("err")], Data: d})
}

// containersSelected는 목록에서 고른 컨테이너들에 한 가지 동작을 한다 (다시 시작·멈추기·켜기 — 지우기는 하나씩 상세에서).
// 보호된 것은 건너뛴다 — 하나라도 막히거나 실패하면 그렇게 알린다.
func (s *Server) containersSelected(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	back := "/containers"
	if ret := r.FormValue("back"); strings.HasPrefix(ret, "?") && !strings.ContainsAny(ret, "\r\n") {
		back += ret
	}
	join := func(k, v string) string {
		if strings.Contains(back, "?") {
			return back + "&" + k + "=" + v
		}
		return back + "?" + k + "=" + v
	}
	a, err := model.ParseContainerAction(r.FormValue("action"))
	names := r.Form["c"]
	if err != nil {
		redirect(w, r, join("err", "action"))
		return
	}
	if len(names) == 0 {
		redirect(w, r, join("err", "none"))
		return
	}
	if a == model.ActRemove { // 지우기는 바로 하지 않는다 — 확인 화면을 그린다
		s.renderRemoval(w, r, names, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var failed error
	for _, n := range names {
		if err := s.d.Containers.Act(ctx, n, a); err != nil {
			s.d.Log.Warn("container action", "container", n, "action", a, "err", err)
			if failed == nil || errors.Is(err, model.ErrProtected) {
				failed = err
			}
		}
	}
	if failed != nil {
		redirect(w, r, join("err", errCode(failed)))
		return
	}
	redirect(w, r, join("ok", "selected-"+string(a)))
}

func (s *Server) containerPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	v, err := s.d.Containers.Detail(r.Context(), r.PathValue("name"), r.URL.Query().Get("q"))
	if err != nil {
		s.render(w, r, http.StatusNotFound, "notfound", view{})
		return
	}
	s.render(w, r, http.StatusOK, "container", view{Nav: "containers", User: &u, OK: okKeys[r.URL.Query().Get("ok")],
		Err: errKeys[r.URL.Query().Get("err")], Data: v})
}

// containerLogsText는 그 컨테이너의 최근 출력을 글자 파일로 준다 (받기).
func (s *Server) containerLogsText(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	v, err := s.d.Containers.Detail(r.Context(), name, r.URL.Query().Get("q"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+url.PathEscape(v.Row.Name)+`-logs.txt"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for _, l := range v.Logs {
		fmt.Fprintf(w, "%s %s %s\n", l.At.UTC().Format(time.RFC3339), l.Stream, l.Text)
	}
}

// containerAction은 컨테이너 하나에 동작한다 — 다시 시작·멈추기·켜기·지우기(멈춘 것, 이름을 다시 적어야).
func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	back := "/containers/" + url.PathEscape(name)
	a, err := model.ParseContainerAction(r.PathValue("action"))
	if err != nil {
		redirect(w, r, back+"?err=action")
		return
	}
	if a == model.ActRemove && strings.TrimSpace(r.FormValue("confirm")) != name {
		redirect(w, r, back+"?err=confirm#remove")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.d.Containers.Act(ctx, name, a); err != nil {
		s.d.Log.Warn("container action", "container", name, "action", a, "err", err)
		redirect(w, r, back+"?err="+errCode(err))
		return
	}
	if a == model.ActRemove {
		redirect(w, r, "/containers?ok=removed")
		return
	}
	redirect(w, r, back+"?ok="+string(a))
}

// projectAction은 compose 묶음에 동작한다.
func (s *Server) projectAction(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	project := r.PathValue("project")
	a, err := model.ParseContainerAction(r.PathValue("action"))
	if err != nil || a == model.ActRemove {
		redirect(w, r, "/containers?err=action")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if err := s.d.Containers.ActProject(ctx, project, a); err != nil {
		s.d.Log.Warn("project action", "project", project, "action", a, "err", err)
		redirect(w, r, "/containers?err="+errCode(err)+"#p-"+url.PathEscape(project))
		return
	}
	redirect(w, r, "/containers?ok=project-"+string(a)+"#p-"+url.PathEscape(project))
}

// ── 이미지 · 볼륨 ─────────────────────────

func (s *Server) storagePage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	v, err := s.d.Storage.Disk(r.Context())
	if err != nil {
		s.d.Log.Warn("disk usage", "err", err)
	}
	s.render(w, r, http.StatusOK, "storage", view{Nav: "storage", User: &u, OK: okKeys[r.URL.Query().Get("ok")],
		Err: errKeys[r.URL.Query().Get("err")], Data: v})
}

func (s *Server) removeImage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	id := model.ImageID(r.PathValue("id"))
	if strings.TrimSpace(r.FormValue("confirm")) != model.ShortImageID(id) {
		redirect(w, r, "/storage?err=confirm")
		return
	}
	if err := s.d.Storage.RemoveImage(r.Context(), id); err != nil {
		s.d.Log.Warn("remove image", "image", model.ShortImageID(id), "err", err)
		redirect(w, r, "/storage?err="+errCode(err))
		return
	}
	redirect(w, r, "/storage?ok=image-removed")
}

func (s *Server) removeVolume(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	if strings.TrimSpace(r.FormValue("confirm")) != name {
		redirect(w, r, "/storage?err=confirm#volumes")
		return
	}
	if err := s.d.Storage.RemoveVolume(r.Context(), name); err != nil {
		s.d.Log.Warn("remove volume", "volume", name, "err", err)
		redirect(w, r, "/storage?err="+errCode(err)+"#volumes")
		return
	}
	redirect(w, r, "/storage?ok=volume-removed#volumes")
}

// errCode는 실패를 쿼리 값으로 — errKeys가 문구로 바꾼다.
func errCode(err error) string {
	switch {
	case errors.Is(err, model.ErrProtected):
		return "protected"
	case errors.Is(err, model.ErrRunning):
		return "running"
	case errors.Is(err, model.ErrInUse):
		return "inuse"
	case errors.Is(err, model.ErrNotFound):
		return "gone"
	}
	if _, ok := model.AsInputError(err); ok {
		return "action"
	}
	return "docker"
}

// 리다이렉트 뒤 ?err= 로 받는 값. 쿼리에서 받는 값은 이 목록으로만 바꾼다.
var errKeys = map[string]string{
	"rollback":  "err.rollback",
	"protected": "err.protected", "running": "err.running", "inuse": "err.inuse", "gone": "err.gone",
	"action": "err.action", "docker": "err.docker", "confirm": "err.confirmname", "none": "err.noneselected",
	"network": "err.networkname", "alias": "err.alias",
}

// auditTarget은 컨테이너·묶음·이미지·볼륨 동작의 대상이다 — 경로에서 꺼낸다.
func auditTarget(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "containers" && parts[1] == "projects":
		return parts[2]
	case len(parts) == 3 && parts[0] == "containers":
		return unescape(parts[1])
	case len(parts) == 5 && parts[0] == "containers" && parts[2] == "networks":
		return unescape(parts[1]) + " → " + unescape(parts[3])
	case len(parts) == 3 && parts[0] == "networks":
		return unescape(parts[1])
	case len(parts) == 4 && parts[0] == "storage" && parts[1] == "images":
		return model.ShortImageID(model.ImageID(unescape(parts[2])))
	case len(parts) == 4 && parts[0] == "storage":
		return unescape(parts[2])
	}
	return ""
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// ── 여러 컨테이너 지우기 — 확인 화면 ────────────

type removalScreen struct {
	Rows    []model.RemovalRow
	Count   int // 지울 것 (건너뛰는 것 빼고, 돌고 있는 것은 넣고)
	Running int
	Word    string // 입력해야 할 확인 글자
}

func (s *Server) renderRemoval(w http.ResponseWriter, r *http.Request, names []string, errKey string) {
	u, _, _ := s.currentUser(r)
	rows, err := s.d.Removal.Plan(r.Context(), names)
	if err != nil {
		redirect(w, r, "/containers?err=docker")
		return
	}
	d := removalScreen{Rows: rows, Word: removeWord(langOf(r))}
	for _, row := range rows {
		switch row.Skip {
		case "":
			d.Count++
		case "running":
			d.Count++
			d.Running++
		}
	}
	status := http.StatusOK
	if errKey != "" {
		status = http.StatusBadRequest
	}
	s.render(w, r, status, "containers_remove", view{Nav: "containers", User: &u, Err: errKey, Data: d})
}

// removeWord는 확인 칸에 적어야 하는 글자다.
func removeWord(lang string) string {
	if lang == "ko" {
		return "지우기"
	}
	return "delete"
}

// containersRemove는 확인 화면에서 지운다 — 확인 글자가 맞아야 한다.
func (s *Server) containersRemove(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	r.ParseForm()
	names := r.PostForm["c"]
	if len(names) == 0 {
		redirect(w, r, "/containers?err=none")
		return
	}
	word := strings.TrimSpace(r.FormValue("confirm"))
	if word != removeWord("ko") && word != removeWord("en") {
		s.renderRemoval(w, r, names, "err.confirmword")
		return
	}
	opts := model.RemoveOptions{StopRunning: r.FormValue("stop") == "1", AnonVolumes: r.FormValue("volumes") == "1"}
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	res, err := s.d.Removal.Remove(ctx, names, opts)
	switch {
	case err != nil:
		s.d.Log.Warn("remove containers", "err", err)
		redirect(w, r, "/containers?err=docker")
	case len(res.Failed) > 0:
		s.d.Log.Warn("remove containers", "failed", res.Failed)
		redirect(w, r, "/containers?err=docker")
	case len(res.Skipped) > 0:
		redirect(w, r, "/containers?ok=removed-some")
	default:
		redirect(w, r, "/containers?ok=removed-many")
	}
}

// ── 네트워크 ──────────────────────────────

type networksScreen struct {
	Rows        []model.NetworkRow
	Unreachable bool
	Name        string // 만들기 칸에 다시 채울 값
	Internal    bool
}

func (s *Server) networksPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	s.renderNetworks(w, r, u, http.StatusOK, "", networksScreen{})
}

func (s *Server) renderNetworks(w http.ResponseWriter, r *http.Request, u model.Account, status int, errKey string, d networksScreen) {
	rows, err := s.d.Networks.List(r.Context())
	if err != nil {
		s.d.Log.Warn("networks", "err", err)
		d.Unreachable = true
	}
	d.Rows = rows
	if errKey == "" {
		errKey = errKeys[r.URL.Query().Get("err")]
	}
	s.render(w, r, status, "networks", view{Nav: "networks", User: &u, OK: okKeys[r.URL.Query().Get("ok")], Err: errKey, Data: d})
}

func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	d := networksScreen{Name: r.FormValue("name"), Internal: r.FormValue("internal") == "1"}
	name, err := model.ParseNetworkName(d.Name)
	if err != nil {
		s.renderNetworks(w, r, u, http.StatusBadRequest, "err.networkname", d)
		return
	}
	if err := s.d.Networks.Create(r.Context(), name, d.Internal); err != nil {
		s.d.Log.Warn("create network", "network", name.String(), "err", err)
		key := "err.docker"
		if errors.Is(err, model.ErrInUse) {
			key = "err.networkexists"
		}
		s.renderNetworks(w, r, u, http.StatusBadRequest, key, d)
		return
	}
	redirect(w, r, "/networks?ok=network-created")
}

func (s *Server) removeNetwork(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	if strings.TrimSpace(r.FormValue("confirm")) != name {
		redirect(w, r, "/networks?err=confirm")
		return
	}
	if err := s.d.Networks.Remove(r.Context(), name); err != nil {
		s.d.Log.Warn("remove network", "network", name, "err", err)
		redirect(w, r, "/networks?err="+errCode(err))
		return
	}
	redirect(w, r, "/networks?ok=network-removed")
}

func (s *Server) containerAttach(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	back := "/containers/" + url.PathEscape(name)
	network, err := model.ParseNetworkName(r.FormValue("network"))
	if err != nil {
		redirect(w, r, back+"?err=network#networks")
		return
	}
	if err := s.d.Networks.Attach(r.Context(), name, network, r.FormValue("alias")); err != nil {
		s.d.Log.Warn("attach network", "container", name, "network", network.String(), "err", err)
		code := errCode(err)
		if ie, ok := model.AsInputError(err); ok && ie.Code == "alias" {
			code = "alias"
		}
		redirect(w, r, back+"?err="+code+"#networks")
		return
	}
	redirect(w, r, back+"?ok=attached#networks")
}

func (s *Server) containerDetach(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.gate(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	back := "/containers/" + url.PathEscape(name)
	if err := s.d.Networks.Detach(r.Context(), name, r.PathValue("network")); err != nil {
		s.d.Log.Warn("detach network", "container", name, "network", r.PathValue("network"), "err", err)
		redirect(w, r, back+"?err="+errCode(err)+"#networks")
		return
	}
	redirect(w, r, back+"?ok=detached#networks")
}
