package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// 컨테이너에 붙이는 라벨 — 어느 서비스의 몇 번째 배포인지
const (
	labelService    = "naru.service"
	labelDeployment = "naru.deployment"

	labelComposeProject = "com.docker.compose.project" // docker compose가 붙인다
	labelComposeService = "com.docker.compose.service"
	labelComposeDepends = "com.docker.compose.depends_on"
	stopGrace           = 10 * time.Second
)

// Puller는 이미지를 받고 그 ID·열어둔 포트를 알려준다.
type Puller struct{ c *Client }

func NewPuller(c *Client) Puller { return Puller{c} }

func (p Puller) Pull(ctx context.Context, ref model.ImageRef, log io.Writer) (model.ImageDetails, error) {
	if err := p.c.pull(ctx, ref.String(), log); err != nil {
		return model.ImageDetails{}, err
	}
	img, err := p.c.image(ctx, ref.String())
	if err != nil {
		return model.ImageDetails{}, err
	}
	d := model.ImageDetails{ID: model.ImageID(img.ID)}
	for _, port := range img.Ports {
		d.Ports = append(d.Ports, model.Port(port))
	}
	return d, nil
}

// Images는 이미지가 있는지 보고, 지운다.
type Images struct{ c *Client }

func NewImages(c *Client) Images { return Images{c} }

func (i Images) Exists(ctx context.Context, id model.ImageID) bool {
	_, err := i.c.image(ctx, string(id))
	return err == nil
}

// Remove는 이미지를 지운다. 강제하지 않는다 — 쓰는 컨테이너가 있거나 태그가 여럿이면 model.ErrInUse.
func (i Images) Remove(ctx context.Context, ref string) error {
	err := i.c.removeImage(ctx, ref)
	switch {
	case errors.Is(err, ErrConflict):
		return fmt.Errorf("%w: %v", model.ErrInUse, err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", model.ErrNotFound, err)
	}
	return err
}

// Containers는 컨테이너를 띄우고·없애고·멈추고·켜고, 상태를 본다. network는 앱 컨테이너가 붙는 네트워크다.
type Containers struct {
	c *Client
}

func NewContainers(c *Client) Containers { return Containers{c} }

// Start는 만들고 띄운다. 네트워크가 없으면 만든다. 띄우지 못하면 만든 것을 지운다.
func (k Containers) Start(ctx context.Context, s model.ContainerSpec) error {
	spec := Spec{
		Name: s.Name, Image: string(s.Image), Env: s.Env, Binds: s.Binds, Network: s.Network, Aliases: s.Aliases,
		Labels: map[string]string{
			labelService:    strconv.FormatInt(int64(s.Service), 10),
			labelDeployment: strconv.FormatInt(int64(s.Deploy), 10),
		},
	}
	// 네트워크가 없으면 먼저 만든다. Docker는 없는 네트워크로도 create를 받아 두고 start에서야 실패한다 (dockerd 27에서 확인).
	if s.Network != "" {
		if err := k.c.ensureNetwork(ctx, s.Network); err != nil {
			return err
		}
	}
	// 더 붙일 네트워크는 만들지 않는다 — 남의 것이다. 없으면 여기서 말한다:
	// Docker는 아직 띄우지 않은 컨테이너를 없는 네트워크에 붙여도 받아 두고 start에서야 실패한다 (dockerd 27에서 확인).
	for _, n := range s.ExtraNetworks {
		if err := k.c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(n), nil, nil); errors.Is(err, ErrNotFound) {
			return fmt.Errorf("네트워크 %q가 이 서버에 없어요 — 서비스 설정의 \"다른 네트워크\"를 확인하세요 / network %q does not exist on this server", n, n)
		} else if err != nil {
			return err
		}
	}
	if _, err := k.c.create(ctx, spec); err != nil {
		return err
	}
	// 다른 네트워크에는 띄우기 전에 붙인다 — 앱이 시작하면서 바로 그 네트워크의 DB를 찾을 수 있게.
	for _, n := range s.ExtraNetworks {
		if n == s.Network {
			continue
		}
		if err := k.c.connect(ctx, n, s.Name, s.Aliases); err != nil {
			k.c.remove(context.Background(), s.Name)
			return fmt.Errorf("네트워크 %q에 붙이지 못했어요 — 서비스 설정의 \"다른 네트워크\"를 확인하세요 / cannot join network %q: %w", n, n, err)
		}
	}
	if err := k.c.start(ctx, s.Name); err != nil {
		k.c.remove(context.Background(), s.Name)
		return err
	}
	return nil
}

// Remove는 없앤다. 이미 없으면 괜찮다.
func (k Containers) Remove(ctx context.Context, name string) error {
	if err := k.c.remove(ctx, name); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

func (k Containers) TurnOff(ctx context.Context, name string) error {
	return k.c.stop(ctx, name, stopGrace)
}
func (k Containers) TurnOn(ctx context.Context, name string) error { return k.c.start(ctx, name) }

// Restart는 다시 시작한다 (ContainerSwitch). 멈춘 것이면 띄운다.
func (k Containers) Restart(ctx context.Context, name string) error {
	return k.c.restart(ctx, name, stopGrace)
}

// RemoveStopped는 멈춘 컨테이너를 지운다 (ContainerSweeper). 강제하지 않는다 — 돌고 있으면 model.ErrRunning.
func (k Containers) RemoveStopped(ctx context.Context, name string) error {
	err := k.c.removeStopped(ctx, name)
	switch {
	case errors.Is(err, ErrConflict):
		return fmt.Errorf("%w: %v", model.ErrRunning, err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", model.ErrNotFound, err)
	}
	return err
}

// DiskUsage는 이미지·볼륨과 크기다 (DiskUsageReader) — docker system df.
func (k Containers) DiskUsage(ctx context.Context) (model.DockerDisk, error) {
	return k.c.diskUsage(ctx)
}

// RemoveVolume은 볼륨을 지운다 (VolumeRemover). 강제하지 않는다 — 쓰는 컨테이너가 있으면 model.ErrInUse.
func (k Containers) RemoveVolume(ctx context.Context, name string) error {
	err := k.c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil)
	switch {
	case errors.Is(err, ErrConflict):
		return fmt.Errorf("%w: %v", model.ErrInUse, err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", model.ErrNotFound, err)
	}
	return err
}

// All은 모든 컨테이너 상태를 한 번에 읽는다.
func (k Containers) All(ctx context.Context) (model.ContainerStates, error) {
	raw, err := k.c.containers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(model.ContainerStates, len(raw))
	for name, c := range raw {
		var ports []model.Port
		for _, p := range c.Ports {
			ports = append(ports, model.Port(p))
		}
		var published []model.Port
		for _, p := range c.Published {
			published = append(published, model.Port(p))
		}
		st := model.ContainerState{Name: name, Running: c.State == Running, State: string(c.State), Status: c.Status, Ports: ports, Published: published,
			Image: c.Image, Project: c.Labels[labelComposeProject], ComposeService: c.Labels[labelComposeService], DependsOn: c.Labels[labelComposeDepends], ImageID: c.ImageID,
			Networks: networksNamed(c.Networks)}
		if id, err := strconv.ParseInt(c.Labels[labelService], 10, 64); err == nil {
			st.Service = model.ServiceID(id)
		}
		out[name] = st
	}
	return out, nil
}

func (k Containers) One(ctx context.Context, name string) (model.ContainerState, error) {
	in, err := k.c.inspect(ctx, name)
	if err != nil {
		return model.ContainerState{}, err
	}
	return model.ContainerState{Name: name, Running: in.Running, State: in.Status, ExitCode: in.ExitCode, Restarts: in.Restarts, OOMKilled: in.OOMKilled}, nil
}

func (k Containers) Logs(ctx context.Context, name string, lines int) (string, error) {
	return k.c.logs(ctx, name, lines)
}

// Recent는 컨테이너가 찍은 최근 n줄이다 (ContainerLogReader).
func (k Containers) Recent(ctx context.Context, name string, n int) ([]model.LogLine, error) {
	return k.c.logLines(ctx, name, n)
}

// NetworksOf는 컨테이너가 붙은 네트워크다 (NetworkJoiner). 없는 컨테이너면 model.ErrNotFound.
func (k Containers) NetworksOf(ctx context.Context, name string) ([]model.DockerNetwork, error) {
	names, err := k.c.networksOf(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return networksNamed(names), nil
}

// networksNamed는 네트워크 이름에 "사용자 정의인가"를 붙인다 — 기본 네트워크(bridge·host·none)에서는 이름 풀이가 안 된다.
func networksNamed(names []string) []model.DockerNetwork {
	out := make([]model.DockerNetwork, 0, len(names))
	for _, n := range names {
		out = append(out, model.DockerNetwork{Name: n, UserDefined: n != "bridge" && n != "host" && n != "none"})
	}
	return out
}

// Inspect는 컨테이너 하나의 환경 변수·볼륨·출처다 (ContainerInspector). 없는 컨테이너면 model.ErrNotFound.
func (k Containers) Inspect(ctx context.Context, name string) (model.ContainerDetails, error) {
	d, err := k.c.details(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return model.ContainerDetails{}, model.ErrNotFound
	}
	if err != nil {
		return model.ContainerDetails{}, err
	}
	return model.ContainerDetails{Env: d.Env, ImageEnv: d.ImageEnv, Binds: d.Binds, NamedVolumes: d.NamedVolumes, Source: d.Source,
		Health: d.Health, HealthLog: d.HealthLog, Started: d.Started, Restarts: d.Restarts}, nil
}

// Join은 컨테이너를 네트워크에 붙인다. 이미 붙어 있으면 그대로 괜찮다.
func (k Containers) Join(ctx context.Context, network, name string) error {
	return k.c.connect(ctx, network, name, nil)
}

// Leave는 컨테이너를 네트워크에서 뗀다. 이미 떨어져 있으면 그대로 괜찮다.
func (k Containers) Leave(ctx context.Context, network, name string) error {
	err := k.c.call(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", map[string]string{"Container": name}, nil)
	if err != nil && strings.Contains(err.Error(), "is not connected") {
		return nil
	}
	return err
}

// Usage는 컨테이너 하나의 CPU·메모리다 (UsageReader).
func (k Containers) Usage(ctx context.Context, name string) (model.ResourceUsage, error) {
	return k.c.usage(ctx, name)
}

func (k Containers) BelongingTo(ctx context.Context, id model.ServiceID) ([]string, error) {
	return k.c.byLabel(ctx, labelService, strconv.FormatInt(int64(id), 10))
}
