// Package kinds는 서비스 종류(저장소·이미지·정적 사이트·외부 연결)마다 다른 일을 맡는다.
// 종류에 따른 분기는 이 패키지에만 있다 — 다른 곳은 KindLookup으로 담당자 묶음을 받아 쓴다.
package kinds

import (
	"errors"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// Tools는 종류 담당자들이 쓰는 도구다. 모두 인터페이스다.
type Tools struct {
	Work       contract.WorkFolder
	Code       contract.CodeDownloader
	Secrets    contract.SecretStore
	Dockerfile contract.DockerfilePortReader
	Packer     contract.BuildContextPacker
	Builder    contract.ImageBuilder
	Puller     contract.ImagePuller
	Images     contract.ImageCleaner
	Starter    contract.ContainerStarter
	Remover    contract.ContainerRemover
	Switch     contract.ContainerSwitch
	Watcher    contract.ContainerWatcher
	Ports      contract.PortChecker
	Files      contract.SiteFiles
	Compose    contract.ComposeRunner      // Compose 종류 — docker compose
	Projects   contract.ProjectFolders     // Compose 작업 폴더 (서버와 같은 경로) — 없으면 Compose 종류를 막는다
	ComposeOn  bool                        // Compose를 쓸 수 있게 설정됐는가 (NARU_HOST_DATA)
	Joiner     contract.NetworkJoiner      // Compose 주소 대상을 앱 네트워크에 붙인다 (웹서버·Naru가 이름으로 닿게)
	AppLogs    contract.ContainerLogReader // 옛 컨테이너를 지우기 전에 앱 출력을 읽는다
	Archive    contract.LogArchive         // 그것을 남긴다 (없으면 남기지 않는다)

	Network    string // 앱 컨테이너가 붙는 네트워크
	SitesShown string // 웹서버 컨테이너에서 본 정적 사이트 폴더 (기본 /srv/sites)

	ReadyTimeout   time.Duration // 새 컨테이너가 응답하기를 기다리는 시간 (기본 60초)
	ComposeTimeout time.Duration // Compose 주소 대상이 응답하기를 기다리는 시간 (기본 5분 — db → 마이그레이션 → 앱 순서라 길다)
	PollEvery      time.Duration // 그 사이 확인 간격 (기본 0.5초)
}

// kindLookup은 종류 이름 → 담당자 묶음이다.
type kindLookup map[model.KindName]contract.KindTools

// NewLookup은 다섯 종류의 담당자 묶음을 만든다.
func NewLookup(t Tools) contract.KindLookup {
	if t.ReadyTimeout == 0 {
		t.ReadyTimeout = 60 * time.Second
	}
	if t.PollEvery == 0 {
		t.PollEvery = 500 * time.Millisecond
	}
	if t.ComposeTimeout == 0 {
		t.ComposeTimeout = 5 * time.Minute
	}
	swapper := ContainerSwapper{t}
	container, external := ContainerAliasDestination{}, ExternalDestination{}
	status := ContainerStatus{}
	return kindLookup{
		model.KindRepo: {
			Input: RepoInput{}, Builder: RepoBuilder{t}, Swapper: swapper, Destination: container, Status: status,
		},
		model.KindCompose: {
			Input: ComposeInput{on: t.ComposeOn}, Builder: ComposeBuilder{t}, Swapper: ComposeSwapper{t}, Destination: ComposeDestination{},
			Status: ComposeStatus{}, Runtime: ComposeRuntime{t},
		},
		model.KindImage: {
			Input: ImageInput{}, Builder: ImageFetcher{t}, Swapper: swapper, Destination: container, Status: status,
		},
		model.KindStatic: {
			Input: StaticInput{}, Builder: StaticBuilder{t}, Swapper: FolderSwapper{t}, Destination: FolderDestination{t.SitesShown}, Status: StaticStatus{},
		},
		model.KindExternal: {
			Input: ExternalInput{}, Destination: external, Status: ExternalStatus{},
		},
	}
}

func (l kindLookup) Find(k model.KindName) (contract.KindTools, bool) {
	tools, ok := l[k]
	return tools, ok
}

// explained는 배포 기록에 남길 사람 말 문구와, 객체들이 errors.Is로 알아볼 실패를 함께 갖는다.
type explained struct {
	is   error
	text string
}

func (e explained) Error() string { return e.text }
func (e explained) Unwrap() error { return e.is }

func explain(is error, text string) error { return explained{is, text} }

var errGone = errors.New("the files or image of that deployment are gone")
