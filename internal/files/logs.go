package files

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

const (
	archivesKept    = 10                  // 서비스마다 최근 배포 몇 개의 출력을 남기나
	archivesKeptFor = 30 * 24 * time.Hour // 이보다 오래된 것은 지운다
	archiveSuffix   = ".jsonl.gz"
)

// LogArchives는 지운 컨테이너의 앱 출력을 <root>/<서비스>/<배포>.jsonl.gz로 남긴다 (LogArchive).
// 앱이 직접 찍은 것이라 그대로 남긴다 — 데이터 폴더(권한 700) 안에 있다.
type LogArchives struct {
	root string
	now  func() time.Time
}

func NewLogArchives(root string) LogArchives { return LogArchives{root: root, now: time.Now} }

func (a LogArchives) dir(s model.ServiceName) string { return filepath.Join(a.root, s.String()) }

func (a LogArchives) file(s model.ServiceName, d model.DeploymentID) string {
	return filepath.Join(a.dir(s), strconv.FormatInt(int64(d), 10)+archiveSuffix)
}

// Keep은 그 배포의 앱 출력을 남기고, 오래된 것(10개 넘게, 30일 넘게)을 지운다. 줄이 없으면 남기지 않는다.
func (a LogArchives) Keep(s model.ServiceName, d model.DeploymentID, lines []model.LogLine) error {
	if len(lines) == 0 || d <= 0 || s.String() == "" {
		return nil
	}
	if err := os.MkdirAll(a.dir(s), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(a.dir(s), ".keep-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	zw := gzip.NewWriter(tmp)
	enc := json.NewEncoder(zw)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), a.file(s, d)); err != nil {
		return err
	}
	return a.prune(s)
}

func (a LogArchives) prune(s model.ServiceName) error {
	list, err := a.List(s)
	if err != nil {
		return err
	}
	for i, e := range list {
		if i >= archivesKept || a.now().Sub(e.At) > archivesKeptFor {
			os.Remove(a.file(s, e.Deployment))
		}
	}
	return nil
}

// List는 남은 것들이다 — 최근 배포부터. 하나도 없으면 비어 있다.
func (a LogArchives) List(s model.ServiceName) ([]model.LogArchiveEntry, error) {
	entries, err := os.ReadDir(a.dir(s))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []model.LogArchiveEntry
	for _, e := range entries {
		num, ok := strings.CutSuffix(e.Name(), archiveSuffix)
		n, err := strconv.ParseInt(num, 10, 64)
		if !ok || err != nil || n <= 0 || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, model.LogArchiveEntry{Deployment: model.DeploymentID(n), At: info.ModTime(), Bytes: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deployment > out[j].Deployment })
	return out, nil
}

// Read는 그 배포의 앱 출력이다. 없으면 model.ErrNotFound.
func (a LogArchives) Read(s model.ServiceName, d model.DeploymentID) ([]model.LogLine, error) {
	f, err := os.Open(a.file(s, d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out []model.LogLine
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l model.LogLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// RemoveAll은 그 서비스의 것을 모두 지운다 — 서비스를 지울 때.
func (a LogArchives) RemoveAll(s model.ServiceName) error {
	if s.String() == "" {
		return nil
	}
	return os.RemoveAll(a.dir(s))
}
