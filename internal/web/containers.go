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
	Groups      []model.ContainerGroup
	Count       int
	Unreachable bool // Docker를 읽지 못했다
}

func (s *Server) containersPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	d := containersScreen{}
	groups, err := s.d.Containers.List(r.Context())
	if err != nil {
		s.d.Log.Warn("containers", "err", err)
		d.Unreachable = true
	}
	d.Groups = groups
	for _, g := range groups {
		d.Count += len(g.Rows)
	}
	s.render(w, r, http.StatusOK, "containers", view{Nav: "containers", User: &u, OK: okKeys[r.URL.Query().Get("ok")],
		Err: errKeys[r.URL.Query().Get("err")], Data: d})
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
	s.render(w, r, http.StatusOK, "storage", view{Nav: "containers", User: &u, OK: okKeys[r.URL.Query().Get("ok")],
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
	"action": "err.action", "docker": "err.docker", "confirm": "err.confirmname",
}

// auditTarget은 컨테이너·묶음·이미지·볼륨 동작의 대상이다 — 경로에서 꺼낸다.
func auditTarget(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "containers" && parts[1] == "projects":
		return parts[2]
	case len(parts) == 3 && parts[0] == "containers":
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
