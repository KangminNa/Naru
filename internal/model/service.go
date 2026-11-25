package model

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type (
	ServiceID    int64
	DomainID     int64
	DeploymentID int64
	AccountID    int64
)

// KindName은 서비스 종류다. 종류마다 다른 행동은 kinds 패키지만 안다.
type KindName string

const (
	KindRepo     KindName = "repo"
	KindCompose  KindName = "compose"
	KindImage    KindName = "image"
	KindStatic   KindName = "static"
	KindExternal KindName = "external"
)

// AllKinds는 화면에 보여줄 순서다.
func AllKinds() []KindName {
	return []KindName{KindRepo, KindCompose, KindImage, KindStatic, KindExternal}
}

// ParseKindName은 아는 종류 이름인지 본다.
func ParseKindName(s string) (KindName, error) {
	for _, k := range AllKinds() {
		if string(k) == s {
			return k, nil
		}
	}
	return "", InputError{Field: "kind", Code: "kind"}
}

// ServiceInput은 화면에서 받은 서비스 값이다. 모양 검사는 값 객체가 이미 했다.
type ServiceInput struct {
	Kind        KindName
	Name        ServiceName // 비면 NameChooser가 짓는다
	Repo        RepoURL
	Branch      Branch
	Image       ImageRef
	External    ExternalAddress
	Folder      FolderPath
	BuildPath   FolderPath
	BuildTarget BuildTarget // 저장소 종류만 — Dockerfile의 단계 이름
	Port        Port
	Domain      DomainName // 비어도 된다
	Token       string     // 비면 그대로 둔다
	ClearToken  bool
	Env         EnvVars
	EnvFile     EnvFile // 저장소 종류만 — 빌드 폴더의 .env
	Volumes     Volumes
	Networks    NetworkNames // 컨테이너 종류만 — 앱 네트워크 말고 더 붙일 네트워크
	AutoDeploy  bool

	// Compose만
	ComposeFile    FolderPath
	ComposeProject ProjectName
	Target         DomainTarget // 처음 붙이는 주소의 대상

	// 돌고 있는 컨테이너를 넘겨받을 때만
	EnvFrom  EnvFrom   // 그 컨테이너에서 가져올 환경 변수 (이름만 — 값은 서비스 쪽이 읽는다)
	TakeOver ServiceID // 배포가 성공하면 이 외부 연결 서비스의 주소와 웹서버 설정을 옮겨 온다 (0이면 안 옮김)
}

// EnvFrom은 다른 컨테이너에서 가져올 환경 변수다 — 값은 화면을 거치지 않는다.
type EnvFrom struct {
	Container string
	Names     []string
}

// Source는 이 입력의 소스를 글자로 — 저장소 주소나 이미지 이름. 종류가 정한 칸만 채워져 있다.
func (in ServiceInput) Source() string {
	if !in.Repo.IsZero() {
		return in.Repo.String()
	}
	return in.Image.String()
}

// Domain은 서비스에 붙은 주소 하나다.
type Domain struct {
	ID     DomainID
	Domain DomainName
	HTTPS  bool
	HSTS   bool
	Target DomainTarget // Compose: 이 주소가 가리키는 서비스·포트 (비면 서비스의 앱)
}

// DomainTarget은 주소가 가리키는 것이다 — Compose 서비스 이름과 포트. 비면 서비스의 앱.
type DomainTarget struct {
	Service string
	Port    Port
}

func (t DomainTarget) IsZero() bool { return t.Service == "" }

var composeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// ParseDomainTarget은 compose 서비스 이름과 포트를 받는다. 이름이 비면 대상이 없다 (서비스의 앱). 이름이 있으면 포트가 있어야 한다.
func ParseDomainTarget(service, port string) (DomainTarget, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		return DomainTarget{}, nil
	}
	bad := InputError{Field: "target", Code: "target"}
	if !composeNamePattern.MatchString(service) {
		return DomainTarget{}, bad
	}
	p, err := ParsePort(port)
	if err != nil || p == 0 {
		return DomainTarget{}, bad
	}
	return DomainTarget{Service: service, Port: p}, nil
}

// ProjectName은 compose 프로젝트 이름이다 (docker compose -p) — 볼륨·네트워크 이름이 이것에 묶인다.
type ProjectName struct{ v string }

var projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ParseProjectName은 비어도 된다 (그러면 naru-<서비스 이름>).
func ParseProjectName(s string) (ProjectName, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ProjectName{}, nil
	}
	if !projectNamePattern.MatchString(s) {
		return ProjectName{}, InputError{Field: "compose_project", Code: "project"}
	}
	return ProjectName{s}, nil
}

func (p ProjectName) String() string { return p.v }

// ComposeProject는 배포할 compose 하나다.
type ComposeProject struct {
	Name string // docker compose -p
	Dir  string // 작업 폴더 — 서버와 Naru 컨테이너에서 같은 경로
	File string // compose 파일 (작업 폴더 기준, 비면 compose가 기본 이름을 찾는다)
}

// Container는 그 서비스의 컨테이너 이름이다 — compose가 붙이는 이름 (<프로젝트>-<서비스>-1). 다시 만들어도 같다.
func (p ComposeProject) Container(service string) string { return p.Name + "-" + service + "-1" }

// ComposeService는 compose가 띄운 서비스 하나다 — 서비스 화면의 목록.
type ComposeService struct {
	Name      string
	Container string
	State     string
	Status    string
	Ports     []Port
	Ended     bool // 한 번 돌고 끝나는 것(마이그레이션 등)이 잘 끝났다 — 문제가 아니다
	Domains   int  // 이 서비스를 가리키는 주소 수
}

// DomainInput은 화면에서 붙이는 주소다.
type DomainInput struct {
	Domain DomainName
	HTTPS  bool
	Target DomainTarget
}

// Service는 서비스 하나의 지금 모습이다. 비밀은 없다.
type Service struct {
	ID             ServiceID
	Name           ServiceName
	Kind           KindName
	Source         string // 저장소 주소 또는 이미지 이름 (저장할 때 이미 검사됨)
	Branch         string
	BuildPath      string
	BuildTarget    string
	Folder         string
	External       string
	Port           Port
	Networks       string // 앱 네트워크 말고 더 붙일 네트워크 이름들 (빈칸으로 나눔)
	AutoDeploy     bool
	TakesOver      ServiceID // 다음 배포가 성공하면 이 서비스의 주소와 웹서버 설정을 옮겨 온다 (0이면 없음)
	ComposeFile    string    // Compose: 작업 폴더 기준 compose 파일 (비면 기본 이름)
	ComposeProject string    // Compose: 프로젝트 이름 (비면 naru-<이름>) — Project()로 읽는다
	Live           LiveState
	HookLog        HookLog
	Domains        []Domain
	Created        time.Time
}

// Project는 Compose 프로젝트 이름이다 — 정하지 않았으면 naru-<서비스 이름>.
func (s Service) Project() string {
	if s.ComposeProject != "" {
		return s.ComposeProject
	}
	return "naru-" + s.Name.String()
}

// FromGit은 저장소에서 코드를 받는 서비스인가 — 소스가 저장소 주소다. git push 웹훅으로 배포한다.
func (s Service) FromGit() bool {
	_, err := ParseRepoURL(s.Source)
	return err == nil
}

// PrimaryDomain은 먼저 붙인 주소다.
func (s Service) PrimaryDomain() string {
	if len(s.Domains) == 0 {
		return ""
	}
	return s.Domains[0].Domain.String()
}

// LiveState는 지금 도는 것이다.
type LiveState struct {
	Alias    string // 네트워크에서 서비스를 부르는 이름 — 웹서버가 여기로 보낸다
	Instance string // 지금 요청을 받는 실제 컨테이너
	Release  string // 정적 사이트: 지금 서빙 중인 배포 번호
	Port     Port
	Stopped  bool // 직접 멈췄다 — 장애가 아니다
}

// CurrentContainer는 지금 요청을 받는 컨테이너다 (v1에서 넘겨받은 것은 Alias 그 자체).
func (l LiveState) CurrentContainer() string {
	if l.Instance != "" {
		return l.Instance
	}
	return l.Alias
}

// LiveDeployment는 지금 서빙 중인 배포 번호다 — 컨테이너 이름 끝의 번호나 정적 배포본.
func (l LiveState) LiveDeployment() DeploymentID {
	if l.Release != "" {
		n, _ := strconv.ParseInt(l.Release, 10, 64)
		return DeploymentID(n)
	}
	return ContainerDeployment(l.Instance)
}

// ContainerDeployment는 Naru가 붙인 컨테이너 이름("naru-blog-12")의 배포 번호다. 번호가 없으면(v1에서 온 것) 0.
func ContainerDeployment(name string) DeploymentID {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '-' {
			n, _ := strconv.ParseInt(name[i+1:], 10, 64)
			return DeploymentID(n)
		}
	}
	return 0
}

// NewService는 저장할 새 서비스다.
type NewService struct {
	ID             ServiceID // 0이 아니면 그 번호 (v1 앱 번호를 지켜 웹훅 주소를 유지)
	Name           ServiceName
	Kind           KindName
	Source         string
	Branch         string
	BuildPath      string
	BuildTarget    string
	Folder         string
	External       string
	Port           Port
	Networks       string
	AutoDeploy     bool
	Alias          string
	TakesOver      ServiceID
	ComposeFile    string
	ComposeProject string
}

// ServiceSettings는 고칠 수 있는 값이다.
type ServiceSettings struct {
	Source      string
	Branch      string
	BuildPath   string
	BuildTarget string
	Folder      string
	External    string
	Port        Port
	Networks    string
	AutoDeploy  bool
	ComposeFile string // 프로젝트 이름은 바꾸지 않는다 — 바꾸면 돌던 컨테이너·볼륨과 떨어진다
}

// ServiceSecrets는 배포에만 쓰는 비밀이다. 화면 계층은 이 타입을 쓰지 않는다.
type ServiceSecrets struct {
	Env           EnvVars
	EnvFile       EnvFile
	Volumes       Volumes
	GitToken      string
	WebhookSecret string
}

// HookLog는 웹훅을 마지막으로 받은 때와 결과다.
type HookLog struct {
	At     time.Time
	Result string
}
