package model

import (
	"fmt"
	"strconv"
	"time"
)

// HumanBytes는 크기를 사람이 읽는 글자로 — "21.4 GB" (1024 단위).
func HumanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// ── 계정 ─────────────────────────────────

// Account는 관리자 한 명이다.
type Account struct {
	ID       AccountID
	Username Username
}

// PasswordHash는 "salt:hash" (v1과 같은 모양). 화면 계층은 이 타입을 쓰지 않는다.
type PasswordHash string

// SessionToken은 쿠키에만 있는 원문이다. 저장할 때는 SessionDigest로만.
type SessionToken string

// SessionDigest는 세션 토큰의 SHA-256이다.
type SessionDigest string

// ── 웹훅 ─────────────────────────────────

// HookRequest는 받은 웹훅이다.
type HookRequest struct {
	Headers map[string]string // 소문자 키
	Query   map[string]string
	Body    []byte
	Ref     string // push가 가리키는 ref ("refs/heads/main") — 받는 쪽이 본문에서 읽어 채운다
}

// HookResult는 웹훅을 처리한 결과다.
type HookResult struct {
	Status     int // HTTP 상태
	Message    string
	Deployment DeploymentID
}

// ── 화면에 보여줄 모습 ───────────────────────

// ServiceStatus는 화면 상태다. Key는 문구 키, Tone은 ok·warn·bad·muted.
type ServiceStatus struct {
	Key    string
	Tone   string
	Detail string // Docker가 말하는 상태 ("Up 3 hours")
}

// ServiceCard는 홈의 카드 하나다.
type ServiceCard struct {
	ID     ServiceID
	Name   string
	Kind   KindName
	Domain string
	Status ServiceStatus
	Usage  *ResourceUsage // 지켜보기가 읽은 CPU·메모리 (실행 중인 컨테이너만, 없으면 nil)
}

// OtherContainer는 Naru가 띄우지 않은, 이 서버의 컨테이너 하나다. Naru는 보기만 한다.
type OtherContainer struct {
	Name       string
	Image      string
	Running    bool
	Status     string // "Up 3 days", "Exited (0) 5 days ago"
	Ports      []Port
	Project    string    // compose 묶음 — 없으면 빈 값
	Networks   []string  // 사용자 정의 네트워크 (이름순)
	OnlyBridge bool      // 돌고 있는데 사용자 정의 네트워크에 없다 — 웹서버가 이름으로 닿지 못한다
	Linked     ServiceID // 이 컨테이너로 보내는 외부 연결 서비스 (없으면 0)
	LinkedName string
}

// Upstream은 외부 연결 칸에 적을 "이름:포트"다. 포트를 모르면 "이름:".
func (c OtherContainer) Upstream() string {
	if len(c.Ports) > 0 {
		return c.Name + ":" + strconv.Itoa(int(c.Ports[0]))
	}
	return c.Name + ":"
}

// ContainerSeed는 돌고 있는 컨테이너를 저장소 서비스로 옮길 때 화면에 미리 채울 값이다. 환경 변수 값은 없다.
type ContainerSeed struct {
	Name         string
	Port         Port
	Volumes      string    // "/서버경로:/컨테이너경로" 줄들 — 호스트 경로를 붙인 것만
	NamedVolumes []string  // 옮기지 않는 것 — 이름 있는 볼륨 ("pgdata:/var/lib/data")
	Networks     string    // 앱 네트워크 말고 붙어 있는 사용자 정의 네트워크 (빈칸으로 나눔)
	Env          []SeedEnv // 이름만 (이름순)
	Repo         string    // 이미지 라벨이 알려 준 저장소 주소 — 없으면 빈 값

	Linked        ServiceID // 이 컨테이너로 보내는 외부 연결 서비스 (없으면 0) — 성공하면 이 서비스의 주소를 옮길 수 있다
	LinkedName    string
	LinkedDomains []string
}

// SeedEnv는 컨테이너의 환경 변수 이름 하나다.
type SeedEnv struct {
	Name      string
	FromImage bool // 이미지가 정한 값 그대로 (PATH 같은 것) — 새 이미지도 스스로 정하므로 기본으로 가져오지 않는다
}

// ContainerDetails는 도구가 읽은 컨테이너 하나다. Env의 값은 비밀이다 — 화면 계층은 이 타입을 쓰지 않는다 (R10).
type ContainerDetails struct {
	Env          []string // "KEY=value"
	ImageEnv     []string // 이미지가 정한 "KEY=value"
	Binds        []string // 호스트 경로를 붙인 것 — "/서버경로:/컨테이너경로[:ro]"
	NamedVolumes []string // "이름:/컨테이너경로"
	Source       string   // 이미지 라벨 org.opencontainers.image.source
}

// HomeView는 홈 화면이다.
type HomeView struct {
	Cards      []ServiceCard
	DockerDown bool
	Findings   []Finding // 주의가 필요한 것 — 무거운 것부터
	Checked    bool      // 지켜보기가 한 번이라도 봤다 (아니면 "모두 정상"이라 말하지 않는다)
}

// WebhookView는 웹훅 설정에 필요한 값이다 (시크릿은 GitHub에 넣어야 하므로 보여준다).
type WebhookView struct {
	URL    string // 관리 주소가 없으면 빈 값
	Secret string
}

// ServiceForm은 설정 폼에 미리 채울 값이다. 토큰 원문은 없다.
type ServiceForm struct {
	Source      string
	Branch      string
	BuildPath   string
	BuildTarget string
	Folder      string
	External    string
	Port        Port
	AutoDeploy  bool
	EnvText     string
	EnvFileText string
	Volumes     string
	Networks    string
	HasToken    bool
}

// DomainView는 서비스 상세의 주소 하나와 그 인증서 상태다.
type DomainView struct {
	Domain      Domain
	Certificate CertificateState
}

// ServiceView는 서비스 상세 화면이다.
type ServiceView struct {
	Service    Service
	Domains    []DomainView
	Status     ServiceStatus
	Target     string
	Container  bool // 멈추기·켜기가 된다
	Deployable bool // 배포가 있다
	Deploys    []Deployment
	LiveID     DeploymentID
	Deploying  bool
	Webhook    WebhookView
	Form       ServiceForm
	Web        WebSettingsView
	Findings   []Finding      // 지켜보기가 찾은 이 서비스의 문제
	Usage      *ResourceUsage // CPU·메모리 (없으면 nil)
}

// StepView는 배포 단계 하나다. State: done · doing · failed
type StepView struct {
	Ko, En string
	State  string
}

// DeploymentView는 배포 화면이다.
type DeploymentView struct {
	Service    Service
	Deployment Deployment
	Steps      []StepView
}

// ServerSnapshot은 서버의 CPU·메모리·디스크다. 읽지 못한 값은 Has…가 거짓.
type ServerSnapshot struct {
	Cores      int
	CPUPercent float64
	HasCPU     bool
	Load1      float64
	HasLoad    bool
	MemTotal   uint64
	MemUsed    uint64
	HasMem     bool
	DiskTotal  uint64
	DiskFree   uint64
	HasDisk    bool
	Uptime     time.Duration
}

// ── 일어난 일 ─────────────────────────────

// Event는 일어난 일이다. 아래 타입들만 Event다.
type Event interface{ event() }

// SiteMapChanged: 주소·포트·서빙 폴더·관리 주소·첫 설정 상태가 바뀌었다.
type SiteMapChanged struct{ Reason string }

// DeployFinished: 배포가 끝났다.
type DeployFinished struct {
	Service    ServiceID
	Deployment DeploymentID
	OK         bool
}

// ServiceDeleted: 서비스를 지웠다.
type ServiceDeleted struct{ Service ServiceID }

func (SiteMapChanged) event() {}
func (DeployFinished) event() {}
func (ServiceDeleted) event() {}
