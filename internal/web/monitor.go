package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// ── 서버 화면 ──────────────────────────────

type serverScreen struct {
	Range    string
	Ranges   []string
	Now      model.ServerSnapshot
	HasData  bool
	CPU      chart
	Mem      chart
	Disk     chart
	NetIn    chart
	NetOut   chart
	Events   []auditRow
	Services []serviceRow
}

// chart는 그래프 하나다 — 값(오래된 것부터)과 눈금의 끝, 그 기간의 최고와 합.
type chart struct {
	Values []float64
	Top    float64
	Peak   float64
	Sum    float64
}

type serviceRow struct {
	ID       model.ServiceID
	Name     string
	CPU      chart
	Mem      chart
	MemNow   uint64
	Restarts int
	OOM      int
}

func chartOf(vals []float64, top float64) chart {
	c := chart{Values: vals, Top: top}
	for _, v := range vals {
		c.Peak = max(c.Peak, v)
		c.Sum += v
	}
	if c.Top <= 0 {
		c.Top = c.Peak
	}
	return c
}

// chartLine은 그래프 값을 SVG polyline의 점으로 바꾼다 (viewBox 0 0 w h, 위가 0).
func chartLine(c chart, w, h int) string {
	if len(c.Values) == 0 || c.Top <= 0 {
		return ""
	}
	var b strings.Builder
	n := len(c.Values)
	for i, v := range c.Values {
		x := 0.0
		if n > 1 {
			x = float64(i) * float64(w) / float64(n-1)
		}
		y := float64(h) - min(v/c.Top, 1)*float64(h-2) - 1
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}

func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func (s *Server) serverPage(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.gate(w, r)
	if !ok {
		return
	}
	rng := model.ParseMetricRange(r.URL.Query().Get("range"))
	d := serverScreen{Range: string(rng), Now: s.d.Stats.Now()}
	for _, x := range model.AllRanges() {
		d.Ranges = append(d.Ranges, string(x))
	}
	mv, err := s.d.Monitor.Monitor(r.Context(), rng)
	if err != nil {
		s.d.Log.Error("monitor", "err", err)
	}
	var cpu, mem, disk, in, out []float64
	for _, p := range mv.Server {
		cpu = append(cpu, p.CPU)
		mem = append(mem, pct(p.MemUsed, p.MemTotal))
		disk = append(disk, pct(p.DiskUsed, p.DiskTotal))
		in, out = append(in, float64(p.NetIn)), append(out, float64(p.NetOut))
	}
	d.HasData = len(mv.Server) > 0
	d.CPU, d.Mem, d.Disk = chartOf(cpu, 100), chartOf(mem, 100), chartOf(disk, 100)
	d.NetIn, d.NetOut = chartOf(in, 0), chartOf(out, 0)
	if top := max(d.NetIn.Peak, d.NetOut.Peak); top > 0 {
		d.NetIn.Top, d.NetOut.Top = top, top // 같은 눈금으로
	}
	for _, sm := range mv.Services {
		var c, m []float64
		row := serviceRow{ID: sm.Service, Name: sm.Name, Restarts: sm.Restarts, OOM: sm.OOM}
		for _, p := range sm.Points {
			c, m = append(c, p.CPU), append(m, float64(p.MemUsed))
			row.MemNow = p.MemUsed
		}
		row.CPU, row.Mem = chartOf(c, 0), chartOf(m, 0)
		d.Services = append(d.Services, row)
	}
	for _, e := range mv.Events {
		d.Events = append(d.Events, auditRowOf(e))
	}
	s.render(w, r, http.StatusOK, "server", view{Nav: "server", User: &u, Data: d})
}

// ── 기록 ───────────────────────────────────

// auditRow는 기록 한 줄을 화면에 — 문구 키(audit.<무엇을>)에 대상·덧붙임을 끼운다.
type auditRow struct {
	At     time.Time
	Key    string
	Args   []string // 문구에 끼울 것 — 대상, 덧붙임
	Target string
	Detail string
	Who    string
	IP     string
	OK     bool
	Tone   string
}

func auditRowOf(e model.AuditEntry) auditRow {
	row := auditRow{At: e.At, Key: "audit." + e.Action, Args: []string{e.Target, e.Detail}, Target: e.Target, Detail: e.Detail, Who: e.Who, IP: e.IP, OK: e.OK}
	if e.Action == "login" && !e.OK {
		row.Key = "audit.login.failed"
	}
	switch {
	case !e.OK:
		row.Tone = "bad"
	case strings.HasSuffix(e.Action, ".ok") || strings.HasSuffix(e.Action, ".up"):
		row.Tone = "ok"
	}
	return row
}

// auditActions는 기록할 관리자의 요청이다 — 경로 모양 → 문구 키. 성공(다른 곳으로 보냄, 3xx)했을 때만 남긴다.
// 로그인은 실패도 남겨야 해서 따로 (login). 웹훅은 사람이 아니라 기록하지 않는다 (배포는 Naru가 본 일로 남는다).
var auditActions = map[string]string{
	"POST /logout":                                 "logout",
	"POST /services/new":                           "service.create",
	"POST /services/{id}/deploy":                   "service.deploy",
	"POST /services/{id}/rollback/{did}":           "service.rollback",
	"POST /services/{id}/stop":                     "service.stop",
	"POST /services/{id}/start":                    "service.start",
	"POST /services/{id}/settings":                 "service.settings",
	"POST /services/{id}/domains":                  "service.domain.add",
	"POST /services/{id}/domains/{did}/delete":     "service.domain.remove",
	"POST /services/{id}/domains/{did}/target":     "service.domain.target",
	"POST /services/{id}/port":                     "service.port",
	"POST /services/{id}/web":                      "service.web",
	"POST /services/{id}/delete":                   "service.delete",
	"POST /settings/password":                      "settings.password",
	"POST /settings/domain":                        "settings.domain",
	"POST /settings/alerts":                        "settings.alert.add",
	"POST /settings/alerts/{cid}/delete":           "settings.alert.remove",
	"POST /containers/{name}/{action}":             "container.*", // * = 경로의 동작 (restart·stop·start·remove)
	"POST /containers/projects/{project}/{action}": "project.*",
	"POST /storage/images/{id}/delete":             "image.remove",
	"POST /storage/volumes/{name}/delete":          "volume.remove",
}

// statusRecorder는 처리 결과(상태 코드·보낸 곳)를 기억한다.
type statusRecorder struct {
	http.ResponseWriter
	code     int
	location string
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code, r.location = code, r.Header().Get("Location")
	}
	r.ResponseWriter.WriteHeader(code)
}

// audited는 관리자의 바꾸는 요청이 성공하면 기록에 남긴다 — 핸들러마다 적지 않고 한 곳에서.
// 누가인지는 처리하기 전에 본다 (로그아웃하면 세션이 사라진다). 바꾼 값은 남기지 않는다.
func (s *Server) audited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || s.d.Audit == nil {
			next.ServeHTTP(w, r)
			return
		}
		_, pattern := s.mux.Handler(r)
		action, ok := auditActions[pattern]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		u, _, signedIn := s.currentUser(r)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		// 로그인 화면이나 첫 설정으로 보냈으면 하지 않은 것이다 (로그아웃만 로그인 화면으로 가는 것이 성공)
		sentAway := strings.HasPrefix(rec.location, "/setup") || (strings.HasPrefix(rec.location, "/login") && action != "logout")
		if !signedIn || rec.code < 300 || rec.code >= 400 || sentAway {
			return
		}
		if prefix, ok := strings.CutSuffix(action, ".*"); ok {
			a, err := model.ParseContainerAction(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			if err != nil {
				return
			}
			action = prefix + "." + string(a)
		}
		failed := strings.Contains(rec.location, "?err=") || strings.Contains(rec.location, "&err=") // 되돌려 보내며 실패를 알렸다
		e := model.AuditEntry{At: s.d.Clock.Now(), Who: u.Username.String(), IP: clientIP(r), Action: action, OK: !failed,
			Service: serviceIn(r.URL.Path), Target: auditTarget(r.URL.Path)}
		if e.Service == 0 {
			e.Service = serviceIn(rec.location) // 만들기 — 새 번호는 보낸 곳에 있다
		}
		if action == "service.delete" {
			e.Target = strings.TrimSpace(r.FormValue("confirm")) // 지운 뒤에는 이름을 찾을 수 없다
		}
		if err := s.d.Audit.Record(r.Context(), e); err != nil {
			s.d.Log.Warn("audit", "err", err)
		}
	})
}

// serviceIn은 "/services/12/…"의 서비스 번호다 (없으면 0).
func serviceIn(path string) model.ServiceID {
	rest, ok := strings.CutPrefix(path, "/services/")
	if !ok {
		return 0
	}
	num, _, _ := strings.Cut(rest, "/")
	num, _, _ = strings.Cut(num, "?")
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return model.ServiceID(n)
}

// auditLogin은 로그인 성공·실패를 남긴다 — 실패면 적은 이름 그대로.
func (s *Server) auditLogin(r *http.Request, username string, ok bool) {
	if s.d.Audit == nil {
		return
	}
	e := model.AuditEntry{At: s.d.Clock.Now(), Who: username, IP: clientIP(r), Action: "login", OK: ok}
	if err := s.d.Audit.Record(r.Context(), e); err != nil {
		s.d.Log.Warn("audit", "err", err)
	}
}
