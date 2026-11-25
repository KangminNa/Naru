package files

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func folder(s string) model.FolderPath {
	p, err := model.ParseFolderPath(s)
	if err != nil {
		panic(err)
	}
	return p
}

func TestInside(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "site", "dist"), 0o755)
	w := NewTempFolders(t.TempDir())
	if p, err := w.Inside(base, folder("/site/dist/")); err != nil || p != filepath.Join(base, "site", "dist") {
		t.Fatalf("%q %v", p, err)
	}
	if p, err := w.Inside(base, folder("")); err != nil || p != base {
		t.Fatalf("empty means the root: %q %v", p, err)
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(base, "sneaky"))
	if _, err := w.Inside(base, folder("sneaky")); err == nil {
		t.Error("a symlink pointing outside must not be followed")
	}
	if _, err := w.Inside(base, folder("missing")); err == nil {
		t.Error("a missing folder is an error")
	}
}

func TestBorrowAndClear(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	w := NewTempFolders(root)
	dir, giveBack, err := w.Borrow("../../escape")
	if err != nil || filepath.Dir(dir) != root {
		t.Fatalf("a borrowed folder stays under the root: %q %v", dir, err)
	}
	os.MkdirAll(dir, 0o755)
	giveBack()
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("giving back removes it")
	}
	os.MkdirAll(filepath.Join(root, "left-over"), 0o755)
	w.Clear()
	if _, err := os.Stat(root); err == nil {
		t.Fatal("Clear removes what a previous run left")
	}
}

// Dockerfile 하나에 앱이 여럿 — 단계마다 EXPOSE, 이어받은 단계의 것도.
func TestDockerfileStages(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"Dockerfile": `FROM node:24 AS base
EXPOSE 9229
FROM base AS deps
FROM deps AS server-build
FROM node:24-slim AS server
EXPOSE 4000
FROM node:24-slim AS Web
EXPOSE 3000
FROM web AS web-debug
FROM --platform=linux/arm64 node:24-slim AS egress
EXPOSE 3128
`})
	r := DockerfileReader{}
	if got := strings.Join(r.Stages(dir), " "); got != "base deps server-build server web web-debug egress" {
		t.Fatalf("stage names in order, lowercased: %s", got)
	}
	for target, want := range map[string]string{"": "[3128]", "web": "[3000]", "WEB": "[3000]", "web-debug": "[3000]", "server": "[4000]", "server-build": "[9229]", "nope": "[]"} {
		got, found := r.Ports(dir, target)
		if !found || fmt.Sprint(got) != want {
			t.Errorf("%q: %v (want %s)", target, got, want)
		}
	}
}

func TestDockerfilePorts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"Dockerfile": "FROM node:20\nEXPOSE 3000\nexpose 9229/tcp 53/udp\n# EXPOSE 1\n"})
	got, found := DockerfileReader{}.Ports(dir, "")
	if !found || len(got) != 2 || got[0] != 3000 || got[1] != 9229 {
		t.Fatalf("%v %v", got, found)
	}
	if ports, found := (DockerfileReader{}).Ports(t.TempDir(), ""); found || ports != nil {
		t.Fatal("no Dockerfile, no ports")
	}
}

func tarNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for name := range tarFiles(t, dir, nil) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// tarFiles는 묶음 안의 이름 → 내용이다.
func tarFiles(t *testing.T, dir string, extra map[string][]byte) map[string]string {
	t.Helper()
	rc := TarPacker{}.Pack(dir, extra)
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := out[h.Name]; dup {
			t.Fatalf("%s is in the tar twice", h.Name)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
	return out
}

// buildx는 stdin의 첫 1024바이트에서 첫 항목을 읽어 tar인지 가린다 — 맨 앞 파일 이름이 길거나 한글이어도 tar로 보여야 한다.
func TestPackLooksLikeATarFromTheFirstKilobyte(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"0" + strings.Repeat("긴이름", 20) + ".txt": "x", // 이 폴더에서 맨 앞 — USTAR에 들어가지 않는 이름
		"Dockerfile": "FROM scratch",
	})
	rc := TarPacker{}.Pack(dir, nil)
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	h, err := tar.NewReader(bytes.NewReader(raw[:1024])).Next()
	if err != nil || h.Name != "./" {
		t.Fatalf("the first kilobyte holds a whole entry: %v %+v", err, h)
	}
	if names := tarNames(t, dir); len(names) != 3 || !strings.HasPrefix(names[1], "0긴이름") {
		t.Fatalf("everything is still there: %v", names)
	}
}

func TestPackAddsTheEnvFileWithoutWritingIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"Dockerfile":    "FROM scratch\nCOPY .env .\n",
		".dockerignore": ".env\n.env.*\n",
		".env":          "FROM_REPO=1\n",
		".env.local":    "LOCAL=1\n",
	})
	got := tarFiles(t, dir, map[string][]byte{".env": []byte("FROM_NARU=1\n")})
	if got[".env"] != "FROM_NARU=1\n" {
		t.Fatalf("Naru's .env replaces the repo's and ignores .dockerignore: %q", got[".env"])
	}
	if _, ok := got[".env.local"]; ok {
		t.Fatal(".dockerignore still applies to everything else")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".env")); string(b) != "FROM_REPO=1\n" {
		t.Fatalf("nothing is written to the folder: %q", b)
	}
	if got := tarFiles(t, dir, nil); got[".env"] != "" || len(got) != 3 {
		t.Fatalf("without extra the folder is packed as before: %v", got)
	}
}

func TestPackHonoursDockerignore(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"Dockerfile":              "FROM scratch",
		".dockerignore":           "node_modules\n*.log\nsecrets/\n!keep.log\n# comment\n",
		"app.js":                  "x",
		"debug.log":               "x",
		"keep.log":                "x",
		"node_modules/a/index.js": "x",
		"secrets/key":             "x",
		"src/lib.js":              "x",
		".git/config":             "x",
	})
	got := strings.Join(tarNames(t, dir), " ")
	for _, want := range []string{"Dockerfile", ".dockerignore", "app.js", "keep.log", "src/", "src/lib.js"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	for _, gone := range []string{"debug.log", "node_modules", "secrets", ".git"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s must be excluded: %s", gone, got)
		}
	}
}

func TestPublishSkipsHiddenFilesAndLinks(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	write(t, src, map[string]string{"index.html": "<h1>hi</h1>", "assets/app.css": "body{}", ".git/HEAD": "ref",
		".env": "SECRET=1", ".github/workflows/ci.yml": "x", "assets/.DS_Store": "x", ".well-known/security.txt": "contact"})
	os.Symlink("/etc/passwd", filepath.Join(src, "passwd"))
	sites := NewSiteFolders(root)
	name, _ := model.ParseServiceName("landing")
	to := model.SiteFolder{Site: name, Release: 4}
	n, err := sites.Publish(src, to)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	dst := filepath.Join(root, "landing", "4")
	for _, gone := range []string{".env", ".github", "assets/.DS_Store", ".git"} {
		if _, err := os.Stat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s must not be published", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ".well-known", "security.txt")); err != nil {
		t.Error(".well-known is a web standard and stays")
	}
	if _, err := os.Lstat(filepath.Join(dst, "passwd")); err == nil {
		t.Error("symlinks are not published")
	}

	if _, err := sites.Publish(t.TempDir(), model.SiteFolder{Site: name, Release: 5}); err != model.ErrNoIndexHTML {
		t.Fatal("a folder without index.html is not published")
	}
	if err := sites.Copy(to, model.SiteFolder{Site: name, Release: 6}); err != nil || !sites.Exists(model.SiteFolder{Site: name, Release: 6}) {
		t.Fatal("copy to a new release")
	}
	for i := 7; i < 14; i++ {
		sites.Copy(to, model.SiteFolder{Site: name, Release: model.DeploymentID(i)})
	}
	sites.Prune(name, 3, 4)
	if !sites.Exists(to) || !sites.Exists(model.SiteFolder{Site: name, Release: 13}) || sites.Exists(model.SiteFolder{Site: name, Release: 9}) {
		t.Fatal("prune keeps the newest and the live one")
	}
	sites.RemoveSite(name)
	if sites.Exists(to) {
		t.Fatal("removed")
	}
}

func TestLogArchivesKeepTenAndThirtyDays(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	a := LogArchives{root: root, now: func() time.Time { return now }}
	blog, _ := model.ParseServiceName("blog")
	line := func(s string) model.LogLine {
		return model.LogLine{At: now, Source: model.LogApp, Stream: "stderr", Text: s}
	}
	if err := a.Keep(blog, 3, []model.LogLine{line("listening on :3000"), line("FATAL heap out of memory")}); err != nil {
		t.Fatal(err)
	}
	got, err := a.Read(blog, 3)
	if err != nil || len(got) != 2 || got[1].Text != "FATAL heap out of memory" || got[1].Stream != "stderr" || !got[1].At.Equal(now) {
		t.Fatalf("%+v %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(root, "blog")); info.Mode().Perm() != 0o700 {
		t.Fatalf("the folder is private: %v", info.Mode())
	}
	if err := a.Keep(blog, 4, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(blog, 4); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("nothing to keep, nothing kept")
	}

	for d := model.DeploymentID(5); d <= 16; d++ {
		a.Keep(blog, d, []model.LogLine{line("hi")})
	}
	list, _ := a.List(blog)
	if len(list) != 10 || list[0].Deployment != 16 || list[9].Deployment != 7 || list[0].Bytes == 0 {
		t.Fatalf("the newest ten, newest first: %+v", list)
	}
	os.Chtimes(filepath.Join(root, "blog", "12.jsonl.gz"), now.Add(-31*24*time.Hour), now.Add(-31*24*time.Hour))
	a.Keep(blog, 17, []model.LogLine{line("hi")})
	list, _ = a.List(blog)
	var nums []model.DeploymentID
	for _, e := range list {
		nums = append(nums, e.Deployment)
	}
	if fmt.Sprint(nums) != "[17 16 15 14 13 11 10 9 8]" {
		t.Fatalf("older than 30 days goes too: %+v", list)
	}
	other, _ := model.ParseServiceName("other")
	if l, err := a.List(other); err != nil || len(l) != 0 {
		t.Fatal("a service with nothing kept has an empty list")
	}
	if err := a.RemoveAll(blog); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "blog")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removing the service removes its logs")
	}
}

func TestProjectFoldersStayAndSwap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compose")
	p := NewProjectFolders(root)
	shop, _ := model.ParseServiceName("shop")
	deploy := func(body, commit string, env string) string {
		fresh, err := p.Fresh(shop)
		if err != nil {
			t.Fatal(err)
		}
		write(t, fresh, map[string]string{"compose.yml": body, ".git/config": "[remote]"})
		dir, err := p.Promote(shop, fresh, []byte(env), model.Commit{Hash: commit})
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dir := deploy("v1", "aaa111", "SECRET=1\n")
	if dir != filepath.Join(root, "shop") || p.Dir(shop) != dir || p.Commit(shop) != "aaa111" {
		t.Fatalf("%s %s", dir, p.Commit(shop))
	}
	if info, err := os.Stat(filepath.Join(dir, ".env")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf(".env is private: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal("no .git left behind")
	}
	deploy("v2", "bbb222", "")
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yml")); string(b) != "v2" || p.Commit(shop) != "bbb222" {
		t.Fatal("the same path now holds the new code")
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("an empty .env box writes no file")
	}
	left, _ := filepath.Glob(filepath.Join(root, ".*"))
	os.MkdirAll(filepath.Join(root, ".shop-stale"), 0o700)
	p.Fresh(shop)
	if stale, _ := filepath.Glob(filepath.Join(root, ".shop-stale")); len(stale) != 0 || len(left) != 0 {
		t.Fatalf("leftovers are cleared: %v %v", left, stale)
	}
	if _, err := p.Promote(shop, "/tmp/elsewhere", nil, model.Commit{}); err == nil {
		t.Fatal("only folders it made")
	}
	p.Remove(shop)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("removed")
	}
	if _, err := NewProjectFolders("").Fresh(shop); err == nil {
		t.Fatal("no root, no compose")
	}
}
