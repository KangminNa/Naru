package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// ComposeCLI는 Naru 이미지 안의 docker compose 실행 파일로 compose 프로젝트를 다룬다 (ComposeRunner).
// buildx처럼 Naru의 환경 변수를 물려주지 않는다. compose는 빌드에 buildx를 쓰므로 빌드마다 만드는 설정 폴더에 플러그인으로 걸어 둔다.
// 작업 폴더는 서버와 같은 경로라서, compose 파일의 상대 경로(./conf/x)를 Docker가 서버에서 그대로 찾는다.
type ComposeCLI struct {
	bin    string // docker compose 실행 파일
	buildx string // buildx 실행 파일 — compose가 플러그인으로 찾는다
	socket string
	temp   string
}

func NewComposeCLI(bin, buildx, socket, temp string) ComposeCLI {
	return ComposeCLI{bin: bin, buildx: buildx, socket: socket, temp: temp}
}

func (c ComposeCLI) Build(ctx context.Context, p model.ComposeProject, log io.Writer) error {
	return c.run(ctx, p, log, "build", "--pull")
}

func (c ComposeCLI) Up(ctx context.Context, p model.ComposeProject, log io.Writer) error {
	return c.run(ctx, p, log, "up", "-d", "--remove-orphans")
}

func (c ComposeCLI) Switch(ctx context.Context, p model.ComposeProject, on bool) error {
	verb := "stop"
	if on {
		verb = "start"
	}
	return c.run(ctx, p, io.Discard, verb)
}

func (c ComposeCLI) Down(ctx context.Context, p model.ComposeProject) error {
	return c.run(ctx, p, io.Discard, "down", "--remove-orphans") // -v 없음 — 데이터 볼륨은 남긴다
}

func (c ComposeCLI) run(ctx context.Context, p model.ComposeProject, log io.Writer, args ...string) error {
	if p.Name == "" || p.Dir == "" {
		return fmt.Errorf("compose: no project")
	}
	if err := os.MkdirAll(c.temp, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(c.temp, "compose-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if c.buildx != "" {
		os.MkdirAll(filepath.Join(dir, "cli-plugins"), 0o700)
		os.Symlink(c.buildx, filepath.Join(dir, "cli-plugins", "docker-buildx"))
	}
	all := []string{"--ansi", "never", "--progress", "plain", "-p", p.Name}
	if p.File != "" {
		all = append(all, "-f", p.File)
	}
	cmd := exec.CommandContext(ctx, c.bin, append(all, args...)...)
	cmd.Dir = p.Dir
	cmd.Env = []string{"DOCKER_HOST=unix://" + c.socket, "DOCKER_CONFIG=" + dir, "HOME=" + dir, "TMPDIR=" + dir, "PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound):
			return fmt.Errorf("docker compose를 찾을 수 없어요 — Naru 이미지를 다시 빌드하세요 / docker compose not found at %s: %w", c.bin, err)
		default:
			return fmt.Errorf("docker compose %s가 실패했어요 — 위 기록을 보세요 / docker compose %s failed, see above: %w", args[0], args[0], err)
		}
	}
	return nil
}
