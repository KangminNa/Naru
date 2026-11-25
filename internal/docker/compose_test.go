package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KangminNa/naru/internal/model"
)

func TestComposeCLIRunsInTheProjectFolder(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	bin := filepath.Join(dir, "compose")
	os.WriteFile(bin, []byte(`#!/bin/sh
printf '%s\n' "$@" > `+rec+`.args
pwd > `+rec+`.pwd
env > `+rec+`.env
ls "$DOCKER_CONFIG/cli-plugins" > `+rec+`.plugins
case "$*" in *fail*) echo "service web failed to build" >&2; exit 1;; esac
echo "Container naru-shop-web-1 Started"
`), 0o755)
	project := filepath.Join(dir, "shop")
	os.MkdirAll(project, 0o755)
	t.Setenv("NARU_SECRET", "x")
	c := NewComposeCLI(bin, "/usr/local/libexec/naru/buildx", "/run/docker.sock", filepath.Join(dir, "tmp"))
	p := model.ComposeProject{Name: "naru-shop", Dir: project, File: "deploy/compose.prod.yml"}
	var log strings.Builder
	if err := c.Up(context.Background(), p, &log); err != nil {
		t.Fatal(err)
	}
	read := func(ext string) string { b, _ := os.ReadFile(rec + ext); return string(b) }
	if got := strings.Join(strings.Fields(read(".args")), " "); got != "--ansi never --progress plain -p naru-shop -f deploy/compose.prod.yml up -d --remove-orphans" {
		t.Fatalf("%s", got)
	}
	if strings.TrimSpace(read(".pwd")) != project || !strings.Contains(log.String(), "naru-shop-web-1 Started") {
		t.Fatal("runs in the project folder, output goes to the log")
	}
	env := read(".env")
	if !strings.Contains(env, "DOCKER_HOST=unix:///run/docker.sock") || strings.Contains(env, "NARU_SECRET") || strings.TrimSpace(read(".plugins")) != "docker-buildx" {
		t.Fatalf("only what compose needs, with buildx as its plugin:\n%s", env)
	}
	c.Build(context.Background(), p, &log)
	if !strings.HasSuffix(strings.Join(strings.Fields(read(".args")), " "), "build --pull") {
		t.Fatal(read(".args"))
	}
	c.Down(context.Background(), p)
	if args := read(".args"); !strings.Contains(args, "down") || strings.Contains(args, "-v") {
		t.Fatalf("down keeps the volumes: %s", args)
	}
	p.File = "fail"
	if err := c.Up(context.Background(), p, &log); err == nil || !strings.Contains(err.Error(), "docker compose up") || !strings.Contains(log.String(), "failed to build") {
		t.Fatalf("%v", err)
	}
	if err := NewComposeCLI(filepath.Join(dir, "missing"), "", "/s", dir).Up(context.Background(), p, &log); err == nil || !strings.Contains(err.Error(), "docker compose") {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(left) != 0 {
		t.Fatal("config folders are removed")
	}
}
