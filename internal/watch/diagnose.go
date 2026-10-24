package watch

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// DiagnoserParts는 진단이 쓰는 것들이다.
type DiagnoserParts struct {
	Kinds    contract.KindLookup
	Ports    contract.PortChecker
	DNS      contract.DNSChecker
	Admin    contract.AdminDomainSetting
	Certs    contract.CertificateReader
	Clock    contract.Clock
	PublicIP string // NARU_PUBLIC_IP — 비면 관리 주소를 조회한 IP를 이 서버의 IP로 본다

	WebServer  contract.WebServerSync // 웹서버가 닿게 하지 못한 연결 대상 (Status().Unreachable)
	AppNetwork string                 // 앱 네트워크 이름 — 안내 문구에 넣는다
}

const (
	dnsKeep     = 10 * time.Minute // DNS 답을 이만큼 들고 있는다 — 30초마다 조회하지 않는다
	certsKeep   = time.Minute
	hookWait    = 24 * time.Hour // 만든 지 이만큼 지나도 웹훅이 오지 않으면 말한다
	portSearch  = 5 * time.Second
	portsToTry  = 8
	answerAfter = 3 * time.Second
)

// commonPorts는 이미지가 EXPOSE를 적지 않았을 때 찔러 볼 흔한 앱 포트다.
var commonPorts = []model.Port{80, 3000, 8080, 8000, 5000, 4000}

type seenDNS struct {
	a  model.DNSAnswer
	at time.Time
}

type diagnoser struct {
	p DiagnoserParts

	mu      sync.Mutex
	dns     map[string]seenDNS
	certs   model.Certificates
	certsAt time.Time
}

// NewDiagnoser는 진단을 만든다. 지켜보기 고리 하나가 차례로 부른다.
func NewDiagnoser(p DiagnoserParts) contract.Diagnoser {
	return &diagnoser{p: p, dns: map[string]seenDNS{}}
}

func (d *diagnoser) Diagnose(ctx context.Context, s model.Service, states model.ContainerStates) []model.Finding {
	tools, ok := d.p.Kinds.Find(s.Kind)
	if !ok {
		return nil
	}
	var out []model.Finding
	add := func(f model.Finding) {
		f.Service, f.Name = s.ID, s.Name.String()
		out = append(out, f)
	}
	if tools.Destination.Find(s).Container {
		if f, ok := d.wrongPort(ctx, s, states); ok {
			add(f)
		}
	} else if s.External != "" {
		if f, ok := d.unreachableExternal(ctx, s.External); ok {
			add(f)
		}
	}
	for _, f := range d.dnsFindings(ctx, s) {
		add(f)
	}
	created := s.Created
	if s.FromGit() && s.AutoDeploy && s.HookLog.At.IsZero() && !created.IsZero() && d.p.Clock.Now().Sub(created) > hookWait {
		add(model.Finding{Key: "find.webhook", Level: model.FindingHint})
	}
	return out
}

// wrongPort는 앱 포트는 응답하지 않는데 다른 포트가 응답하는지 본다 — 이미지가 연 포트 먼저, 그다음 흔한 포트.
func (d *diagnoser) wrongPort(ctx context.Context, s model.Service, states model.ContainerStates) (model.Finding, bool) {
	st, ok := states[s.Live.CurrentContainer()]
	if !ok || st.State != "running" || s.Port == 0 {
		return model.Finding{}, false
	}
	host := reach(s)
	actx, cancel := context.WithTimeout(ctx, answerAfter)
	err := d.p.Ports.Answers(actx, host, s.Port)
	cancel()
	if err == nil {
		return model.Finding{}, false
	}
	var candidates []model.Port
	for _, p := range append(slices.Clone(st.Ports), commonPorts...) {
		if p != s.Port && !slices.Contains(candidates, p) && len(candidates) < portsToTry {
			candidates = append(candidates, p)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, portSearch)
	defer cancel()
	for _, p := range candidates {
		if d.p.Ports.Answers(sctx, host, p) == nil {
			return model.Finding{Key: "find.port", Level: model.FindingUrgent, Args: []string{portArg(s.Port), portArg(p)}, FixPort: p}, true
		}
	}
	return model.Finding{}, false
}

// unreachableExternal은 외부 연결 대상에 웹서버가 닿지 못하는 까닭을 찾는다 — 진단만 하고 알리지는 않는다 (M6: 외부 연결은 감시하지 않는다).
//   - "이 서버"의 포트: Naru도 웹서버와 같은 길(host.docker.internal)로 연결해 본다. 127.0.0.1에만 열린 포트는 이 길로 닿지 않는다.
//   - 다른 컨테이너: 웹서버가 닿게 하지 못했다고 맞추기가 알려 온 것(기본 bridge·host 네트워크에만 있음).
//
// 그 밖의 바깥 주소(NAS 등)는 찔러 보지 않는다.
func (d *diagnoser) unreachableExternal(ctx context.Context, external string) (model.Finding, bool) {
	host, port, err := net.SplitHostPort(strings.TrimPrefix(external, "https://"))
	if err != nil {
		return model.Finding{}, false
	}
	if host == "host.docker.internal" {
		p, err := model.ParsePort(port)
		if err != nil || p == 0 {
			return model.Finding{}, false
		}
		actx, cancel := context.WithTimeout(ctx, answerAfter)
		defer cancel()
		if d.p.Ports.Answers(actx, host, p) != nil {
			return model.Finding{Key: "find.hostport", Level: model.FindingUrgent, Args: []string{port, d.bridgeIP(ctx)}}, true
		}
		return model.Finding{}, false
	}
	for _, pr := range d.p.WebServer.Status().Unreachable {
		if pr.Host == host && pr.Why == "defaultbridge" {
			return model.Finding{Key: "find.bridge", Level: model.FindingUrgent, Args: []string{host, d.p.AppNetwork}}, true
		}
	}
	return model.Finding{}, false
}

// bridgeIP는 컨테이너에서 본 "이 서버"의 주소(Docker 브리지 IP)다 — 서버마다 다르다 (172.17.0.1, 172.18.0.1 …).
// 포트를 이 IP에 열면 컨테이너에서는 닿고 바깥에는 열리지 않는다. 조회하지 못하면 흔한 값을 쓴다.
func (d *diagnoser) bridgeIP(ctx context.Context) string {
	if name, err := model.ParseDomainName("host.docker.internal"); err == nil {
		if ips := d.lookup(ctx, name, d.p.Clock.Now()).IPs; len(ips) > 0 {
			return ips[0]
		}
	}
	return "172.17.0.1"
}

// dnsFindings는 서비스 주소가 이 서버를 가리키는지 본다. 이 서버의 IP를 모르면 보지 않는다.
// 쓸 수 있는 인증서가 있는 주소는 건너뛴다 — 이미 여기로 온다는 뜻이고, 프록시를 쓰는 주소를 잘못 짚지 않는다.
func (d *diagnoser) dnsFindings(ctx context.Context, s model.Service) []model.Finding {
	if len(s.Domains) == 0 {
		return nil
	}
	here := d.thisServer(ctx)
	if len(here) == 0 {
		return nil
	}
	now := d.p.Clock.Now()
	certs := d.certificates(ctx, now)
	var out []model.Finding
	for _, dom := range s.Domains {
		if dom.HTTPS && certs.For(dom.Domain, now).Usable(now) {
			continue
		}
		a := d.lookup(ctx, dom.Domain, now)
		switch {
		case !a.Resolves:
			out = append(out, model.Finding{Key: "find.dns.none", Level: model.FindingWarning, Args: []string{dom.Domain.String(), strings.Join(here, ", ")}})
		case !overlaps(a.IPs, here):
			out = append(out, model.Finding{Key: "find.dns.elsewhere", Level: model.FindingWarning, Args: []string{dom.Domain.String(), strings.Join(here, ", "), a.IPList()}})
		}
	}
	return out
}

// thisServer는 이 서버의 IP다 — NARU_PUBLIC_IP, 없으면 관리 주소를 조회한 IP.
func (d *diagnoser) thisServer(ctx context.Context) []string {
	if d.p.PublicIP != "" {
		return []string{d.p.PublicIP}
	}
	admin, _ := d.p.Admin.Get(ctx)
	if admin.IsZero() {
		return nil
	}
	return d.lookup(ctx, admin, d.p.Clock.Now()).IPs
}

func (d *diagnoser) lookup(ctx context.Context, name model.DomainName, now time.Time) model.DNSAnswer {
	d.mu.Lock()
	seen, ok := d.dns[name.String()]
	d.mu.Unlock()
	if ok && now.Sub(seen.at) < dnsKeep {
		return seen.a
	}
	a := d.p.DNS.PointsHere(ctx, name, "")
	d.mu.Lock()
	d.dns[name.String()] = seenDNS{a: a, at: now}
	d.mu.Unlock()
	return a
}

func (d *diagnoser) certificates(ctx context.Context, now time.Time) model.Certificates {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.certsAt) >= certsKeep {
		d.certs, _ = d.p.Certs.Read(ctx) // 못 읽으면 인증서가 없는 것으로 본다
		d.certsAt = now
	}
	return d.certs
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// reach는 Naru가 컨테이너의 앱에 닿는 주소다 — Naru도 같은 네트워크에 있어 컨테이너 이름으로 닿는다.
func reach(s model.Service) string { return s.Live.CurrentContainer() }

// portArg는 문구에 끼울 포트다.
func portArg(p model.Port) string { return strconv.Itoa(int(p)) }
