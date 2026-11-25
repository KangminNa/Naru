package kinds

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// ── 새 버전 만들기 ─────────────────────────

// RepoBuilder는 저장소를 내려받아 이미지를 빌드한다.
type RepoBuilder struct{ t Tools }

func (b RepoBuilder) Build(ctx context.Context, req model.BuildRequest, log contract.DeployLogWriter) (model.Version, error) {
	s := req.Service
	dir, giveBack, commit, err := fetchCode(ctx, b.t, req, log)
	if err != nil {
		return model.Version{}, err
	}
	defer giveBack()
	v := model.Version{Commit: commit}

	buildPath, _ := model.ParseFolderPath(s.BuildPath)
	folder, err := b.t.Work.Inside(dir, buildPath)
	if err != nil {
		return v, fmt.Errorf("빌드 경로 %q를 찾을 수 없어요 / build path not found: %w", s.BuildPath, err)
	}
	ports, found := b.t.Dockerfile.Ports(folder, s.BuildTarget)
	if !found {
		return v, explain(model.ErrNoDockerfile, "Dockerfile이 없어요. HTML 파일만 있는 저장소라면 '정적 사이트'로 올리세요 / no Dockerfile — for plain HTML use a static site")
	}
	stages := b.t.Dockerfile.Stages(folder)
	switch {
	case s.BuildTarget != "" && !slices.Contains(stages, s.BuildTarget):
		return v, fmt.Errorf("빌드 타깃 %q가 Dockerfile에 없어요 — 있는 단계: %s / build target %q is not in the Dockerfile — stages: %s",
			s.BuildTarget, stageList(stages), s.BuildTarget, stageList(stages))
	case s.BuildTarget != "":
		fmt.Fprintf(log, "빌드 타깃: %s / build target: %s\n", s.BuildTarget, s.BuildTarget)
	case len(stages) > 1:
		// 단계가 여럿인데 고르지 않았다 — Docker처럼 맨 마지막을 빌드하되, 다른 것이 있다고 말해 둔다
		fmt.Fprintf(log, "Dockerfile에 단계가 %d개예요 — 맨 마지막(%s)을 빌드해요. 다른 앱이면 서비스 설정의 \"빌드 타깃\"에 적으세요: %s / "+
			"%d stages — building the last one (%s); set a build target for another: %s\n",
			len(stages), stages[len(stages)-1], stageList(stages), len(stages), stages[len(stages)-1], stageList(stages))
	}
	v.Port = only(ports)

	secrets, err := b.t.Secrets.Get(ctx, s.ID)
	if err != nil {
		return v, err
	}
	log.Step("이미지 만들기", "building the image")
	var extra map[string][]byte
	if !secrets.EnvFile.IsZero() {
		extra = map[string][]byte{".env": secrets.EnvFile.Bytes()}
		n := secrets.EnvFile.Lines()
		fmt.Fprintf(log, ".env 파일 %d줄을 빌드 폴더에 넣어요 (저장소의 .env는 이것으로 바뀌어요) / adding the .env file (%d lines) to the build folder, replacing any in the repo\n", n, n)
	}
	packed := b.t.Packer.Pack(folder, extra)
	defer packed.Close()
	v.Image, err = b.t.Builder.Build(ctx, packed, model.ImageTag(s.Name, req.Deployment), s.BuildTarget, log)
	return v, err
}

// ImageFetcher는 이미지를 받는다.
type ImageFetcher struct{ t Tools }

func (f ImageFetcher) Build(ctx context.Context, req model.BuildRequest, log contract.DeployLogWriter) (model.Version, error) {
	ref, err := model.ParseImageRef(req.Service.Source)
	if err != nil {
		return model.Version{}, err
	}
	log.Step("이미지 받기", "pulling the image")
	got, err := f.t.Puller.Pull(ctx, ref, log)
	if err != nil {
		return model.Version{}, err
	}
	return model.Version{Image: got.ID, Port: only(got.Ports)}, nil
}

// fetchCode는 작업 폴더를 빌려 코드를 내려받는다. 다 쓰면 giveBack을 불러 돌려준다.
func fetchCode(ctx context.Context, t Tools, req model.BuildRequest, log contract.DeployLogWriter) (dir string, giveBack func(), c model.Commit, err error) {
	s := req.Service
	repo, err := model.ParseRepoURL(s.Source)
	if err != nil {
		return "", nil, c, err
	}
	branch, err := model.ParseBranch(s.Branch)
	if err != nil {
		return "", nil, c, err
	}
	if branch.IsZero() {
		branch = model.DefaultBranch()
	}
	secrets, err := t.Secrets.Get(ctx, s.ID)
	if err != nil {
		return "", nil, c, err
	}
	dir, giveBack, err = t.Work.Borrow(fmt.Sprintf("%s-%d", s.Name, req.Deployment))
	if err != nil {
		return "", nil, c, err
	}
	log.Step("코드 가져오기", "fetching the code")
	c, err = t.Code.Download(ctx, model.CodeSource{Repo: repo, Branch: branch, Token: secrets.GitToken}, dir, log)
	if err != nil {
		giveBack()
		return "", nil, c, err
	}
	fmt.Fprintf(log, "%s %s\n", model.Deployment{Commit: c.Hash}.ShortCommit(), c.Message)
	return dir, giveBack, c, nil
}

func stageList(stages []string) string {
	if len(stages) == 0 {
		return "(이름 있는 단계 없음 / none named)"
	}
	return strings.Join(stages, ", ")
}

// only는 포트가 하나일 때만 그것이다. 여럿이면 어느 것인지 모른다.
func only(ports []model.Port) model.Port {
	if len(ports) == 1 {
		return ports[0]
	}
	return 0
}

// ── 바꿔 끼우기 ────────────────────────────

// ContainerSwapper는 새 컨테이너를 같은 네트워크 별칭으로 띄우고, 응답하면 옛 것을 내린다.
// 별칭이 같으니 웹서버는 설정을 바꾸지 않아도 새 것으로 보낸다 — 그래서 요청이 끊기지 않는다.
// 새 것이 응답하지 않으면 새 것만 지우고 지금 도는 것은 그대로 둔다.
type ContainerSwapper struct{ t Tools }

func (w ContainerSwapper) Swap(ctx context.Context, req model.SwapRequest, v model.Version, log contract.DeployLogWriter) (model.LiveState, error) {
	s := req.Service
	if v.Image == "" {
		return model.LiveState{}, model.ErrCannotRollBack
	}
	if !w.t.Images.Exists(ctx, v.Image) {
		return model.LiveState{}, explain(errGone, "그때의 이미지가 지워졌어요 — 다시 배포하세요 / that image is gone, deploy again")
	}
	port := s.Port
	if port == 0 {
		port = v.Port
		if port == 0 {
			return model.LiveState{}, explain(model.ErrUnknownPort, "앱이 어느 포트를 듣는지 몰라요. Dockerfile에 EXPOSE를 쓰거나 서비스 설정에서 앱 포트를 적어 주세요 / unknown app port — add EXPOSE or set the port")
		}
		fmt.Fprintf(log, "앱 포트 %d (이미지에서 찾음) / app port %d (from the image)\n", port, port)
	}
	secrets, err := w.t.Secrets.Get(ctx, s.ID)
	if err != nil {
		return model.LiveState{}, err
	}

	alias := s.Live.Alias
	if alias == "" {
		alias = "naru-" + s.Name.String()
	}
	name := fmt.Sprintf("%s-%d", alias, req.Deployment)
	log.Step("새 컨테이너 시작", "starting the new container")
	err = w.t.Starter.Start(ctx, model.ContainerSpec{
		Name: name, Image: v.Image, Env: secrets.Env.List(), Binds: secrets.Volumes.List(),
		Network: w.t.Network, Aliases: []string{alias}, Service: s.ID, Deploy: req.Deployment,
		ExtraNetworks: strings.Fields(s.Networks),
	})
	if err != nil {
		return model.LiveState{}, err
	}

	log.Step(fmt.Sprintf("포트 %d 응답 기다리기", port), fmt.Sprintf("waiting for port %d", port))
	if err := w.waitReady(ctx, name, port); err != nil {
		if logs, lerr := w.t.Watcher.Logs(context.Background(), name, 30); lerr == nil && strings.TrimSpace(logs) != "" {
			fmt.Fprintf(log, "— 컨테이너 출력 마지막 30줄 / last 30 lines —\n%s\n", logs)
		}
		w.t.Remover.Remove(context.Background(), name)
		return model.LiveState{}, fmt.Errorf("%w — 지금 돌던 것은 그대로 둡니다 / the running version is untouched", err)
	}
	fmt.Fprintln(log, "응답함 / answering")
	return model.LiveState{Alias: alias, Instance: name, Port: port}, nil
}

// Retire는 웹서버가 새 것(keep)을 가리킨 뒤에 부른다 — 이 서비스로 돌던 나머지 컨테이너를 내린다.
func (w ContainerSwapper) Retire(ctx context.Context, s model.Service, keep model.LiveState, log contract.DeployLogWriter) {
	log.Step("옛 컨테이너 내리기", "retiring the old container")
	for _, old := range w.older(ctx, s, keep.Instance) {
		w.t.Switch.TurnOff(ctx, old)
		w.archive(ctx, s, old, log)
		if err := w.t.Remover.Remove(ctx, old); err != nil {
			fmt.Fprintf(log, "%s: %v\n", old, err)
		} else {
			fmt.Fprintf(log, "%s 내림 / removed\n", old)
		}
	}
}

// archiveLines는 지울 컨테이너에서 남길 앱 출력의 최대 줄 수다.
const archiveLines = 5000

// archive는 지울 컨테이너의 앱 출력을 남긴다 — 지우면 Docker가 함께 지운다. 이름에 배포 번호가 없으면(v1에서 온 것) 남기지 않는다.
// 남기지 못해도 배포는 그대로 간다.
func (w ContainerSwapper) archive(ctx context.Context, s model.Service, name string, log contract.DeployLogWriter) {
	d := model.ContainerDeployment(name)
	if w.t.AppLogs == nil || w.t.Archive == nil || d == 0 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	lines, err := w.t.AppLogs.Recent(lctx, name, archiveLines)
	cancel()
	if err == nil && len(lines) > 0 {
		err = w.t.Archive.Keep(s.Name, d, lines)
	}
	if err != nil {
		fmt.Fprintf(log, "%s의 앱 출력을 보관하지 못했어요 / could not keep the output of %s: %v\n", name, name, err)
	} else if len(lines) > 0 {
		fmt.Fprintf(log, "%s의 앱 출력 %d줄을 보관했어요 — 로그 탭에서 볼 수 있어요 / kept %d lines of output from %s\n", name, len(lines), len(lines), name)
	}
}

// waitReady는 새 컨테이너가 앱 포트로 응답하기를 기다린다. 먼저 죽으면 바로 실패한다.
// Naru도 같은 네트워크에 있어 컨테이너 이름으로 닿는다.
func (w ContainerSwapper) waitReady(ctx context.Context, name string, port model.Port) error {
	ctx, cancel := context.WithTimeout(ctx, w.t.ReadyTimeout)
	defer cancel()
	wait := int(w.t.ReadyTimeout.Seconds())
	for {
		if st, err := w.t.Watcher.One(ctx, name); err == nil {
			switch {
			case st.Restarts > 0 || st.State == "restarting":
				// 앱이 죽고 Docker가 다시 띄우고 있다 — 기다려도 소용없다 (재시작 정책 때문에 "멈춤"으로 보이지 않는다)
				return fmt.Errorf("앱이 시작하자마자 죽어요 — Docker가 %d번 다시 띄웠어요. 위 출력의 오류를 보세요 / the app dies right after it starts (restarted %d times) — see the output above",
					max(st.Restarts, 1), max(st.Restarts, 1))
			case !st.Running && (st.State == "exited" || st.State == "dead"):
				return fmt.Errorf("컨테이너가 바로 멈췄어요 (종료 코드 %d) / the container exited (code %d)", st.ExitCode, st.ExitCode)
			}
		}
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
		err := w.t.Ports.Answers(dctx, name, port)
		dcancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d초 동안 포트 %d에서 응답이 없어요. 앱이 다른 포트를 듣고 있지 않은지 확인하세요 / no answer on port %d after %ds", wait, port, port, wait)
		case <-time.After(w.t.PollEvery):
		}
	}
}

// older는 새 것(keep)을 빼고 이 서비스로 돌고 있는 컨테이너들이다 — 지금 것, 라벨이 붙은 남은 것.
func (w ContainerSwapper) older(ctx context.Context, s model.Service, keep string) []string {
	seen := map[string]bool{keep: true}
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(s.Live.CurrentContainer())
	if names, err := w.t.Watcher.BelongingTo(ctx, s.ID); err == nil {
		for _, n := range names {
			add(n)
		}
	}
	return out
}

// ── 보낼 곳 · 상태 ─────────────────────────

// ContainerAliasDestination은 서비스 별칭:앱 포트다 (Docker 방식 — 웹서버가 같은 네트워크에 있다).
// 새 컨테이너가 같은 별칭으로 붙으니 배포 때 웹서버 설정이 바뀌지 않는다. 아직 모르면 비어 있다.
type ContainerAliasDestination struct{}

func (ContainerAliasDestination) Find(s model.Service) model.Destination {
	d := model.Destination{Container: true}
	if s.Live.Alias != "" && s.Port != 0 {
		d.Address = fmt.Sprintf("%s:%d", s.Live.Alias, s.Port)
	}
	return d
}

// ContainerStatus는 컨테이너 상태를 화면 상태로 바꾼다. containers가 nil이면 Docker를 읽지 못한 것이다.
type ContainerStatus struct{}

func (ContainerStatus) Read(s model.Service, containers model.ContainerStates) model.ServiceStatus {
	if containers == nil {
		return model.ServiceStatus{Key: "unknown", Tone: "muted"}
	}
	c, ok := containers[s.Live.CurrentContainer()]
	if !ok {
		if s.Live.CurrentContainer() == "" {
			return model.ServiceStatus{Key: "notdeployed", Tone: "muted"}
		}
		return model.ServiceStatus{Key: "missing", Tone: "bad"}
	}
	st := model.ServiceStatus{Detail: c.Status}
	switch c.State {
	case "running":
		st.Key, st.Tone = "running", "ok"
	case "restarting", "dead":
		st.Key, st.Tone = "restarting", "bad"
	case "exited":
		st.Key, st.Tone = "crashed", "bad"
		if s.Live.Stopped {
			st.Key, st.Tone = "stopped", "muted"
		}
	case "paused":
		st.Key, st.Tone = "paused", "warn"
	default:
		st.Key, st.Tone = "created", "muted"
	}
	return st
}
