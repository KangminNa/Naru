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
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// BuildxBuilder는 buildx(BuildKit)로 이미지를 만든다 — 로컬의 docker build와 같은 빌더.
// Docker API의 /build는 클래식 빌더라 RUN --mount 같은 BuildKit 문법을 못 쓰고, API로 BuildKit을 켜려면 세션(gRPC)이 필요하다.
// 그래서 Naru 이미지에 넣은 buildx 실행 파일 하나를 부른다 (docker CLI는 없다).
//
// 빌드 묶음은 stdin tar로 넘긴다 — buildx는 그 묶음에 .dockerignore를 다시 적용하지 않는다 (scripts/buildx-check.sh).
// buildx는 stdin의 첫 1024바이트로 tar인지 가린다 — 그래서 묶음은 늘 짧은 USTAR 항목(./)으로 시작한다 (files.TarPacker).
// buildx에는 Naru의 환경 변수를 물려주지 않는다. 설정·임시 폴더는 빌드마다 새로 만들고 지운다 — 레지스트리 로그인 정보는 쓰지 않는다.
type BuildxBuilder struct {
	bin    string // buildx 실행 파일
	socket string // Docker 소켓
	temp   string // 빌드마다 빈 설정 폴더를 만들 곳
}

func NewBuildxBuilder(bin, socket, temp string) BuildxBuilder {
	return BuildxBuilder{bin: bin, socket: socket, temp: temp}
}

func (b BuildxBuilder) Build(ctx context.Context, buildFolder io.Reader, tag string, log io.Writer) (model.ImageID, error) {
	if err := os.MkdirAll(b.temp, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(b.temp, "buildx-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	iid := filepath.Join(dir, "iid")

	// --pull: 기반 이미지(FROM nginx:alpine 같은 움직이는 태그)를 배포마다 레지스트리에서 확인한다.
	// 없으면 서버에 남은 옛 것으로 빌드해서, 기반 이미지의 보안 수정이 다시 배포해도 들어오지 않는다. 바뀌지 않았으면 받지 않는다.
	cmd := exec.CommandContext(ctx, b.bin, "build", "--progress=plain", "--provenance=false", "--sbom=false", "--pull",
		"--iidfile", iid, "--tag", tag, "-")
	// TMPDIR도 이 폴더로 — buildx는 stdin 묶음을 임시 파일로 받는다. 빌드가 끝나면 함께 지운다.
	cmd.Env = []string{"DOCKER_HOST=unix://" + b.socket, "DOCKER_CONFIG=" + dir, "HOME=" + dir, "TMPDIR=" + dir, "PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Dir = dir
	cmd.Stdin = buildFolder
	cmd.Stdout, cmd.Stderr = log, log // 같은 값이라 os/exec가 한 번에 한 곳에서만 쓴다
	cmd.WaitDelay = 5 * time.Second   // 끊겼는데 파이프가 남아 기다리지 않게

	if err := cmd.Run(); err != nil {
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound):
			return "", fmt.Errorf("빌드 도구(buildx)를 찾을 수 없어요 — Naru 이미지를 다시 빌드하세요 / buildx not found at %s: %w", b.bin, err)
		default:
			return "", fmt.Errorf("이미지를 만들지 못했어요 — 위 기록의 ERROR 줄을 보세요 / the build failed, see ERROR above: %w", err)
		}
	}
	raw, err := os.ReadFile(iid)
	if err != nil {
		return "", fmt.Errorf("buildx가 이미지 ID를 남기지 않았어요 / no image ID: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(id, "sha256:") {
		return "", fmt.Errorf("buildx가 남긴 이미지 ID가 이상해요 / odd image ID %q", id)
	}
	return model.ImageID(id), nil
}
