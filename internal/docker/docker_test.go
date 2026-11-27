package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KangminNa/naru/internal/model"
)

// fakeDocker는 유닉스 소켓에서 Docker인 척한다.
func fakeDocker(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock") // 유닉스 소켓 경로는 짧아야 한다
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestContainers(t *testing.T) {
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" || r.URL.Query().Get("all") != "1" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[{"Names":["/shelf-blog"],"Image":"shelf-app-blog","State":"running","Status":"Up 3 hours",
			"Ports":[{"PrivatePort":3000,"Type":"tcp"},{"IP":"0.0.0.0","PrivatePort":3000,"PublicPort":3893,"Type":"tcp"},{"PrivatePort":53,"Type":"udp"}]},
			{"Names":["/naru-api"],"Image":"ghcr.io/me/api","State":"restarting","Status":"Restarting (1) 5 seconds ago",
			 "Labels":{"naru.service":"7","naru.deployment":"3"},"NetworkSettings":{"Networks":{"naru-net":{}}}},
			{"Names":["/cairn-web-1"],"Image":"cairn-web","State":"running","Status":"Up 3 days","Ports":[{"PrivatePort":3000,"Type":"tcp"}],
			 "Labels":{"com.docker.compose.project":"cairn","com.docker.compose.service":"web"},
			 "NetworkSettings":{"Networks":{"shelf-net":{},"cairn_default":{},"bridge":{}}}}]`))
	}))
	got, err := New(sock).containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	delete(got, "cairn-web-1")
	// 다른 컨테이너 목록이 쓰는 것 — 이미지, compose 묶음, 붙은 네트워크, Naru 서비스 번호
	all, err := NewContainers(New(sock)).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	web := all["cairn-web-1"]
	if web.Image != "cairn-web" || web.Project != "cairn" || web.Service != 0 || all["naru-api"].Service != 7 || all["naru-api"].Project != "" {
		t.Fatalf("%+v %+v", web, all["naru-api"])
	}
	if n := web.Networks; len(n) != 3 || n[0] != (model.DockerNetwork{Name: "bridge"}) || n[1] != (model.DockerNetwork{Name: "cairn_default", UserDefined: true}) {
		t.Fatalf("networks by name, the default ones marked: %+v", n)
	}
	if got["shelf-blog"].State != Running || got["naru-api"].State != "restarting" || len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if p := got["shelf-blog"].Ports; len(p) != 1 || p[0] != 3000 {
		t.Fatalf("exposed TCP ports, once each: %v", p)
	}
}

func TestErrorsCarryTheReason(t *testing.T) {
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"client version too old"}`, http.StatusBadRequest)
	}))
	if _, err := New(sock).containers(context.Background()); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("got %v", err)
	}
}

func TestUnreachableDocker(t *testing.T) {
	if _, err := New("/nonexistent/docker.sock").containers(context.Background()); err == nil {
		t.Fatal("a missing socket is an error")
	}
}

func TestSplitRef(t *testing.T) {
	cases := map[string][2]string{
		"traefik/whoami":       {"traefik/whoami", "latest"},
		"ghcr.io/me/api:v1":    {"ghcr.io/me/api", "v1"},
		"localhost:5000/app":   {"localhost:5000/app", "latest"},
		"localhost:5000/app:2": {"localhost:5000/app", "2"},
		"nginx@sha256:abc":     {"nginx@sha256:abc", ""},
	}
	for in, want := range cases {
		if n, tag := splitRef(in); n != want[0] || tag != want[1] {
			t.Errorf("%s → %s %s", in, n, tag)
		}
	}
}

func TestFollowReportsErrorsAndImageID(t *testing.T) {
	var log strings.Builder
	id, err := follow(strings.NewReader(`{"stream":"Step 1/2 : FROM alpine\n"}{"aux":{"ID":"sha256:abc"}}{"stream":"Successfully built\n"}`), &log)
	if err != nil || id != "sha256:abc" || !strings.Contains(log.String(), "Step 1/2") {
		t.Fatalf("%q %v %q", id, err, log.String())
	}
	if _, err := follow(strings.NewReader(`{"stream":"RUN false\n"}{"error":"The command '/bin/sh -c false' returned a non-zero code: 1"}`), &log); err == nil || !strings.Contains(err.Error(), "non-zero") {
		t.Fatalf("build errors surface: %v", err)
	}
}

func TestDemuxStripsFrameHeaders(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		h := []byte{stream, 0, 0, 0, 0, 0, 0, byte(len(s))}
		return append(h, s...)
	}
	raw := append(frame(1, "listening on :3000\n"), frame(2, "warn: x\n")...)
	if got := demux(raw); got != "listening on :3000\nwarn: x\n" {
		t.Fatalf("%q", got)
	}
	if got := demux([]byte("tty output\n")); got != "tty output\n" {
		t.Fatalf("tty logs pass through: %q", got)
	}
}

// 앱 네트워크가 없으면 처음 띄울 때 만든다 (compose는 external 네트워크를 만들어 주지 않는다).
// 실제 dockerd(27)는 없는 네트워크로도 create를 받고 start에서야 실패한다 — 그래서 먼저 확인한다.
func TestStartCreatesTheNetwork(t *testing.T) {
	var calls []string
	var created string
	networkExists := false
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/create" {
			b, _ := io.ReadAll(r.Body)
			created = string(b)
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks/naru-net":
			if !networkExists {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"network naru-net not found"}`))
				return
			}
			w.Write([]byte(`{"Name":"naru-net"}`))
		case r.URL.Path == "/networks/create":
			networkExists = true
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"net1"}`))
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"abc"}`))
		case r.URL.Path == "/containers/naru-x-1/start":
			if !networkExists {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"network naru-net not found"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewContainers(New(sock))
	spec := model.ContainerSpec{Name: "naru-x-1", Image: "img", Network: "naru-net", Aliases: []string{"naru-x"}}
	if err := c.Start(context.Background(), spec); err != nil {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
	if strings.Count(strings.Join(calls, " "), "/networks/create") != 1 {
		t.Fatalf("the network is created once: %v", calls)
	}
	if !strings.Contains(created, `"LogConfig":{"Config":{"max-file":"3","max-size":"10m"},"Type":"json-file"}`) {
		t.Fatalf("container logs are capped — Docker's default never stops growing: %s", created)
	}
}

// 다른 네트워크에는 만든 뒤, 띄우기 전에 같은 별칭으로 붙인다. 없는 네트워크면 만들기 전에 말한다 —
// Docker는 띄우지 않은 컨테이너를 없는 네트워크에 붙여도 받아 두고 start에서야 실패한다. 붙이지 못하면 만든 것을 지운다.
func TestStartJoinsTheOtherNetworksBeforeStarting(t *testing.T) {
	var calls []string
	var joinBody string
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/networks/naru-net" || r.URL.Path == "/networks/cairn_default" || r.URL.Path == "/networks/locked"):
			w.Write([]byte(`{"Name":"x"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/networks/gone":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"network gone not found"}`))
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"abc"}`))
		case r.URL.Path == "/networks/cairn_default/connect":
			b, _ := io.ReadAll(r.Body)
			joinBody = string(b)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/networks/locked/connect":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"operation not permitted"}`))
		case strings.HasSuffix(r.URL.Path, "/start"), r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewContainers(New(sock))
	spec := model.ContainerSpec{Name: "naru-x-1", Image: "img", Network: "naru-net", Aliases: []string{"naru-x"}, ExtraNetworks: []string{"naru-net", "cairn_default"}}
	if err := c.Start(context.Background(), spec); err != nil {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
	got := strings.Join(calls, " ")
	if i, j := strings.Index(got, "/networks/cairn_default/connect"), strings.Index(got, "/containers/naru-x-1/start"); i < 0 || j < i || strings.Contains(got, "/networks/naru-net/connect") {
		t.Fatalf("joined after create and before start, the app network not twice: %v", calls)
	}
	if !strings.Contains(joinBody, `"Container":"naru-x-1"`) || !strings.Contains(joinBody, `"Aliases":["naru-x"]`) {
		t.Fatalf("same alias on the other network: %s", joinBody)
	}

	calls = nil
	spec.ExtraNetworks = []string{"gone"}
	err := c.Start(context.Background(), spec)
	got = strings.Join(calls, " ")
	if err == nil || !strings.Contains(err.Error(), `네트워크 "gone"가 이 서버에 없어요`) || strings.Contains(got, "/containers/create") {
		t.Fatalf("a missing network is named before anything is created: %v %v", err, calls)
	}

	calls = nil
	spec.ExtraNetworks = []string{"locked"}
	err = c.Start(context.Background(), spec)
	got = strings.Join(calls, " ")
	if err == nil || !strings.Contains(err.Error(), `"locked"`) || !strings.Contains(got, "DELETE /containers/naru-x-1") || strings.Contains(got, "/start") {
		t.Fatalf("a failed join removes what was created and never starts it: %v %v", err, calls)
	}
}

// mux는 Docker가 TTY 없는 컨테이너 로그를 보내는 모양이다 — 8바이트 머리(흐름·길이) + 내용.
func mux(stream byte, s string) []byte {
	h := []byte{stream, 0, 0, 0, byte(len(s) >> 24), byte(len(s) >> 16), byte(len(s) >> 8), byte(len(s))}
	return append(h, s...)
}

func TestRecentLogsKeepTimeAndStream(t *testing.T) {
	var query string
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		body := append(mux(1, "2026-10-10T12:00:01.500000000Z listening on :80\n"), mux(2, "2026-10-10T12:00:02.000000000Z warn: slow\n2026-10-10T12:00:03.000000000Z second line\n")...)
		w.Write(body)
	}))
	lines, err := NewContainers(New(sock)).Recent(context.Background(), "naru-x-1", 50)
	if err != nil || len(lines) != 3 {
		t.Fatalf("%+v %v", lines, err)
	}
	if !strings.Contains(query, "timestamps=1") || !strings.Contains(query, "tail=50") {
		t.Fatalf("asks for timestamps and the tail: %s", query)
	}
	if lines[0].Text != "listening on :80" || lines[0].Stream != "stdout" || lines[0].At.Second() != 1 || lines[0].Source != model.LogApp {
		t.Fatalf("%+v", lines[0])
	}
	if lines[2].Stream != "stderr" || lines[2].Text != "second line" {
		t.Fatalf("%+v", lines[2])
	}
}

func TestUsage(t *testing.T) {
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/naru-blog-1/stats" || r.URL.Query().Get("stream") != "false" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"cpu_stats":{"cpu_usage":{"total_usage":3500000000},"system_cpu_usage":120000000000,"online_cpus":4},
			"precpu_stats":{"cpu_usage":{"total_usage":3000000000},"system_cpu_usage":116000000000},
			"memory_stats":{"usage":157286400,"limit":8238469120,"stats":{"inactive_file":52428800}},
			"networks":{"eth0":{"rx_bytes":1000,"tx_bytes":5000},"eth1":{"rx_bytes":24,"tx_bytes":6}}}`))
	}))
	u, err := NewContainers(New(sock)).Usage(context.Background(), "naru-blog-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.CPUPercent < 12.49 || u.CPUPercent > 12.51 {
		t.Fatalf("0.5s of CPU in 4s of the whole machine is 12.5%%: %v", u.CPUPercent)
	}
	if u.MemUsed != 104857600 || u.MemLimit != 8238469120 {
		t.Fatalf("file cache is not counted as used: %+v", u)
	}
	if u.NetIn != 1024 || u.NetOut != 5006 {
		t.Fatalf("bytes in and out, all networks together: %+v", u)
	}
	if _, err := NewContainers(New(sock)).Usage(context.Background(), "nope"); err == nil {
		t.Fatal("unknown container")
	}
}

// 웹서버 컨테이너를 다른 네트워크에 붙이고 떼기 — 이미 붙어 있거나 이미 떨어져 있으면 그대로 괜찮다.
func TestOneReadsRestartsAndOutOfMemory(t *testing.T) {
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"RestartCount":3,"State":{"Running":true,"Status":"running","ExitCode":0,"OOMKilled":true}}`))
	}))
	st, err := NewContainers(New(sock)).One(context.Background(), "naru-blog-4")
	if err != nil || st.Restarts != 3 || !st.OOMKilled || !st.Running {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestInspectReadsEnvVolumesAndSource(t *testing.T) {
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/cairn-web-1/json":
			w.Write([]byte(`{"Image":"sha256:img1","Config":{"Env":["DATABASE_URL=postgres://u:p@db/app","PATH=/usr/bin","NODE_ENV=production"],
				"Labels":{"org.opencontainers.image.source":"https://github.com/me/cairn"}},
				"Mounts":[{"Type":"bind","Source":"/srv/uploads","Destination":"/app/uploads","RW":true},
				          {"Type":"bind","Source":"/etc/localtime","Destination":"/etc/localtime","RW":false},
				          {"Type":"volume","Name":"cairn_cache","Source":"/var/lib/docker/volumes/cairn_cache/_data","Destination":"/cache","RW":true}]}`))
		case "/images/sha256:img1/json":
			w.Write([]byte(`{"Config":{"Env":["PATH=/usr/bin","NODE_ENV=development"]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such container"}`))
		}
	}))
	c := NewContainers(New(sock))
	d, err := c.Inspect(context.Background(), "cairn-web-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Env) != 3 || strings.Join(d.ImageEnv, "|") != "PATH=/usr/bin|NODE_ENV=development" || d.Source != "https://github.com/me/cairn" {
		t.Fatalf("%+v", d)
	}
	if strings.Join(d.Binds, "|") != "/etc/localtime:/etc/localtime:ro|/srv/uploads:/app/uploads" || strings.Join(d.NamedVolumes, "|") != "cairn_cache:/cache" {
		t.Fatalf("host paths are binds, named volumes apart: %v %v", d.Binds, d.NamedVolumes)
	}
	if _, err := c.Inspect(context.Background(), "gone"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("a missing container is ErrNotFound: %v", err)
	}
}

func TestNetworks(t *testing.T) {
	var calls []string
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		switch {
		case r.URL.Path == "/containers/couchdb/json":
			w.Write([]byte(`{"NetworkSettings":{"Networks":{"bridge":{},"db_default":{}}}}`))
		case r.URL.Path == "/containers/nas.local/json":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such container: nas.local"}`))
		case r.URL.Path == "/networks/db_default/connect" && strings.Contains(string(body), "naru-caddy"):
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/networks/other/connect":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"endpoint with name naru-caddy already exists in network other"}`))
		case r.URL.Path == "/networks/gone/disconnect":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"container 3f2 is not connected to network gone"}`))
		case strings.HasSuffix(r.URL.Path, "/disconnect"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewContainers(New(sock))
	nets, err := c.NetworksOf(context.Background(), "couchdb")
	if err != nil || fmt.Sprint(nets) != "[{bridge false} {db_default true}]" {
		t.Fatalf("%v %v", nets, err)
	}
	if _, err := c.NetworksOf(context.Background(), "nas.local"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("not a container: %v", err)
	}
	if err := c.Join(context.Background(), "db_default", "naru-caddy"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), `POST /networks/db_default/connect {"Container":"naru-caddy"}`) {
		t.Fatalf("%v", calls)
	}
	if err := c.Join(context.Background(), "other", "naru-caddy"); err != nil {
		t.Fatalf("already joined is fine: %v", err)
	}
	if err := c.Leave(context.Background(), "db_default", "naru-caddy"); err != nil {
		t.Fatal(err)
	}
	if err := c.Leave(context.Background(), "gone", "naru-caddy"); err != nil {
		t.Fatalf("already left is fine: %v", err)
	}
}

// 컨테이너 화면이 쓰는 것 — 다시 시작, 강제하지 않는 지우기, 볼륨·이미지 지우기의 409, system df, 서버에 연 포트.
func TestContainerScreenCalls(t *testing.T) {
	var calls []string
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		conflict := func(msg string) { w.WriteHeader(http.StatusConflict); fmt.Fprintf(w, `{"message":%q}`, msg) }
		switch r.Method + " " + r.URL.Path {
		case "POST /containers/web/restart":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /containers/busy":
			conflict("cannot remove container: container is running")
		case "DELETE /containers/old":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /volumes/used":
			conflict("volume is in use - [abc]")
		case "DELETE /volumes/free":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /images/sha256:busy":
			conflict("image is being used by stopped container 1234")
		case "GET /system/df":
			w.Write([]byte(`{"Images":[{"Id":"sha256:aaa","RepoTags":["me/app:1"],"Size":1000,"Created":1795000000,"Containers":0},
				{"Id":"sha256:bbb","RepoTags":["<none>:<none>"],"Size":5000,"Created":1795000000,"Containers":2}],
				"Volumes":[{"Name":"shop_db","Labels":{"com.docker.compose.project":"shop"},"UsageData":{"Size":2048,"RefCount":1}},
				{"Name":"loose","Labels":null,"UsageData":{"Size":-1,"RefCount":0}}]}`))
		case "GET /containers/json":
			w.Write([]byte(`[{"Names":["/mail"],"State":"running","Status":"Up 1 hour (healthy)","Ports":[
				{"PrivatePort":25,"PublicPort":25,"Type":"tcp","IP":"0.0.0.0"},{"PrivatePort":25,"PublicPort":25,"Type":"tcp","IP":"::"},
				{"PrivatePort":993,"PublicPort":993,"Type":"tcp"},{"PrivatePort":8080,"Type":"tcp"}]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	k := NewContainers(New(sock))
	if err := k.Restart(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveStopped(ctx, "busy", false); !errors.Is(err, model.ErrRunning) {
		t.Fatalf("a running container: %v", err)
	}
	if err := k.RemoveStopped(ctx, "old", false); err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveStopped(ctx, "gone", false); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := k.RemoveVolume(ctx, "used"); !errors.Is(err, model.ErrInUse) {
		t.Fatalf("%v", err)
	}
	if err := k.RemoveVolume(ctx, "free"); err != nil {
		t.Fatal(err)
	}
	if err := NewImages(New(sock)).Remove(ctx, "sha256:busy"); !errors.Is(err, model.ErrInUse) {
		t.Fatalf("%v", err)
	}
	for _, c := range calls {
		if strings.Contains(c, "force") {
			t.Fatalf("nothing is forced: %s", c)
		}
	}
	d, err := k.DiskUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Images) != 2 || d.Images[0].ID != "sha256:bbb" || len(d.Images[0].Tags) != 0 || d.Images[0].UsedBy != 2 || d.Images[1].Tags[0] != "me/app:1" {
		t.Fatalf("largest first, <none> dropped: %+v", d.Images)
	}
	if len(d.Volumes) != 2 || d.Volumes[0].Name != "shop_db" || d.Volumes[0].Project != "shop" || d.Volumes[0].UsedBy != 1 || d.Volumes[1].Size != 0 {
		t.Fatalf("%+v", d.Volumes)
	}
	all, err := k.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := all["mail"]; fmt.Sprint(m.Published) != "[25 993]" || fmt.Sprint(m.Ports) != "[25 993 8080]" {
		t.Fatalf("ports on the server vs. inside: %+v", m)
	}
}

// 네트워크 — 목록(붙은 것은 컨테이너 목록에서), 만들기, 지우기의 거절, 별칭, 상세의 IP·별칭, 익명 볼륨 지우기.
func TestNetworkCalls(t *testing.T) {
	var calls []string
	sock := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		switch r.Method + " " + r.URL.Path {
		case "GET /networks":
			w.Write([]byte(`[{"Name":"shop_default","Driver":"bridge","Internal":false,"Labels":{"com.docker.compose.project":"shop"},"IPAM":{"Config":[{"Subnet":"172.20.0.0/16"}]}},
				{"Name":"db-only","Driver":"bridge","Internal":true,"IPAM":{"Config":[]}},{"Name":"host","Driver":"host"}]`))
		case "GET /containers/json":
			w.Write([]byte(`[{"Names":["/shop-web-1"],"State":"running","NetworkSettings":{"Networks":{"shop_default":{}}}},
				{"Names":["/shop-db-1"],"State":"running","NetworkSettings":{"Networks":{"shop_default":{},"db-only":{}}}}]`))
		case "POST /networks/create":
			if strings.Contains(string(body), `"Name":"taken"`) {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"message":"network with name taken already exists"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"abc"}`))
		case "DELETE /networks/busy":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"error while removing network: network busy id abc has active endpoints"}`))
		case "DELETE /networks/free":
			w.WriteHeader(http.StatusNoContent)
		case "POST /networks/shop_default/connect":
			w.WriteHeader(http.StatusOK)
		case "DELETE /containers/old":
			w.WriteHeader(http.StatusNoContent)
		case "GET /containers/web/json":
			w.Write([]byte(`{"Name":"/web","NetworkSettings":{"Networks":{"shop_default":{"IPAddress":"172.20.0.3","Aliases":["web","3f2a9c1b7d4e","api"]}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	k := NewContainers(New(sock))
	rows, err := k.Networks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Name != "db-only" || !rows[0].Internal || strings.Join(rows[0].Containers, ",") != "shop-db-1" ||
		rows[2].Name != "shop_default" || rows[2].Subnet != "172.20.0.0/16" || rows[2].Project != "shop" || strings.Join(rows[2].Containers, ",") != "shop-db-1,shop-web-1" {
		t.Fatalf("%+v", rows)
	}
	taken, _ := model.ParseNetworkName("taken")
	if err := k.CreateNetwork(ctx, taken, false); !errors.Is(err, model.ErrInUse) {
		t.Fatalf("%v", err)
	}
	fresh, _ := model.ParseNetworkName("fresh")
	if err := k.CreateNetwork(ctx, fresh, true); err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveNetwork(ctx, "busy"); !errors.Is(err, model.ErrInUse) {
		t.Fatalf("active endpoints: %v", err)
	}
	if err := k.RemoveNetwork(ctx, "free"); err != nil {
		t.Fatal(err)
	}
	if err := k.JoinAs(ctx, "shop_default", "web", []string{"api"}); err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveStopped(ctx, "old", true); err != nil {
		t.Fatal(err)
	}
	d, err := k.Inspect(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Networks) != 1 || d.Networks[0].IP != "172.20.0.3" || strings.Join(d.Networks[0].Aliases, ",") != "api" {
		t.Fatalf("own name and short id are not aliases: %+v", d.Networks)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{`"Internal":true`, `"Aliases":["api"]`, "DELETE /containers/old?v=1 "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "force") {
		t.Fatal("nothing is forced")
	}
}
