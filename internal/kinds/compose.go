package kinds

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// ── Compose — 저장소의 compose 파일을 통째로 ─────────────
//
// 무중단 교체는 없다 — compose가 바뀐 컨테이너를 다시 만든다. 배포 성공의 기준은 Dockerfile 방식과 같다:
// 주소가 가리키는 서비스의 포트가 응답해야 한다.

// project는 그 서비스의 compose 프로젝트다.
func project(t Tools, s model.Service) model.ComposeProject {
	return model.ComposeProject{Name: s.Project(), Dir: t.Projects.Dir(s.Name), File: s.ComposeFile}
}

// checkout은 작업 폴더에 코드를 받고(커밋을 정하면 그 커밋) .env 파일을 쓴다.
func checkout(ctx context.Context, t Tools, s model.Service, commit string, log contract.DeployLogWriter) (model.Commit, error) {
	repo, err := model.ParseRepoURL(s.Source)
	if err != nil {
		return model.Commit{}, err
	}
	branch, _ := model.ParseBranch(s.Branch)
	secrets, err := t.Secrets.Get(ctx, s.ID)
	if err != nil {
		return model.Commit{}, err
	}
	fresh, err := t.Projects.Fresh(s.Name)
	if err != nil {
		return model.Commit{}, err
	}
	log.Step("코드 가져오기", "fetching the code")
	c, err := t.Code.Download(ctx, model.CodeSource{Repo: repo, Branch: branch, Token: secrets.GitToken, Commit: commit}, fresh, log)
	if err != nil {
		return c, err
	}
	fmt.Fprintf(log, "%s %s\n", model.Deployment{Commit: c.Hash}.ShortCommit(), c.Message)
	if !secrets.EnvFile.IsZero() {
		n := secrets.EnvFile.Lines()
		fmt.Fprintf(log, ".env 파일 %d줄을 작업 폴더에 써요 (권한 600) / writing the .env file (%d lines)\n", n, n)
	}
	if _, err := t.Projects.Promote(s.Name, fresh, secrets.EnvFile.Bytes(), c); err != nil {
		return c, err
	}
	return c, nil
}

// ComposeBuilder는 작업 폴더에 코드를 받고 compose build --pull로 이미지를 만든다.
type ComposeBuilder struct{ t Tools }

func (b ComposeBuilder) Build(ctx context.Context, req model.BuildRequest, log contract.DeployLogWriter) (model.Version, error) {
	c, err := checkout(ctx, b.t, req.Service, "", log)
	v := model.Version{Commit: c}
	if err != nil {
		return v, err
	}
	log.Step("빌드", "building")
	return v, b.t.Compose.Build(ctx, project(b.t, req.Service), log)
}

// ComposeSwapper는 compose up -d로 띄우고, 주소가 가리키는 서비스들이 응답하기를 기다린다.
// 되돌리기면(작업 폴더의 커밋이 다르면) 그 커밋을 다시 받아 빌드한다. 지난 배포가 쓰던 이미지는 ID로 지운다.
type ComposeSwapper struct{ t Tools }

func (w ComposeSwapper) Swap(ctx context.Context, req model.SwapRequest, v model.Version, log contract.DeployLogWriter) (model.LiveState, error) {
	s := req.Service
	p := project(w.t, s)
	if v.Commit.Hash != "" && w.t.Projects.Commit(s.Name) != v.Commit.Hash {
		if _, err := checkout(ctx, w.t, s, v.Commit.Hash, log); err != nil {
			return model.LiveState{}, err
		}
		log.Step("빌드", "building")
		if err := w.t.Compose.Build(ctx, p, log); err != nil {
			return model.LiveState{}, err
		}
	}
	before := w.images(ctx, p.Name)

	log.Step("띄우기", "starting")
	if err := w.t.Compose.Up(ctx, p, log); err != nil {
		return model.LiveState{}, err
	}

	live := model.LiveState{Alias: p.Name}
	targets := targetsOf(s)
	if len(targets) > 0 {
		log.Step("주소 대상 응답 기다리기", "waiting for the addressed services")
	} else {
		fmt.Fprintln(log, "주소가 가리키는 서비스가 없어요 — 서비스 화면에서 주소의 대상을 고르세요 / no address points at a service yet")
	}
	for i, t := range targets {
		name := p.Container(t.Service)
		// compose의 컨테이너는 자기 네트워크에만 있다 — 주소 대상만 앱 네트워크에 붙인다 (나머지는 밖에서 닿지 않는다).
		// compose가 컨테이너를 다시 만들면 떨어지므로 up 뒤마다 붙인다 (이미 붙어 있으면 그대로).
		if w.t.Joiner != nil {
			if err := w.t.Joiner.Join(ctx, w.t.Network, name); err != nil {
				return model.LiveState{}, fmt.Errorf("%s를 %s 네트워크에 붙이지 못했어요 — compose 파일에 그 서비스가 있는지 보세요 / could not attach %s to %s: %w",
					t.Service, w.t.Network, t.Service, w.t.Network, err)
			}
		}
		if err := waitAnswer(ctx, w.t, name, t.Port, w.t.ComposeTimeout); err != nil {
			if logs, lerr := w.t.Watcher.Logs(context.Background(), name, 30); lerr == nil && strings.TrimSpace(logs) != "" {
				fmt.Fprintf(log, "— %s 출력 마지막 30줄 / last 30 lines —\n%s\n", t.Service, logs)
			}
			return model.LiveState{}, fmt.Errorf("%s:%d — %w", t.Service, t.Port, err)
		}
		fmt.Fprintf(log, "%s:%d 응답함 / answering\n", t.Service, t.Port)
		if i == 0 { // 대표 — 로그 탭·CPU·메모리·다시 시작 감지가 본다
			live.Instance, live.Port = name, t.Port
		}
	}

	after := w.images(ctx, p.Name)
	for _, id := range before {
		if !slices.Contains(after, id) {
			if err := w.t.Images.Remove(ctx, id); err == nil {
				fmt.Fprintf(log, "지난 이미지 %s 지움 / removed\n", short(id))
			}
		}
	}
	return live, nil
}

// Retire는 할 일이 없다 — compose가 이미 옛 컨테이너를 바꿨다.
func (ComposeSwapper) Retire(context.Context, model.Service, model.LiveState, contract.DeployLogWriter) {
}

// images는 그 프로젝트의 컨테이너가 쓰는 이미지 ID들이다.
func (w ComposeSwapper) images(ctx context.Context, name string) []string {
	all, err := w.t.Watcher.All(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, st := range all {
		if st.Project == name && st.ImageID != "" && !slices.Contains(out, st.ImageID) {
			out = append(out, st.ImageID)
		}
	}
	return out
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// targetsOf는 주소들이 가리키는 서비스·포트다 (같은 것은 한 번).
func targetsOf(s model.Service) []model.DomainTarget {
	var out []model.DomainTarget
	for _, d := range s.Domains {
		if !d.Target.IsZero() && !slices.Contains(out, d.Target) {
			out = append(out, d.Target)
		}
	}
	return out
}

// ComposeDestination은 주소마다 그 대상 서비스의 컨테이너:포트다. 대상이 없으면 보낼 곳이 없다.
type ComposeDestination struct{}

func (ComposeDestination) Find(model.Service) model.Destination {
	return model.Destination{Container: true}
}

// ForDomain은 바깥 연결처럼 "이름:포트"로 낸다 (Container 거짓) — 배포가 대상을 앱 네트워크에 붙이지만,
// 배포 없이 대상을 다른 서비스로 바꾸면 그 컨테이너는 아직 프로젝트 네트워크에만 있다. 그때는 웹서버가 그 네트워크에 붙는다(webServerReach).
func (ComposeDestination) ForDomain(s model.Service, d model.Domain) model.Destination {
	if d.Target.IsZero() {
		return model.Destination{}
	}
	return model.Destination{Address: model.ComposeProject{Name: s.Project()}.Container(d.Target.Service) + ":" + strconv.Itoa(int(d.Target.Port))}
}

// ComposeStatus는 프로젝트의 컨테이너 전부로 상태를 낸다. 한 번 돌고 잘 끝난 것은 문제가 아니다.
type ComposeStatus struct{}

func (ComposeStatus) Read(s model.Service, containers model.ContainerStates) model.ServiceStatus {
	if containers == nil {
		return model.ServiceStatus{Key: "unknown", Tone: "muted"}
	}
	running, total := 0, 0
	bad := ""
	for _, c := range containers {
		if c.Project != s.Project() || c.Ended() {
			continue
		}
		total++
		switch {
		case c.State == "running":
			running++
		case c.State == "restarting" || c.State == "dead":
			bad = "restarting"
		case bad == "":
			bad = "crashed"
		}
	}
	switch {
	case total == 0 && s.Live.Alias == "":
		return model.ServiceStatus{Key: "notdeployed", Tone: "muted"}
	case total == 0:
		return model.ServiceStatus{Key: "missing", Tone: "bad"}
	case s.Live.Stopped && running == 0:
		return model.ServiceStatus{Key: "stopped", Tone: "muted"}
	case bad != "":
		return model.ServiceStatus{Key: bad, Tone: "bad", Detail: fmt.Sprintf("%d/%d", running, total)}
	}
	return model.ServiceStatus{Key: "running", Tone: "ok", Detail: fmt.Sprintf("%d/%d", running, total)}
}

// ComposeRuntime은 프로젝트를 통째로 멈추고·켜고·지운다.
type ComposeRuntime struct{ t Tools }

func (r ComposeRuntime) TurnOff(ctx context.Context, s model.Service) error {
	return r.t.Compose.Switch(ctx, project(r.t, s), false)
}

func (r ComposeRuntime) TurnOn(ctx context.Context, s model.Service) error {
	return r.t.Compose.Switch(ctx, project(r.t, s), true)
}

// Remove는 compose down(볼륨은 남긴다)과 작업 폴더를 지운다. 이미지는 남긴다 — 이름으로 다른 곳과 겹칠 수 있다.
func (r ComposeRuntime) Remove(ctx context.Context, s model.Service) error {
	if err := r.t.Compose.Down(ctx, project(r.t, s)); err != nil {
		return err
	}
	return r.t.Projects.Remove(s.Name)
}
