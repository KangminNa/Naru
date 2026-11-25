package files

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/KangminNa/naru/internal/model"
)

const commitMark = ".naru-commit"

// ProjectFolderSet은 Compose 작업 폴더들이다 — <root>/<서비스>. 배포 사이에도 남는다 (ProjectFolders).
// root는 서버와 Naru 컨테이너에서 같은 경로여야 한다 (compose의 상대 경로를 Docker가 서버에서 찾는다).
type ProjectFolderSet struct{ root string }

func NewProjectFolders(root string) ProjectFolderSet { return ProjectFolderSet{root} }

func (p ProjectFolderSet) dir(s model.ServiceName) string { return filepath.Join(p.root, s.String()) }

// Dir은 그 서비스의 작업 폴더 경로다.
func (p ProjectFolderSet) Dir(s model.ServiceName) string { return p.dir(s) }

// Fresh는 코드를 받을 빈 폴더다 — 작업 폴더 옆이라 바꿔 끼울 때 같은 디스크 안에서 이름만 바꾼다.
func (p ProjectFolderSet) Fresh(s model.ServiceName) (string, error) {
	if p.root == "" {
		return "", errors.New("compose: no folder (NARU_HOST_DATA)")
	}
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		return "", err
	}
	// 지난번에 받다가 멈춘 것이 남았으면 지운다
	if left, _ := filepath.Glob(filepath.Join(p.root, "."+s.String()+"-*")); len(left) > 0 {
		for _, l := range left {
			os.RemoveAll(l)
		}
	}
	return os.MkdirTemp(p.root, "."+s.String()+"-")
}

// Promote는 받은 코드에 .env(600)와 커밋 표시를 쓰고 작업 폴더로 바꿔 끼운다. 옛 폴더는 지운다
// (돌고 있는 컨테이너가 붙여 둔 파일은 다시 만들어질 때까지 옛 내용을 그대로 본다).
func (p ProjectFolderSet) Promote(s model.ServiceName, fresh string, env []byte, c model.Commit) (string, error) {
	if !strings.HasPrefix(filepath.Clean(fresh), filepath.Clean(p.root)+string(filepath.Separator)) {
		return "", errors.New("compose: folder outside the projects")
	}
	os.RemoveAll(filepath.Join(fresh, ".git")) // 코드만 남긴다 — 토큰이 남을 곳도 없다
	if len(env) > 0 {
		if err := os.WriteFile(filepath.Join(fresh, ".env"), env, 0o600); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(fresh, commitMark), []byte(c.Hash+"\n"), 0o600); err != nil {
		return "", err
	}
	dir := p.dir(s)
	old := dir + ".old"
	os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(fresh, dir); err != nil {
		os.Rename(old, dir)
		return "", err
	}
	os.RemoveAll(old)
	return dir, nil
}

// Commit은 지금 작업 폴더의 커밋이다.
func (p ProjectFolderSet) Commit(s model.ServiceName) string {
	b, err := os.ReadFile(filepath.Join(p.dir(s), commitMark))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (p ProjectFolderSet) Remove(s model.ServiceName) error {
	if p.root == "" || s.String() == "" {
		return nil
	}
	return os.RemoveAll(p.dir(s))
}
