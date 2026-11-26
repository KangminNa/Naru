package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/KangminNa/naru/internal/model"
)

const (
	commitMark = ".naru-commit"
	fileList   = ".naru-files" // 지난 배포에서 저장소로부터 둔 파일들 (작업 폴더 기준, 한 줄에 하나)
)

// ProjectFolderSet은 Compose 작업 폴더들이다 — <root>/<서비스>. 배포 사이에도 같은 폴더다 (ProjectFolders).
// root는 서버와 Naru 컨테이너에서 같은 경로여야 한다 (compose의 상대 경로를 Docker가 서버에서 찾는다).
type ProjectFolderSet struct{ root string }

func NewProjectFolders(root string) ProjectFolderSet { return ProjectFolderSet{root} }

func (p ProjectFolderSet) dir(s model.ServiceName) string { return filepath.Join(p.root, s.String()) }

// Dir은 그 서비스의 작업 폴더 경로다.
func (p ProjectFolderSet) Dir(s model.ServiceName) string { return p.dir(s) }

// Fresh는 코드를 받을 빈 폴더다 (작업 폴더 옆). Promote가 작업 폴더에 맞춘 뒤 지운다.
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

// Promote는 받은 코드를 작업 폴더에 그 자리에서 맞춘다 — 폴더를 바꿔 끼우지 않는다.
//   - 저장소의 파일은 덮어쓴다 (같은 파일에 다시 써서, 그 파일을 붙여 둔 컨테이너가 새 내용을 본다)
//   - 지난번에 저장소에서 왔는데 이번에 없는 파일은 지운다 (.naru-files 목록으로 안다)
//   - 그 밖의 것 — 컨테이너가 상대 경로 볼륨(./docker-data:/var/mail)으로 쓴 데이터 — 은 건드리지 않는다
//
// .env(600)와 커밋 표시도 쓴다. .env 칸이 비면 Naru가 썼던 .env는 지워진다 (저장소에 .env가 있으면 그것).
// 중간에 멈추면 일부만 바뀐 채로 남는다 — 다음 배포가 다시 맞춘다.
func (p ProjectFolderSet) Promote(s model.ServiceName, fresh string, env []byte, c model.Commit) (string, error) {
	if !strings.HasPrefix(filepath.Clean(fresh), filepath.Clean(p.root)+string(filepath.Separator)) {
		return "", errors.New("compose: folder outside the projects")
	}
	defer os.RemoveAll(fresh)
	os.RemoveAll(filepath.Join(fresh, ".git")) // 코드만 둔다 — 토큰이 남을 곳도 없다
	if len(env) > 0 {
		if err := os.WriteFile(filepath.Join(fresh, ".env"), env, 0o600); err != nil {
			return "", err
		}
	}
	dir := p.dir(s)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	before := readList(dir)
	var placed []string
	err := filepath.WalkDir(fresh, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(fresh, path)
		if rel == "." {
			return nil
		}
		target := filepath.Join(dir, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return placeDir(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := clear(target); err != nil {
				return err
			}
			placed = append(placed, rel)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			placed = append(placed, rel)
			return placeFile(path, target, info.Mode().Perm())
		}
		return nil // 장치 파일 같은 것은 두지 않는다
	})
	if err != nil {
		return "", fmt.Errorf("compose: placing the code: %w", err)
	}
	now := map[string]bool{}
	for _, rel := range placed {
		now[rel] = true
	}
	for _, rel := range before {
		if !now[rel] {
			removeInside(dir, rel)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, commitMark), []byte(c.Hash+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := writeList(dir, placed); err != nil {
		return "", err
	}
	return dir, nil
}

// placeDir은 폴더를 만든다. 그 자리에 파일·링크가 있으면 저장소가 이긴다.
func placeDir(target string, perm fs.FileMode) error {
	if info, err := os.Lstat(target); err == nil {
		if info.IsDir() {
			return nil
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	return os.Mkdir(target, perm|0o700)
}

// placeFile은 파일을 그 자리에 다시 쓴다 — 이미 있는 파일이면 같은 파일(inode)에 써서, 붙여 둔 컨테이너가 바뀐 내용을 본다.
func placeFile(from, target string, perm fs.FileMode) error {
	body, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		if err := clear(target); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(target, perm)
}

// clear는 그 자리의 파일·링크·빈 폴더를 치운다. 내용이 든 폴더는 지우지 않는다 (데이터일 수 있다).
func clear(target string) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: the repository has a file where a folder with content is: %w", filepath.Base(target), err)
	}
	return nil
}

// removeInside는 작업 폴더 안의 rel을 지우고, 비게 된 위 폴더도 지운다.
// 바깥을 가리키는 경로나 링크를 거쳐 가는 경로는 지우지 않는다 (목록이 망가져도 작업 폴더 밖은 안전하다).
func removeInside(dir, rel string) {
	if !filepath.IsLocal(rel) {
		return
	}
	for parent := filepath.Dir(rel); parent != "."; parent = filepath.Dir(parent) {
		if info, err := os.Lstat(filepath.Join(dir, parent)); err != nil || !info.IsDir() {
			return
		}
	}
	if err := os.Remove(filepath.Join(dir, rel)); err != nil {
		return
	}
	for parent := filepath.Dir(rel); parent != "."; parent = filepath.Dir(parent) {
		if os.Remove(filepath.Join(dir, parent)) != nil {
			return
		}
	}
}

// readList는 지난번에 저장소에서 둔 파일들이다 (없으면 비어 있다 — 예전 Naru가 만든 폴더는 아무것도 지우지 않는다).
func readList(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, fileList))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func writeList(dir string, rels []string) error {
	tmp := filepath.Join(dir, fileList+".tmp")
	if err := os.WriteFile(tmp, []byte(strings.Join(rels, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, fileList))
}

// Commit은 지금 작업 폴더의 커밋이다.
func (p ProjectFolderSet) Commit(s model.ServiceName) string {
	b, err := os.ReadFile(filepath.Join(p.dir(s), commitMark))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Remove는 저장소에서 온 파일과 Naru의 표시만 지운다. 컨테이너가 쓴 데이터가 남았으면 폴더도 남는다 — 사람이 지운다.
func (p ProjectFolderSet) Remove(s model.ServiceName) error {
	if p.root == "" || s.String() == "" {
		return nil
	}
	dir := p.dir(s)
	list := readList(dir)
	sort.Sort(sort.Reverse(sort.StringSlice(list))) // 안쪽부터
	for _, rel := range list {
		removeInside(dir, rel)
	}
	os.Remove(filepath.Join(dir, commitMark))
	os.Remove(filepath.Join(dir, fileList))
	os.Remove(dir) // 비었을 때만
	return nil
}
