package files

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TarPacker는 빌드할 폴더를 tar로 묶는다. .git과 .dockerignore에 맞는 것은 빠진다.
// extra(.env 파일 칸)는 디스크에 쓰지 않고 묶음에만 넣는다. 폴더의 같은 이름은 빼고 이것을 넣는다.
// .dockerignore에 맞아도 넣는다 — Naru는 클래식 빌더를 써서 데몬은 Dockerfile·.dockerignore 말고는 빼지 않는다.
type TarPacker struct{}

func (TarPacker) Pack(folder string, extra map[string][]byte) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(folder, extra, pw)) }()
	return pr
}

// ignoreRules는 .dockerignore다. 마지막으로 맞은 규칙이 이긴다 (!는 다시 넣기).
type ignoreRules []struct {
	pattern string
	negate  bool
}

func readIgnore(dir string) ignoreRules {
	raw, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		return nil
	}
	var rules ignoreRules
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		neg := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		line = strings.Trim(path.Clean("/"+line), "/")
		rules = append(rules, struct {
			pattern string
			negate  bool
		}{line, neg})
	}
	return rules
}

// ignored는 rel(슬래시 구분)이 빠져야 하는가. 패턴이 경로나 그 상위 폴더에 맞으면 빠진다.
func (r ignoreRules) ignored(rel string) bool {
	out := false
	for _, rule := range r {
		if matchPathOrParent(rule.pattern, rel) {
			out = !rule.negate
		}
	}
	return out
}

func matchPathOrParent(pattern, rel string) bool {
	for p := rel; p != "." && p != ""; p = path.Dir(p) {
		if ok, _ := path.Match(pattern, p); ok {
			return true
		}
		if strings.HasPrefix(pattern, "**/") {
			if ok, _ := path.Match(strings.TrimPrefix(pattern, "**/"), path.Base(p)); ok {
				return true
			}
		}
	}
	return false
}

func writeTar(dir string, extra map[string][]byte, w io.Writer) error {
	rules := readIgnore(dir)
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		_, replaced := extra[rel]
		if replaced || rel == ".git" || strings.HasPrefix(rel, ".git/") || (rules.ignored(rel) && rel != "Dockerfile" && rel != ".dockerignore") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := extra[name]
		// 앱이 root가 아닌 사용자로 돌아도 읽을 수 있게 0644. 시각은 고정 — 내용이 같으면 빌드 캐시가 그대로 맞는다.
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	return tw.Close()
}
