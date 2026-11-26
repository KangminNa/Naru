package model

import (
	"strconv"
	"strings"
	"time"
)

// ── 컨테이너 화면 — 이 서버의 모든 컨테이너 ─────────────

// ContainerAction은 컨테이너 하나 또는 compose 묶음에 하는 동작이다.
type ContainerAction string

const (
	ActRestart ContainerAction = "restart"
	ActStop    ContainerAction = "stop"
	ActStart   ContainerAction = "start"
	ActRemove  ContainerAction = "remove" // 컨테이너 하나만, 멈춘 것만
)

// ParseContainerAction은 아는 동작인지 본다.
func ParseContainerAction(s string) (ContainerAction, error) {
	switch a := ContainerAction(s); a {
	case ActRestart, ActStop, ActStart, ActRemove:
		return a, nil
	}
	return "", InputError{Field: "action", Code: "action"}
}

// 동작을 막는 이유 (ContainerRow.Locked · ContainerGroup.Locked) — 화면 문구 키 containers.locked.<이유>
const (
	LockedNaru      = "naru"      // Naru 자신
	LockedWebServer = "webserver" // 웹서버 — 멈추면 모든 사이트가 끊긴다
	LockedService   = "service"   // Naru 서비스의 것 — 서비스 화면에서 다룬다
)

// ContainerRow는 컨테이너 화면 목록의 한 줄이다.
type ContainerRow struct {
	Name, Image, State, Status string
	Health                     string         // healthy · unhealthy · starting · "" (검사 없음)
	Ports                      []Port         // 서버에 연 포트 (바깥에서 닿는다)
	Exposed                    []Port         // 이미지가 연 포트 (같은 네트워크에서만)
	Networks                   []string       // 사용자 정의 네트워크 (이름순)
	OnlyBridge                 bool           // 돌고 있는데 사용자 정의 네트워크에 없다 — 웹서버가 이름으로 닿지 못한다
	Usage                      *ResourceUsage // 지켜보기가 마지막으로 읽은 값 (없으면 nil)
	Ended                      bool           // 한 번 돌고 잘 끝난 것 (Exited (0))
	Service                    ServiceID      // Naru 서비스의 것이면 그 번호 — 동작은 서비스 화면에서
	ServiceName                string
	Linked                     ServiceID // 이 컨테이너로 보내는 외부 연결 서비스 (없으면 0)
	LinkedName                 string
	Domains                    []string // 이 컨테이너로 보내는 주소
	Locked                     string   // 동작을 막는 이유 (Locked…) — 비면 동작할 수 있다
}

// Running은 돌고 있는가.
func (r ContainerRow) Running() bool { return r.State == "running" }

// Upstream은 외부 연결 칸에 적을 "이름:포트"다. 포트를 모르면 "이름:".
func (r ContainerRow) Upstream() string {
	ports := r.Exposed
	if len(ports) == 0 {
		ports = r.Ports
	}
	if len(ports) > 0 {
		return r.Name + ":" + strconv.Itoa(int(ports[0]))
	}
	return r.Name + ":"
}

// ContainerGroup은 compose 묶음 하나다 (Project가 비면 따로 띄운 것).
type ContainerGroup struct {
	Project string
	Rows    []ContainerRow
	Locked  string // 묶음 동작을 막는 이유 (Naru Compose 서비스의 묶음, Naru 자신이 든 묶음)
	Running int
}

// ContainerView는 컨테이너 상세다. 환경 변수는 이름만 — 값은 이 타입에 없다 (R10).
type ContainerView struct {
	Row       ContainerRow
	Project   string
	Started   time.Time
	Restarts  int
	Mounts    []string // "/서버경로 → /컨테이너경로", "볼륨이름 (볼륨) → /경로"
	EnvNames  []string
	HealthLog string    // 상태 검사의 마지막 출력 (짧게)
	Logs      []LogLine // 최근 줄 — Query로 거른 것
	Query     string
}

// HealthOf는 Docker 상태 글("Up 2 hours (unhealthy)")에서 상태 검사 결과를 꺼낸다. 검사가 없으면 빈 값.
func HealthOf(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// EndedOK는 한 번 돌고 잘 끝난 컨테이너인가 (마이그레이션 같은 것) — 다시 띄우지 않는다.
func EndedOK(state, status string) bool {
	return state == "exited" && strings.HasPrefix(status, "Exited (0)")
}

// ── 이미지 · 볼륨 ─────────────────────────

// DockerDisk는 이미지·볼륨과 크기다 (docker system df).
type DockerDisk struct {
	Images  []DiskImage
	Volumes []DiskVolume
}

// DiskImage는 이미지 하나다.
type DiskImage struct {
	ID      ImageID
	Tags    []string
	Size    uint64
	Created time.Time
	UsedBy  int // 이 이미지로 만든 컨테이너 수 (멈춘 것 포함)
}

// Short는 화면과 확인 칸에 쓰는 ID 앞 12자다.
func (i DiskImage) Short() string { return ShortImageID(i.ID) }

// ShortImageID는 "sha256:"을 뗀 ID 앞 12자다.
func ShortImageID(id ImageID) string {
	s := strings.TrimPrefix(string(id), "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// DiskVolume은 볼륨 하나다.
type DiskVolume struct {
	Name    string
	Size    uint64 // 모르면 0
	UsedBy  int    // 붙어 있는 컨테이너 수 (멈춘 것 포함)
	Project string // compose 묶음 라벨
}

// StorageView는 이미지·볼륨 화면이다. 안 쓰는 것을 위로, 큰 것부터.
type StorageView struct {
	Disk           DockerDisk
	UnusedImages   uint64 // 안 쓰는 이미지 크기 합
	UnusedVolumes  uint64
	DockerReadable bool
}
