package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBuildx는 buildx인 척하는 셸 스크립트다 — 받은 인자·환경·stdin을 rec 폴더에 남기고, --iidfile에 ID를 쓴다.
func fakeBuildx(t *testing.T, body string) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	os.MkdirAll(rec, 0o755)
	bin = filepath.Join(dir, "buildx")
	script := `#!/bin/sh
rec=` + rec + `
printf '%s\n' "$@" > $rec/args
env > $rec/env
pwd > $rec/pwd
cat > $rec/stdin
iid=""
while [ $# -gt 0 ]; do [ "$1" = "--iidfile" ] && iid=$2; shift; done
` + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, rec
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestBuildxBuildsFromTheTarOnStdin(t *testing.T) {
	bin, rec := fakeBuildx(t, `echo "#1 [1/2] FROM busybox"; echo sha256:abc > $iid`)
	t.Setenv("NARU_SECRET_SHOULD_NOT_LEAK", "x")
	temp := filepath.Join(t.TempDir(), "buildx")
	var log strings.Builder
	id, err := NewBuildxBuilder(bin, "/run/docker.sock", temp).Build(context.Background(), strings.NewReader("TARBALL"), "naru-blog:7", &log)
	if err != nil || id != "sha256:abc" {
		t.Fatalf("%q %v\n%s", id, err, log.String())
	}
	if args := strings.Fields(read(t, rec+"/args")); strings.Join(args, " ") !=
		"build --progress=plain --provenance=false --sbom=false --iidfile "+args[5]+" --tag naru-blog:7 -" {
		t.Fatalf("args: %v", args)
	}
	if read(t, rec+"/stdin") != "TARBALL" || !strings.Contains(log.String(), "#1 [1/2] FROM busybox") {
		t.Fatal("the tar goes in on stdin, the progress comes out in the log")
	}
	env := read(t, rec+"/env")
	if !strings.Contains(env, "DOCKER_HOST=unix:///run/docker.sock") || !strings.Contains(env, "TMPDIR="+temp) || strings.Contains(env, "NARU_SECRET_SHOULD_NOT_LEAK") {
		t.Fatalf("only what buildx needs, nothing from Naru:\n%s", env)
	}
	if left, _ := os.ReadDir(temp); len(left) != 0 {
		t.Fatalf("the config folder is gone after the build: %v", left)
	}
}

func TestBuildxFailuresSayWhy(t *testing.T) {
	temp := t.TempDir()
	bin, _ := fakeBuildx(t, `echo "ERROR: failed to solve: process did not complete successfully" >&2; exit 1`)
	var log strings.Builder
	_, err := NewBuildxBuilder(bin, "/s", temp).Build(context.Background(), strings.NewReader(""), "t:1", &log)
	if err == nil || !strings.Contains(log.String(), "ERROR: failed to solve") || !strings.Contains(err.Error(), "ERROR") {
		t.Fatalf("%v\n%s", err, log.String())
	}

	bin, _ = fakeBuildx(t, `echo not-an-id > $iid`)
	if _, err := NewBuildxBuilder(bin, "/s", temp).Build(context.Background(), strings.NewReader(""), "t:1", &log); err == nil {
		t.Fatal("an odd image ID is refused")
	}

	if _, err := NewBuildxBuilder(filepath.Join(temp, "missing"), "/s", temp).Build(context.Background(), strings.NewReader(""), "t:1", &log); err == nil || !strings.Contains(err.Error(), "buildx") {
		t.Fatalf("a missing buildx says so: %v", err)
	}

	bin, _ = fakeBuildx(t, `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := NewBuildxBuilder(bin, "/s", temp).Build(ctx, strings.NewReader(""), "t:1", &log); err != context.DeadlineExceeded || time.Since(start) > 10*time.Second {
		t.Fatalf("cancelling stops the build: %v after %s", err, time.Since(start))
	}
}
