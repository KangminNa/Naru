package model

import (
	"strconv"
	"strings"
	"time"
)

type DeployStatus string

const (
	DeployRunning DeployStatus = "running"
	DeploySuccess DeployStatus = "success"
	DeployFailed  DeployStatus = "failed"
)

// DeployReason은 왜 배포했는가다.
type DeployReason string

const (
	ReasonManual   DeployReason = "manual"
	ReasonPush     DeployReason = "webhook"
	ReasonCreate   DeployReason = "create"
	ReasonRollBack DeployReason = "rollback"
)

// Deployment는 배포 한 번의 기록이다.
type Deployment struct {
	ID         DeploymentID
	ServiceID  ServiceID
	Status     DeployStatus
	Reason     DeployReason
	Commit     string
	Message    string
	Image      ImageID
	Log        string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Duration은 걸린 시간이다. 끝나지 않았으면 지금까지.
func (d Deployment) Duration() time.Duration {
	end := d.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(d.StartedAt).Round(time.Second)
}

// ShortCommit은 커밋 앞 7자리다.
func (d Deployment) ShortCommit() string {
	if len(d.Commit) > 7 {
		return d.Commit[:7]
	}
	return d.Commit
}

// ImageID는 Docker 이미지 ID다.
type ImageID string

// ImageTag는 빌드한 이미지의 이름이다 — 서비스 이름과 배포 번호. 되돌리기와 정리가 이 이름으로 찾는다.
func ImageTag(n ServiceName, d DeploymentID) string {
	return "naru-" + n.String() + ":" + strconv.FormatInt(int64(d), 10)
}

// Commit은 가져온 커밋이다.
type Commit struct {
	Hash    string
	Message string
}

// Version은 배포할 수 있게 만든 것이다 — 이미지 또는 정적 파일 폴더.
type Version struct {
	Image  ImageID
	Folder SiteFolder
	Commit Commit
	Port   Port // 찾은 앱 포트 (0이면 서비스 설정을 따른다)
}

// SiteFolder는 정적 사이트 배포본 하나다.
type SiteFolder struct {
	Site    ServiceName
	Release DeploymentID
}

func (f SiteFolder) ReleaseName() string { return strconv.FormatInt(int64(f.Release), 10) }

// BuildRequest는 새 버전 만들기에 넘기는 값이다.
type BuildRequest struct {
	Service    Service
	Deployment DeploymentID
}

// SwapRequest는 바꿔 끼우기에 넘기는 값이다.
type SwapRequest struct {
	Service    Service
	Deployment DeploymentID
}

// CodeSource는 내려받을 코드다.
type CodeSource struct {
	Repo   RepoURL
	Branch Branch
	Token  string
	Commit string // 비면 브랜치의 최신 — 있으면 그 커밋 (되돌리기)
}

// ImageDetails는 받은 이미지의 ID와 열어둔 포트다.
type ImageDetails struct {
	ID    ImageID
	Ports []Port
}

// ContainerSpec은 띄울 컨테이너다.
type ContainerSpec struct {
	Name    string
	Image   ImageID
	Env     []string
	Binds   []string
	Network string
	Aliases []string
	Service ServiceID
	Deploy  DeploymentID

	ExtraNetworks []string // 앱 네트워크 말고 더 붙일 네트워크 — 같은 별칭으로, 띄우기 전에 붙인다
}

// ContainerState는 컨테이너 하나의 상태다.
type ContainerState struct {
	Name      string
	Running   bool
	State     string // running · restarting · exited · created · paused · dead
	Status    string // "Up 3 hours"
	ExitCode  int
	Ports     []Port // 이미지가 연 포트 (EXPOSE) — 진단이 앱 포트 대신 응답하는 포트를 찾을 때
	Published []Port // 서버에 연 포트 (-p) — All만 채운다

	// One만 채운다 — Docker가 다시 시작한 횟수, 마지막으로 죽은 것이 메모리 부족이었는지
	Restarts  int
	OOMKilled bool

	// 아래는 All이 채운다 (One은 채우지 않는다) — 다른 컨테이너 목록이 쓴다.
	Image          string
	Project        string          // compose 묶음 이름 (com.docker.compose.project) — 없으면 빈 값
	ComposeService string          // compose 서비스 이름 (com.docker.compose.service)
	DependsOn      string          // compose가 적은 기다리는 것들 ("migrate:service_completed_successfully:false,db:service_started:false")
	ImageID        string          // 이미지 ID (sha256:…)
	Networks       []DockerNetwork // 붙은 네트워크 (이름순)
	Service        ServiceID       // Naru가 띄운 것이면 그 서비스 (naru.service 라벨) — 아니면 0
}

// Ended는 한 번 돌고 잘 끝난 컨테이너인가 (마이그레이션 같은 것) — 멈춘 것이지만 문제가 아니다.
// Finished는 그 컨테이너가 한 번 돌고 잘 끝난 것(마이그레이션 같은 것)인가 — 다시 띄우지 않고, 멈춘 것으로 세지 않는다.
// 종료 코드 0만으로는 모른다(멈추라고 해서 깔끔히 끝난 DB도 0이다). compose 묶음의 다른 서비스가 이것을
// depends_on: service_completed_successfully로 기다리는 것만 그렇다 (compose가 com.docker.compose.depends_on 라벨에 적어 둔다).
func (cs ContainerStates) Finished(name string) bool {
	c, ok := cs[name]
	if !ok || c.Project == "" || c.ComposeService == "" || !EndedOK(c.State, c.Status) {
		return false
	}
	for _, o := range cs {
		if o.Project != c.Project {
			continue
		}
		for _, dep := range strings.Split(o.DependsOn, ",") {
			svc, rest, _ := strings.Cut(dep, ":")
			cond, _, _ := strings.Cut(rest, ":")
			if svc == c.ComposeService && cond == "service_completed_successfully" {
				return true
			}
		}
	}
	return false
}

// ContainerStates는 이름 → 상태. nil이면 Docker를 읽지 못했다.
type ContainerStates map[string]ContainerState
