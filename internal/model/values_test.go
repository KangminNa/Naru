package model

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDomainNames(t *testing.T) {
	cases := map[string]string{
		" HTTPS://Naru.Example.com/admin ": "naru.example.com",
		"naru.example.com.":                "naru.example.com",
		"http://a.b.co":                    "a.b.co",
	}
	for in, want := range cases {
		if got, err := ParseDomainName(in); err != nil || got.String() != want {
			t.Errorf("ParseDomainName(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, ok := range []string{"naru.example.com", "a.co", "my-app.nakangmin.duckdns.org"} {
		if _, err := ParseDomainName(ok); err != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "localhost", "198.51.100.24", "-a.example.com", "a_b.example.com", "naru.example.com:8080", "a..b.com"} {
		if _, err := ParseDomainName(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if d, err := ParseOptionalDomainName("  "); err != nil || !d.IsZero() {
		t.Fatal("an optional domain may be empty")
	}
	if d, _ := ParseDomainName("blog.example.com"); d.FirstLabel() != "blog" {
		t.Fatal(d.FirstLabel())
	}
}

func TestRepoURLsAndBranches(t *testing.T) {
	for _, ok := range []string{"https://github.com/me/blog", "https://github.com/me/blog.git", "http://git.lan/x"} {
		if _, err := ParseRepoURL(ok); err != nil {
			t.Errorf("%s should pass", ok)
		}
	}
	for _, bad := range []string{"", "-uhttps://x", "file:///etc", "ssh://git@github.com/x", "https://user:pw@github.com/x", "https://github.com/a b", "ext::sh -c id"} {
		if _, err := ParseRepoURL(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	for _, bad := range []string{"-x", "--upload-pack=x", "a..b", "feat x"} {
		if _, err := ParseBranch(bad); err == nil {
			t.Errorf("branch %q should be refused", bad)
		}
	}
	if b, err := ParseBranch("feature/new-ui"); err != nil || b.String() != "feature/new-ui" {
		t.Error("slashes in branches are normal")
	}
	if b, err := ParseBranch(""); err != nil || !b.IsZero() {
		t.Error("an empty branch means the default")
	}
	if r, _ := ParseRepoURL("https://github.com/me/Blog.git"); r.Name() != "Blog" {
		t.Error(r.Name())
	}
}

func TestSuggestedNames(t *testing.T) {
	img, _ := ParseImageRef("ghcr.io/me/api:latest")
	ext, _ := ParseExternalAddress("https://router.lan:443")
	repo, _ := ParseRepoURL("https://github.com/me/Blog.git")
	cases := map[string]string{
		"blog":    SuggestServiceName(repo.Name()).String(),
		"api":     SuggestServiceName(img.Name()).String(),
		"router":  SuggestServiceName(ext.Host()).String(),
		"service": SuggestServiceName("!!!").String(),
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	long, _ := ParseServiceName("abcdefghijklmnopqrstuvwxyz01234")
	if s := long.WithSuffix(12).String(); len(s) > 31 || s[len(s)-3:] != "-12" {
		t.Errorf("suffixes keep names within 31: %q", s)
	}
}

func TestExternalAddresses(t *testing.T) {
	cases := map[string]string{
		"localhost:5000":         "host.docker.internal:5000",
		"127.0.0.1:81":           "host.docker.internal:81",
		"https://router.lan:443": "https://router.lan:443",
		"http://192.168.0.20:80": "192.168.0.20:80",
	}
	for in, want := range cases {
		if got, err := ParseExternalAddress(in); err != nil || got.String() != want {
			t.Errorf("%q → %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"nope", "x:0", "x:99999", "a b:80"} {
		if _, err := ParseExternalAddress(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestFolderPaths(t *testing.T) {
	for in, want := range map[string]string{"/site/dist/": "site/dist", "": "", "a\\b": "a/b"} {
		if got, err := ParseFolderPath(in); err != nil || got.String() != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"../etc", "site/../../x", ".."} {
		if _, err := ParseFolderPath(bad); err == nil {
			t.Errorf("%q must not leave the repository", bad)
		}
	}
}

func TestKindNames(t *testing.T) {
	for _, k := range AllKinds() {
		if got, err := ParseKindName(string(k)); err != nil || got != k {
			t.Error(k)
		}
	}
	if _, err := ParseKindName("nonsense"); err == nil {
		t.Fatal("unknown kinds are refused")
	}
}

func TestLiveDeployment(t *testing.T) {
	cases := []struct {
		l    LiveState
		want DeploymentID
	}{
		{LiveState{Alias: "naru-app", Instance: "naru-app-12"}, 12},
		{LiveState{Alias: "shelf-blog"}, 0},
		{LiveState{Release: "7"}, 7},
	}
	for _, c := range cases {
		if got := c.l.LiveDeployment(); got != c.want {
			t.Errorf("%+v → %d", c.l, got)
		}
	}
}

func TestCertificatesCoverDomains(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	certs := Certificates{
		{Names: []string{"blog.example.com"}, Issuer: "Let's Encrypt", NotBefore: now.Add(-day), NotAfter: now.Add(60 * day)},
		{Names: []string{"blog.example.com"}, Issuer: "ZeroSSL", NotBefore: now.Add(-day), NotAfter: now.Add(80 * day)},
		{Names: []string{"*.apps.example.com"}, Issuer: "Let's Encrypt", NotBefore: now.Add(-day), NotAfter: now.Add(30 * day)},
		{Names: []string{"old.example.com"}, NotBefore: now.Add(-90 * day), NotAfter: now.Add(-day)},
		{Names: []string{"soon.example.com"}, NotBefore: now.Add(-80 * day), NotAfter: now.Add(5 * day)},
		{Names: []string{"future.example.com"}, NotBefore: now.Add(day), NotAfter: now.Add(90 * day)},
		{Names: []string{"dev.localhost"}, NotBefore: now.Add(-6 * time.Hour), NotAfter: now.Add(6 * time.Hour)}, // 내부 CA 12시간짜리
		{Names: []string{"stuck.localhost"}, NotBefore: now.Add(-11 * time.Hour), NotAfter: now.Add(time.Hour)},  // 갱신이 막힘
		{Names: []string{"short.example.com"}, NotBefore: now.Add(-40 * day), NotAfter: now.Add(5 * day)},        // 45일짜리
	}
	d := func(s string) DomainName { v, _ := ParseDomainName(s); return v }

	if st := certs.For(d("Blog.Example.com"), now); !st.Usable(now) || st.Issuer != "ZeroSSL" {
		t.Fatalf("the longest-lasting certificate wins: %+v", st)
	}
	if !certs.For(d("a.apps.example.com"), now).Usable(now) {
		t.Fatal("a wildcard covers one level")
	}
	for _, no := range []string{"a.b.apps.example.com", "apps.example.com", "old.example.com", "future.example.com", "nothing.example.com"} {
		if certs.For(d(no), now).Usable(now) {
			t.Errorf("%s has no usable certificate", no)
		}
	}
	if st := certs.For(d("soon.example.com"), now); !st.EndsSoon(now) {
		t.Fatal("five days left is soon")
	}
	if certs.For(d("blog.example.com"), now).EndsSoon(now) {
		t.Fatal("eighty days left is not soon")
	}
	if certs.For(d("dev.localhost"), now).EndsSoon(now) {
		t.Fatal("a short-lived certificate halfway through is not ending soon — soon is relative to its lifetime")
	}
	if !certs.For(d("stuck.localhost"), now).EndsSoon(now) || !certs.For(d("short.example.com"), now).EndsSoon(now) {
		t.Fatal("under a sixth of the lifetime left is soon")
	}
	if (CertificateState{}).Usable(now) {
		t.Fatal("no certificate is not usable")
	}
}

// .env를 그대로 붙여 넣어도 받는다 — 저장할 때 "KEY=값"(= 뒤 그대로)으로 바꾸고, 고친 것을 알려 준다.
func TestEnvVarsAcceptDotenvAndSayWhatChanged(t *testing.T) {
	cases := []struct {
		in, stored string
		did        []EnvFix
	}{
		{"KEY=abc", "KEY=abc", nil},
		{"export KEY=abc", "KEY=abc", []EnvFix{EnvFixExport}},
		{`KEY="abc"`, "KEY=abc", []EnvFix{EnvFixQuotes}},
		{`KEY='abc'`, "KEY=abc", []EnvFix{EnvFixQuotes}},
		{`KEY="a \"b\" c\\d"`, `KEY=a "b" c\d`, []EnvFix{EnvFixQuotes}},
		{`KEY='a \"b'`, `KEY=a \"b`, []EnvFix{EnvFixQuotes}},
		{"KEY=abc # 설명", "KEY=abc", []EnvFix{EnvFixComment}},
		{"KEY=http://x/#frag", "KEY=http://x/#frag", nil},
		{`KEY="abc # 아님"`, "KEY=abc # 아님", []EnvFix{EnvFixQuotes}},
		{`KEY="abc"   # 설명`, "KEY=abc", []EnvFix{EnvFixQuotes, EnvFixComment}},
		{"KEY = abc ", "KEY=abc", []EnvFix{EnvFixSpaces}},
		{"KEY=", "KEY=", nil},
		{`KEY=""`, "KEY=", []EnvFix{EnvFixQuotes}},
		{"KEY=a=b", "KEY=a=b", nil},
		{"export  KEY=\"x\"", "KEY=x", []EnvFix{EnvFixExport, EnvFixQuotes}},
	}
	for _, c := range cases {
		e, notes, err := ParseEnvVars(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if e.Text() != c.stored {
			t.Errorf("%q stored as %q, want %q", c.in, e.Text(), c.stored)
		}
		var did []EnvFix
		for _, n := range notes {
			if n.Line != 1 || n.Key != "KEY" {
				t.Errorf("%q: note %+v", c.in, n)
			}
			did = append(did, n.Did)
		}
		if fmt.Sprint(did) != fmt.Sprint(c.did) {
			t.Errorf("%q: notes %v, want %v", c.in, did, c.did)
		}
		if got := e.List(); len(got) != 1 || got[0] != c.stored {
			t.Errorf("%q: the container gets %v", c.in, got)
		}
	}

	// 주석·빈 줄은 그대로, 같은 이름은 둘 다 두되(나중 것이 이긴다) 알린다
	e, notes, err := ParseEnvVars("# DB\nA=1\n\nexport B='two words'\nA=3\n")
	if err != nil {
		t.Fatal(err)
	}
	if e.Text() != "# DB\nA=1\n\nB=two words\nA=3\n" {
		t.Fatalf("%q", e.Text())
	}
	if fmt.Sprint(notes) != fmt.Sprint([]EnvNote{{4, "B", EnvFixExport}, {4, "B", EnvFixQuotes}, {5, "A", EnvFixDuplicate}}) {
		t.Fatalf("%+v", notes)
	}
	if fmt.Sprint(e.List()) != "[A=1 B=two words A=3]" {
		t.Fatalf("%v", e.List())
	}

	// 받지 않는 것 — 몇 번째 줄인지 함께
	for in, want := range map[string]InputError{
		"A=1\nbad":                        {Field: "env", Code: "env", Line: 2},
		"A=1\n1BAD=x":                     {Field: "env", Code: "env", Line: 2},
		`A="abc`:                          {Field: "env", Code: "envquote", Line: 1},
		"A=1\nKEY=\"line one\nline two\"": {Field: "env", Code: "envmultiline", Line: 2},
		`KEY="a\nb"`:                      {Field: "env", Code: "envmultiline", Line: 1},
		`KEY="a"b`:                        {Field: "env", Code: "envquote", Line: 1},
	} {
		_, _, err := ParseEnvVars(in)
		ie, ok := AsInputError(err)
		if !ok || ie != want {
			t.Errorf("%q: got %v, want %+v", in, err, want)
		}
	}

	// 이미 저장된 값은 그대로 읽는다 — 따옴표가 든 예전 값의 뜻이 바뀌지 않는다
	if got := StoredEnvVars(`KEY="kept"`).List(); len(got) != 1 || got[0] != `KEY="kept"` {
		t.Fatalf("%v", got)
	}
}

func TestEnvFileKeepsWhatWasWritten(t *testing.T) {
	in := "# 빌드에 필요\r\nNEXT_PUBLIC_API=\"https://api.example.com\" # 그대로\nexport A='1'"
	f, err := ParseEnvFile(in)
	if err != nil {
		t.Fatal(err)
	}
	want := "# 빌드에 필요\nNEXT_PUBLIC_API=\"https://api.example.com\" # 그대로\nexport A='1'\n"
	if f.Text() != want || string(f.Bytes()) != want || f.Lines() != 3 {
		t.Fatalf("written as is, ending with a newline: %q (%d lines)", f.Text(), f.Lines())
	}
	if f, err := ParseEnvFile(" \n\t\n"); err != nil || !f.IsZero() {
		t.Fatalf("blank means no file: %q %v", f.Text(), err)
	}
	for _, bad := range []string{"A=1\x00", strings.Repeat("A", 64<<10+1)} {
		if _, err := ParseEnvFile(bad); err == nil {
			t.Errorf("%.10q… should be refused", bad)
		} else if ie, _ := AsInputError(err); ie.Field != "envfile" || ie.Code != "envfile" {
			t.Errorf("%v", err)
		}
	}
	if StoredEnvFile("A=1").Text() != "A=1" {
		t.Fatal("stored text is read as is")
	}
}

func TestNetworkNames(t *testing.T) {
	n, err := ParseNetworkNames(" cairn_default, monitor.net\ncairn_default  db-net ")
	if err != nil || n.String() != "cairn_default monitor.net db-net" {
		t.Fatalf("spaces, commas and lines separate; each name once: %q %v", n.String(), err)
	}
	if n, err := ParseNetworkNames("  "); err != nil || !n.IsZero() {
		t.Fatalf("blank means none: %q %v", n.String(), err)
	}
	for _, bad := range []string{"bridge", "ok host", "none", "-x", "a/b", "a;rm", strings.Repeat("n", 129), "a b c d e f g h i"} {
		_, err := ParseNetworkNames(bad)
		if ie, ok := AsInputError(err); !ok || ie.Field != "networks" || ie.Code != "network" {
			t.Errorf("%.20q should be refused: %v", bad, err)
		}
	}
}

func TestEnvVarsFromAnotherContainer(t *testing.T) {
	typed, _, _ := ParseEnvVars("NODE_ENV=production\nEXTRA=1")
	e, err := EnvVarsFrom([]string{"DATABASE_URL=postgres://u:p@db/app", "NODE_ENV=development"}, typed)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.List(), "|"); got != "DATABASE_URL=postgres://u:p@db/app|NODE_ENV=development|NODE_ENV=production|EXTRA=1" {
		t.Fatalf("the container's come first, so what was typed wins: %s", got)
	}
	if e, err := EnvVarsFrom(nil, typed); err != nil || e.Text() != typed.Text() {
		t.Fatal("nothing chosen leaves what was typed")
	}
	for in, want := range map[string]string{"KEY=line one\nline two": "envmultiline", "1BAD=x": "env", "noequals": "env"} {
		_, err := EnvVarsFrom([]string{"OK=1", in}, typed)
		if ie, ok := AsInputError(err); !ok || ie.Code != want || ie.Line != 2 {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestLogQuery(t *testing.T) {
	q := ParseLogQuery("app", "11", "  Error  ")
	if q.Show != LogsApp || q.Deployment != 11 || q.Text != "Error" {
		t.Fatalf("%+v", q)
	}
	if q := ParseLogQuery("weird", "-3", strings.Repeat("가", 300)); q.Show != LogsAll || q.Deployment != 0 || len([]rune(q.Text)) != 200 {
		t.Fatalf("unknown is the default, the text is cut at 200 characters: %+v", q)
	}
	app := LogLine{Source: LogApp, Text: "FATAL: Heap out of memory"}
	req := LogLine{Source: LogRequest, Method: "POST", Path: "/api/login", Status: 500}
	for text, want := range map[string][2]bool{"": {true, true}, "heap": {true, false}, "/API/LOGIN": {false, true}, "500": {false, true}, "post /api": {false, true}} {
		q := LogQuery{Text: text}
		if q.Matches(app) != want[0] || q.Matches(req) != want[1] {
			t.Errorf("%q: app %v req %v", text, q.Matches(app), q.Matches(req))
		}
	}
	if ContainerDeployment("naru-my-blog-12") != 12 || ContainerDeployment("shelf-blog") != 0 || ContainerDeployment("x") != 0 {
		t.Fatal("the deployment number is the suffix")
	}
	if HumanBytes(512) != "512 B" || HumanBytes(21<<30+400<<20) != "21.4 GB" {
		t.Fatal(HumanBytes(21<<30 + 400<<20))
	}
}

func TestBuildTarget(t *testing.T) {
	if b, err := ParseBuildTarget("  Web "); err != nil || b.String() != "web" {
		t.Fatalf("%q %v", b.String(), err)
	}
	if b, err := ParseBuildTarget(""); err != nil || !b.IsZero() {
		t.Fatal("empty means the last stage")
	}
	for _, bad := range []string{"-web", "1web", "web app", "web;rm", strings.Repeat("a", 64), "--target=x"} {
		if _, err := ParseBuildTarget(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestComposeValues(t *testing.T) {
	if d, err := ParseDomainTarget(" web ", "3000"); err != nil || d != (DomainTarget{Service: "web", Port: 3000}) {
		t.Fatalf("%+v %v", d, err)
	}
	if d, err := ParseDomainTarget("", "3000"); err != nil || !d.IsZero() {
		t.Fatal("no service means the service's app")
	}
	for _, bad := range [][2]string{{"web", ""}, {"web", "0"}, {"-web", "80"}, {"web app", "80"}, {"web", "70000"}} {
		if _, err := ParseDomainTarget(bad[0], bad[1]); err == nil {
			t.Errorf("%v should be refused", bad)
		}
	}
	if p, err := ParseProjectName(" shop2 "); err != nil || p.String() != "shop2" {
		t.Fatalf("%q %v", p.String(), err)
	}
	for _, bad := range []string{"Shop2", "-x", "a b", "a/b"} {
		if _, err := ParseProjectName(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	shop, _ := ParseServiceName("shop")
	if (Service{Name: shop}).Project() != "naru-shop" || (Service{Name: shop, ComposeProject: "shop2"}).Project() != "shop2" {
		t.Fatal("the project name")
	}
	if (ComposeProject{Name: "naru-shop"}).Container("web") != "naru-shop-web-1" {
		t.Fatal("compose names containers <project>-<service>-1")
	}
	// 한 번 도는 것 = 다른 서비스가 service_completed_successfully로 기다리는 것. 멈추라고 해서 0으로 끝난 DB는 아니다.
	cs := ContainerStates{
		"shop-migrate-1": {State: "exited", Status: "Exited (0) 2 minutes ago", Project: "shop", ComposeService: "migrate"},
		"shop-db-1":      {State: "exited", Status: "Exited (0) 1 minute ago", Project: "shop", ComposeService: "db"},
		"shop-seed-1":    {State: "exited", Status: "Exited (1) now", Project: "shop", ComposeService: "seed"},
		"shop-server-1": {State: "running", Status: "Up", Project: "shop", ComposeService: "server",
			DependsOn: "db:service_started:false,migrate:service_completed_successfully:false,seed:service_completed_successfully:true"},
		"other-migrate-1": {State: "exited", Status: "Exited (0) now", Project: "other", ComposeService: "migrate"},
	}
	if !cs.Finished("shop-migrate-1") || cs.Finished("shop-db-1") || cs.Finished("shop-seed-1") || cs.Finished("shop-server-1") || cs.Finished("other-migrate-1") || cs.Finished("nope") {
		t.Fatal("only a clean exit of something others wait to complete is a finished one-off")
	}
	if !IsCommitHash("a1b2c3d4e5f6") || IsCommitHash("--upload-pack=x") || IsCommitHash("main") {
		t.Fatal("commit hashes only")
	}
}

func TestContainerScreenValues(t *testing.T) {
	for in, want := range map[string]string{"Up 2 hours (unhealthy)": "unhealthy", "Up 3 days (healthy)": "healthy", "Up 5 seconds (health: starting)": "starting", "Up 1 hour": ""} {
		if got := HealthOf(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	if !EndedOK("exited", "Exited (0) 3 days ago") || EndedOK("exited", "Exited (1) 3 days ago") || EndedOK("running", "Up 1 hour") {
		t.Fatal("only a clean exit is a finished one-off")
	}
	if a, err := ParseContainerAction("restart"); err != nil || a != ActRestart {
		t.Fatal(a, err)
	}
	if _, err := ParseContainerAction("kill"); err == nil {
		t.Fatal("unknown action")
	}
	if ShortImageID("sha256:0123456789abcdef") != "0123456789ab" || ShortImageID("abc") != "abc" {
		t.Fatal("short id")
	}
	if (ContainerRow{Name: "web", Exposed: []Port{3000}, Ports: []Port{8080}}).Upstream() != "web:3000" || (ContainerRow{Name: "db"}).Upstream() != "db:" {
		t.Fatal("upstream prefers the inside port")
	}
}
