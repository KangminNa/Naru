package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

var ctx = context.Background()

func open(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(filepath.Join(dir, "naru.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func name(s string) model.ServiceName {
	n, err := model.ParseServiceName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func domain(s string) model.DomainName {
	d, err := model.ParseDomainName(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestCreateKeepsAnExplicitID(t *testing.T) {
	r := NewServices(open(t, t.TempDir()))
	id, err := r.Create(ctx, model.NewService{ID: 6, Name: name("landing"), Kind: model.KindRepo, Alias: "shelf-landing", Port: 4023})
	if err != nil || id != 6 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	next, _ := r.Create(ctx, model.NewService{Name: name("nas"), Kind: model.KindExternal, External: "host.docker.internal:5000"})
	if next != 7 {
		t.Fatalf("later services continue after the kept id, got %d", next)
	}
	if _, err := r.Create(ctx, model.NewService{Name: name("landing"), Kind: model.KindImage}); err == nil {
		t.Fatal("names are unique")
	}
	if _, err := r.Create(ctx, model.NewService{Name: name("bad"), Kind: "nonsense"}); err == nil {
		t.Fatal("unknown kinds are refused by the schema")
	}
	if !r.NameTaken(ctx, name("nas")) || r.NameTaken(ctx, name("other")) {
		t.Fatal("NameTaken")
	}
}

func TestListCarriesDomainsInOrder(t *testing.T) {
	db := open(t, t.TempDir())
	r, d := NewServices(db), NewDomains(db)
	blog, _ := r.Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindRepo, Alias: "shelf-blog", Port: 3000})
	r.Create(ctx, model.NewService{Name: name("api"), Kind: model.KindImage, Alias: "naru-api", Port: 8080})
	if err := d.Add(ctx, blog, model.DomainInput{Domain: domain("Blog.Example.com"), HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	d.Add(ctx, blog, model.DomainInput{Domain: domain("www.blog.example.com"), HTTPS: true})
	if err := d.Add(ctx, blog, model.DomainInput{Domain: domain("blog.example.com")}); err == nil {
		t.Fatal("a domain belongs to one service")
	}

	all, err := r.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("%v %v", all, err)
	}
	if all[0].Name.String() != "api" || len(all[0].Domains) != 0 {
		t.Fatal("sorted by name; api has no domains")
	}
	b := all[1]
	if b.PrimaryDomain() != "blog.example.com" || len(b.Domains) != 2 || b.Live.Alias != "shelf-blog" || b.Port != 3000 {
		t.Fatalf("%+v", b)
	}
	if owner, ok := d.FindOwner(ctx, domain("BLOG.example.com")); !ok || owner != blog {
		t.Fatal("FindOwner matches the normalized name")
	}
	if err := d.Remove(ctx, 999, b.Domains[0].ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("a domain is removed only through its own service")
	}
	if err := d.Remove(ctx, blog, b.Domains[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestGetUnknown(t *testing.T) {
	if _, err := NewServices(open(t, t.TempDir())).Get(ctx, 42); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestSecretsAndLiveStateStaySeparate(t *testing.T) {
	db := open(t, t.TempDir())
	r := NewServices(db)
	id, _ := r.Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindRepo, Alias: "naru-blog", Port: 3000})
	env, _, _ := model.ParseEnvVars("A=1\nB=2")
	envFile, _ := model.ParseEnvFile("NEXT_PUBLIC_X=\"y\"")
	if err := NewSecrets(db).Set(ctx, id, model.ServiceSecrets{Env: env, EnvFile: envFile, GitToken: "ghp_x", WebhookSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := NewLiveStates(db).Save(ctx, id, model.LiveState{Alias: "naru-blog", Instance: "naru-blog-4"}); err != nil {
		t.Fatal(err)
	}
	sec, _ := NewSecrets(db).Get(ctx, id)
	if sec.GitToken != "ghp_x" || len(sec.Env.List()) != 2 || sec.EnvFile.Text() != "NEXT_PUBLIC_X=\"y\"\n" {
		t.Fatalf("%+v", sec)
	}
	s, _ := r.Get(ctx, id)
	if s.Live.Instance != "naru-blog-4" || s.Port != 3000 {
		t.Fatalf("a zero port in the live state keeps the saved port: %+v", s.Live)
	}
	if s.Live.LiveDeployment() != 4 {
		t.Fatal("the live deployment comes from the instance name")
	}
	NewHookLogs(db).Save(ctx, id, model.HookLog{At: time.Unix(1790000000, 0), Result: "queued"})
	s, _ = r.Get(ctx, id)
	if s.HookLog.Result != "queued" {
		t.Fatalf("%+v", s.HookLog)
	}
}

func TestDeploymentHistory(t *testing.T) {
	db := open(t, t.TempDir())
	id, _ := NewServices(db).Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindRepo})
	h := NewDeployments(db)
	first, _ := h.Start(ctx, id, model.ReasonManual)
	h.Finish(ctx, model.Deployment{ID: first, Status: model.DeploySuccess, Commit: "abc", Image: "sha256:1", Log: "ok"})
	second, _ := h.Start(ctx, id, model.ReasonPush)
	h.SaveLog(ctx, second, "building")
	if n, _ := h.CloseInterrupted(ctx, "naru restarted"); n != 1 {
		t.Fatalf("closed %d", n)
	}
	got, _ := h.Get(ctx, second)
	if got.Status != model.DeployFailed || got.Log != "building\nnaru restarted\n" || got.Reason != model.ReasonPush {
		t.Fatalf("%+v", got)
	}
	recent, _ := h.Recent(ctx, id, 10)
	if len(recent) != 2 || recent[0].ID != second || recent[0].Log != "" {
		t.Fatalf("recent is newest first and without logs: %+v", recent)
	}
	ok, _ := h.Succeeded(ctx, id)
	if len(ok) != 1 || ok[0] != first {
		t.Fatalf("%v", ok)
	}
	if _, err := h.Get(ctx, 99); !errors.Is(err, model.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAccountsAndSessions(t *testing.T) {
	db := open(t, t.TempDir())
	a, s := NewAccounts(db), NewSessions(db)
	u, _ := model.ParseUsername("kangmin")
	id, err := a.CreateFirst(ctx, u, "salt:hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFirst(ctx, u, "x"); !errors.Is(err, model.ErrSetupClosed) {
		t.Fatal("only the first account is created this way")
	}
	now := time.Unix(1790000000, 0)
	s.Save(ctx, "d1", id, now.Add(time.Hour))
	s.Save(ctx, "d2", id, now.Add(time.Hour))
	if who, ok := s.FindOwner(ctx, "d1", now); !ok || who.Username.String() != "kangmin" {
		t.Fatal("session owner")
	}
	if _, ok := s.FindOwner(ctx, "d1", now.Add(2*time.Hour)); ok {
		t.Fatal("expired sessions have no owner")
	}
	s.DeleteOthers(ctx, id, "d2")
	if _, ok := s.FindOwner(ctx, "d1", now); ok {
		t.Fatal("other sessions are gone")
	}
	if _, ok := s.FindOwner(ctx, "d2", now); !ok {
		t.Fatal("the kept session stays")
	}
	a.DeleteAll(ctx)
	if _, ok := s.FindOwner(ctx, "d2", now); ok {
		t.Fatal("sessions go with their account")
	}
}

// ── v1 ────────────────────────────────────

const v1Hash = "0123456789abcdef0123456789abcdef:6822e8648a09c289b3fb964753d46cc34c9b8fac18f3bd52e8513551b035ceb65985818b05aa03f43c1dd0e6d7b5cf496e9ba336bc9b599c4e76b57f03df2ba0"

func writeV1(t *testing.T, dir, file string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
}

func writeV1Accounts(t *testing.T, dir string) {
	writeV1(t, dir, "auth.db",
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at INTEGER)`,
		`INSERT INTO users (username, password_hash) VALUES ('kangmin', '`+v1Hash+`')`,
	)
	writeV1(t, dir, "proxy.db",
		`CREATE TABLE proxy_hosts (id INTEGER PRIMARY KEY AUTOINCREMENT, domain TEXT NOT NULL UNIQUE, target_scheme TEXT NOT NULL DEFAULT 'http', target_host TEXT, target_port INTEGER,
			ssl_enabled INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1, description TEXT DEFAULT '')`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, description) VALUES ('shelf.example.com', '127.0.0.1', 81, 'Shelf admin (auto-registered via ADMIN_DOMAIN)')`,
	)
}

func TestImportCarriesAccountsAndAdminDomain(t *testing.T) {
	dir := t.TempDir()
	writeV1Accounts(t, dir)
	db := open(t, dir)

	r, err := ImportV1(ctx, db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Found || r.Users != 1 || r.AdminDomain != "shelf.example.com" {
		t.Fatalf("unexpected report %+v", r)
	}
	if _, h, err := NewAccounts(db).FindByName(ctx, "kangmin"); err != nil || h != v1Hash {
		t.Fatal("the v1 password hash is kept as is")
	}
	if v, _ := NewSettings(db).Get(ctx, model.SettingSetupDone); v != "1" {
		t.Fatal("a server that ran v1 skips the setup wizard")
	}
	again, err := ImportV1(ctx, db, dir)
	if err != nil || again.Users != 0 || again.Found {
		t.Fatalf("the import runs once: %+v %v", again, err)
	}
}

func TestImportLeavesV1Untouched(t *testing.T) {
	dir := t.TempDir()
	writeV1Accounts(t, dir)
	if _, err := ImportV1(ctx, open(t, dir), dir); err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite", filepath.Join(dir, "auth.db"))
	defer db.Close()
	var tables int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables)
	if tables != 1 {
		t.Fatalf("v1 auth.db must not gain tables, has %d", tables)
	}
}

func TestFreshInstallImportsNothing(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	r, err := ImportV1(ctx, db, dir)
	if err != nil || r.Found || r.Users != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if n, _ := NewAccounts(db).Count(ctx); n != 0 {
		t.Fatal("a fresh install still needs setup")
	}
	if _, set := NewSettings(db).Get(ctx, model.SettingSetupDone); set {
		t.Fatal("setup is not marked done without accounts")
	}
}

func TestExistingAdminDomainWins(t *testing.T) {
	dir := t.TempDir()
	writeV1Accounts(t, dir)
	db := open(t, dir)
	NewSettings(db).Set(ctx, model.SettingAdminDomain, "naru.example.com")
	if _, err := ImportV1(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	if v, _ := NewSettings(db).Get(ctx, model.SettingAdminDomain); v != "naru.example.com" {
		t.Fatalf("an admin domain already set must win, got %q", v)
	}
}

const v1Projects = `CREATE TABLE projects (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, repo_url TEXT NOT NULL,
	branch TEXT NOT NULL DEFAULT 'main', port INTEGER, env TEXT DEFAULT '', domain TEXT DEFAULT '', webhook_secret TEXT NOT NULL,
	auto_deploy INTEGER NOT NULL DEFAULT 1, source_type TEXT NOT NULL DEFAULT 'git', image TEXT DEFAULT '', container_port INTEGER,
	volumes TEXT DEFAULT '', git_token TEXT DEFAULT '', build_path TEXT NOT NULL DEFAULT '')`

const v1Hosts = `CREATE TABLE proxy_hosts (id INTEGER PRIMARY KEY AUTOINCREMENT, domain TEXT NOT NULL UNIQUE, target_scheme TEXT NOT NULL DEFAULT 'http',
	target_host TEXT NOT NULL, target_port INTEGER NOT NULL, ssl_enabled INTEGER NOT NULL DEFAULT 0, force_ssl INTEGER NOT NULL DEFAULT 0,
	enabled INTEGER NOT NULL DEFAULT 1, description TEXT DEFAULT '', hsts_enabled INTEGER NOT NULL DEFAULT 0)`

func TestAppsAndHostsBecomeServices(t *testing.T) {
	dir := t.TempDir()
	writeV1(t, dir, "deploy.db", v1Projects,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, container_port, build_path, env, git_token) VALUES (3, 'blog', 'https://github.com/me/blog', 'whsec', 3000, '', 'A=1', 'ghp_x')`,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, source_type, image, container_port, auto_deploy) VALUES (6, 'api', '', 'whsec2', 'image', 'ghcr.io/me/api:latest', 8080, 0)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL, commit_hash TEXT DEFAULT '', commit_message TEXT DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending', trigger_type TEXT NOT NULL DEFAULT 'manual', log TEXT DEFAULT '', duration_ms INTEGER DEFAULT 0, created_at INTEGER)`,
		`INSERT INTO deployments (project_id, commit_hash, commit_message, status, trigger_type, log, duration_ms, created_at) VALUES (3, 'abc1234', 'first', 'success', 'webhook', 'built', 38000, 1790000000)`,
		`INSERT INTO deployments (project_id, status, created_at) VALUES (3, 'failed', 1790000100)`,
		`INSERT INTO deployments (project_id, status, created_at) VALUES (99, 'success', 1790000200)`,
	)
	writeV1(t, dir, "proxy.db", v1Hosts,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, ssl_enabled, hsts_enabled, description) VALUES ('blog.example.com', 'shelf-blog', 3000, 1, 1, 'Auto-created by app blog')`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, description) VALUES ('nas.example.com', '127.0.0.1', 5000, 'NAS')`,
		`INSERT INTO proxy_hosts (domain, target_scheme, target_host, target_port, description) VALUES ('router.example.com', 'https', '192.168.0.1', 443, '')`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, enabled) VALUES ('off.example.com', '10.0.0.9', 80, 0)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, description) VALUES ('shelf.example.com', '127.0.0.1', 81, 'Shelf admin (auto-registered via ADMIN_DOMAIN)')`,
	)
	db := open(t, dir)
	r, err := ImportV1(ctx, db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Apps != 2 || r.External != 2 || r.Domains != 3 || r.History != 2 {
		t.Fatalf("%+v", r)
	}

	repo := NewServices(db)
	blog, err := repo.Get(ctx, 3)
	if err != nil {
		t.Fatal("app ids are kept so webhook URLs survive:", err)
	}
	if blog.Kind != model.KindRepo || blog.Live.Alias != "shelf-blog" || blog.Port != 3000 || blog.Branch != "main" {
		t.Fatalf("%+v", blog)
	}
	if len(blog.Domains) != 1 || !blog.Domains[0].HTTPS || !blog.Domains[0].HSTS {
		t.Fatalf("the app's own host becomes its domain: %+v", blog.Domains)
	}
	if sec, _ := NewSecrets(db).Get(ctx, 3); sec.GitToken != "ghp_x" || sec.WebhookSecret != "whsec" || sec.Env.Text() != "A=1" {
		t.Fatalf("secrets come along: %+v", sec)
	}

	api, _ := repo.Get(ctx, 6)
	if api.Kind != model.KindImage || api.Source != "ghcr.io/me/api:latest" || api.AutoDeploy || api.Branch != "" {
		t.Fatalf("%+v", api)
	}

	all, _ := repo.List(ctx)
	external := map[string]string{}
	for _, s := range all {
		external[s.PrimaryDomain()] = s.External
	}
	if external["nas.example.com"] != "host.docker.internal:5000" {
		t.Fatalf("v1's 127.0.0.1 meant this server: %q", external["nas.example.com"])
	}
	if external["router.example.com"] != "https://192.168.0.1:443" {
		t.Fatalf("https upstreams keep their scheme: %q", external["router.example.com"])
	}
	if _, ok := external["shelf.example.com"]; ok {
		t.Fatal("the admin host is a setting, not a service")
	}
	if _, ok := external["off.example.com"]; ok {
		t.Fatal("disabled hosts are not imported")
	}
	if again, _ := ImportV1(ctx, db, dir); again.Apps != 0 {
		t.Fatal("runs once")
	}
}

// 실서버 그림자 실행에서 찾은 두 가지 (2026-10-10):
//   - 초기 v1이 만든 앱은 포트가 예전 칸(port)에만 있고 container_port는 비어 있다.
//   - v1은 호스트의 "인증서 사용"과 상관없이, 그 주소를 덮는 인증서가 있으면 HTTPS로 서빙했다 (SNI).
func TestImportMatchesWhatV1ActuallyServed(t *testing.T) {
	dir := t.TempDir()
	writeV1(t, dir, "deploy.db", v1Projects,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, source_type, image, port, auto_deploy) VALUES (3, 'potainer', '', 's', 'image', 'portainer/portainer-ce:latest', 9000, 1)`,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, source_type, image, auto_deploy) VALUES (4, 'noport', '', 's', 'image', 'me/app', 1)`,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, container_port) VALUES (6, 'site', 'https://github.com/me/site', 's', 4023)`,
	)
	writeV1(t, dir, "proxy.db", v1Hosts,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, ssl_enabled) VALUES ('potainer.example.com', 'shelf-potainer', 9000, 1)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, ssl_enabled) VALUES ('site.example.com', 'shelf-site', 4023, 0)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, ssl_enabled) VALUES ('app.apps.example.com', 'shelf-noport', 8080, 0)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, ssl_enabled) VALUES ('plain.example.com', '10.0.0.7', 80, 0)`,
		`CREATE TABLE ssl_certs (id INTEGER PRIMARY KEY AUTOINCREMENT, domain TEXT NOT NULL UNIQUE, cert_path TEXT, key_path TEXT, provider TEXT DEFAULT 'manual',
			expires_at INTEGER, auto_renew INTEGER NOT NULL DEFAULT 0, domains TEXT DEFAULT '', dns_provider TEXT DEFAULT '', dns_token TEXT DEFAULT '')`,
		`INSERT INTO ssl_certs (domain, provider) VALUES ('site.example.com', 'letsencrypt')`,
		`INSERT INTO ssl_certs (domain, provider, domains) VALUES ('apps.example.com', 'letsencrypt', 'apps.example.com
*.apps.example.com')`,
	)
	db := open(t, dir)
	if _, err := ImportV1(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	repo := NewServices(db)
	if p, _ := repo.Get(ctx, 3); p.Port != 9000 {
		t.Fatalf("the old port column is used when container_port is empty: %d", p.Port)
	}
	if p, _ := repo.Get(ctx, 4); p.Port != 8080 {
		t.Fatalf("with no port at all, the port v1 proxied to is used: %d", p.Port)
	}
	if p, _ := repo.Get(ctx, 6); p.Port != 4023 {
		t.Fatalf("%d", p.Port)
	}
	https := map[string]bool{}
	all, _ := repo.List(ctx)
	for _, s := range all {
		for _, d := range s.Domains {
			https[d.Domain.String()] = d.HTTPS
		}
	}
	for d, want := range map[string]bool{"potainer.example.com": true, "site.example.com": true, "app.apps.example.com": true, "plain.example.com": false} {
		if https[d] != want {
			t.Errorf("%s: HTTPS %v, want %v (v1 served HTTPS wherever a certificate covered the name)", d, https[d], want)
		}
	}
}

// 옮기지 못한 것은 조용히 버리지 않고 이유와 함께 알린다 — 실서버를 넘길 때 무엇이 빠졌는지 보여야 한다.
func TestImportSaysWhatItLeftBehind(t *testing.T) {
	dir := t.TempDir()
	writeV1(t, dir, "deploy.db", v1Projects,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, container_port) VALUES (3, 'blog', 'https://github.com/me/blog', 's', 3000)`,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, container_port) VALUES (4, 'Bad Name!', 'https://github.com/me/x', 's', 80)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL, commit_hash TEXT DEFAULT '', commit_message TEXT DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending', trigger_type TEXT NOT NULL DEFAULT 'manual', log TEXT DEFAULT '', duration_ms INTEGER DEFAULT 0, created_at INTEGER)`,
		`INSERT INTO deployments (project_id, status, created_at) VALUES (3, 'success', 1790000000)`,
		`INSERT INTO deployments (project_id, status, created_at) VALUES (99, 'success', 1790000200)`,
		`INSERT INTO deployments (project_id, status, created_at) VALUES (98, 'failed', 1790000300)`,
	)
	writeV1(t, dir, "proxy.db", v1Hosts,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, ssl_enabled) VALUES ('blog.example.com', 'shelf-blog', 3000, 1)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port) VALUES ('Blog.Example.com', '10.0.0.5', 80)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port, enabled) VALUES ('off.example.com', '10.0.0.9', 80, 0)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port) VALUES ('bad_domain', '10.0.0.9', 80)`,
		`INSERT INTO proxy_hosts (domain, target_host, target_port) VALUES ('nas.example.com', 'bad host', 5000)`,
		`CREATE TABLE ssl_certs (id INTEGER PRIMARY KEY AUTOINCREMENT, domain TEXT NOT NULL UNIQUE, cert_path TEXT, key_path TEXT, provider TEXT DEFAULT 'manual',
			expires_at INTEGER, auto_renew INTEGER NOT NULL DEFAULT 0, domains TEXT DEFAULT '', dns_provider TEXT DEFAULT '', dns_token TEXT DEFAULT '')`,
		`INSERT INTO ssl_certs (domain, provider) VALUES ('blog.example.com', 'letsencrypt')`,
		`INSERT INTO ssl_certs (domain, provider) VALUES ('files.example.com', 'manual')`,
		`INSERT INTO ssl_certs (domain, provider, domains, dns_provider, dns_token) VALUES ('example.com', 'letsencrypt', 'example.com
*.example.com', 'cloudflare', 'cf_secret_token')`,
		`INSERT INTO ssl_certs (domain, provider) VALUES ('*.local.test', 'selfsigned')`,
	)
	writeV1(t, dir, "notify.db",
		`CREATE TABLE channels (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL DEFAULT 'webhook', url TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, description TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO channels (url, description) VALUES ('ftp://secret-host/hook-token', 'pager')`,
	)
	r, err := ImportV1(ctx, open(t, dir), dir)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(r.Skipped, "\n")
	for _, want := range []string{
		"앱 Bad Name!: 이름 모양이 틀림",
		"배포 기록 2개: 지워진 앱의 것",
		"주소 off.example.com: v1에서 꺼져 있음",
		"주소 bad_domain: 주소 모양이 틀림",
		"주소 blog.example.com: 이미 다른 서비스에 있음",
		"주소 nas.example.com: 연결 대상 bad host:5000 모양이 틀림",
		"인증서 files.example.com: 직접 올린 인증서",
		"인증서 example.com, *.example.com: DNS-01(cloudflare)",
		"인증서 *.local.test: 자체 서명",
		"알림 주소 pager: 주소 모양이 틀림",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "cf_secret_token") || strings.Contains(all, "hook-token") || strings.Contains(all, "secret-host") {
		t.Fatal("secrets (DNS tokens, alert addresses) never appear in the report")
	}
	if strings.Contains(all, "인증서 blog.example.com") {
		t.Fatal("ordinary Let's Encrypt certificates are simply issued again — nothing to say")
	}
}

// 아주 오래된 v1(빌드 경로·이미지 칸이 생기기 전)도 읽는다.
func TestOldV1SchemaStillImports(t *testing.T) {
	dir := t.TempDir()
	writeV1(t, dir, "deploy.db",
		`CREATE TABLE projects (id INTEGER PRIMARY KEY, name TEXT, repo_url TEXT, branch TEXT, port INTEGER, webhook_secret TEXT, auto_deploy INTEGER)`,
		`INSERT INTO projects VALUES (1, 'old', 'https://github.com/me/old', 'main', 4000, 's', 1)`,
	)
	db := open(t, dir)
	if r, err := ImportV1(ctx, db, dir); err != nil || r.Apps != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	old, _ := NewServices(db).Get(ctx, 1)
	if old.Port != 4000 || old.Kind != model.KindRepo {
		t.Fatalf("%+v", old)
	}
}

func TestNameCollisionsGetASuffix(t *testing.T) {
	dir := t.TempDir()
	writeV1(t, dir, "deploy.db", v1Projects,
		`INSERT INTO projects (id, name, repo_url, webhook_secret, container_port) VALUES (1, 'nas', 'https://github.com/me/nas', 's', 80)`)
	writeV1(t, dir, "proxy.db", v1Hosts,
		`INSERT INTO proxy_hosts (domain, target_host, target_port) VALUES ('nas.example.com', '192.168.0.20', 5000)`)
	db := open(t, dir)
	if _, err := ImportV1(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	all, _ := NewServices(db).List(ctx)
	if len(all) != 2 || all[1].Name.String() != "nas-2" {
		t.Fatalf("%+v", all)
	}
}

func TestWebSettingsStorage(t *testing.T) {
	db := open(t, t.TempDir())
	id, _ := NewServices(db).Create(ctx, model.NewService{Name: name("shop"), Kind: model.KindImage})
	w := NewWebSettings(db)
	if got, err := w.Get(ctx, id); err != nil || got.Maintenance || len(got.Headers) != 0 {
		t.Fatalf("no settings yet is the empty setting: %+v %v", got, err)
	}
	prefix, _ := model.ParsePathPrefix("/api")
	in := model.WebSettings{Maintenance: true, Headers: []model.HeaderRule{{Name: "X", Value: "1"}},
		Login: model.BasicLogin{User: "me", Hash: "$2a$10$abc"}, Paths: []model.PathRoute{{Prefix: prefix, Service: 2}}}
	if err := w.Set(ctx, id, in); err != nil {
		t.Fatal(err)
	}
	in.Maintenance = false
	w.Set(ctx, id, in)
	got, _ := w.Get(ctx, id)
	if got.Maintenance || got.Login.Hash != "$2a$10$abc" || got.Paths[0].Prefix.String() != "/api" {
		t.Fatalf("%+v", got)
	}
	NewServices(db).Delete(ctx, id)
	if got, _ := w.Get(ctx, id); got.Login.User != "" {
		t.Fatal("settings go with their service")
	}
}

func TestAlertChannelsAndDeliveries(t *testing.T) {
	db := open(t, t.TempDir())
	ch := NewChannels(db)
	id, err := ch.Add(ctx, model.AlertChannel{Name: "team", URL: "https://discord.com/api/webhooks/1/x", Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	all, _ := ch.List(ctx)
	if len(all) != 1 || all[0].ID != id || all[0].Secret != "s" {
		t.Fatalf("%+v", all)
	}
	dl := NewDeliveries(db)
	for i := 0; i < 205; i++ {
		dl.Save(ctx, model.Delivery{Channel: id, ChannelName: "team", Event: "service.down", Title: "t", OK: i%2 == 0, At: time.Unix(int64(1790000000+i), 0)})
	}
	recent, _ := dl.Recent(ctx, 300)
	if len(recent) != 200 || recent[0].At.Unix() != 1790000204 {
		t.Fatalf("only the latest 200, newest first: %d %v", len(recent), recent[0].At)
	}
	ch.Remove(ctx, id)
	if all, _ := ch.List(ctx); len(all) != 0 {
		t.Fatal("removed")
	}
	if recent, _ := dl.Recent(ctx, 10); len(recent) != 10 || recent[0].ChannelName != "team" {
		t.Fatal("results stay readable after the channel is gone")
	}
}

func TestV1AlertChannelsComeAlong(t *testing.T) {
	dir := t.TempDir()
	writeV1(t, dir, "notify.db",
		`CREATE TABLE channels (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL DEFAULT 'webhook', url TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, description TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO channels (url, secret, description) VALUES ('https://discord.com/api/webhooks/1/x', '', 'team chat')`,
		`INSERT INTO channels (url, secret, enabled) VALUES ('https://ops.example.com/hook', 'sig', 1)`,
		`INSERT INTO channels (url, enabled) VALUES ('https://old.example.com/off', 0)`,
		`INSERT INTO channels (url) VALUES ('not a url')`,
	)
	db := open(t, dir)
	r, err := ImportV1(ctx, db, dir)
	if err != nil || r.Channels != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	all, _ := NewChannels(db).List(ctx)
	if len(all) != 2 || all[0].Name != "team chat" || all[1].Name != "ops.example.com" || all[1].Secret != "sig" {
		t.Fatalf("enabled, valid channels come along: %+v", all)
	}
	if again, _ := ImportV1(ctx, db, dir); again.Channels != 0 {
		t.Fatal("once")
	}
}

// 넘겨받기: 주소와 웹서버 설정이 한 번에 옮겨 가고, 표시가 지워진다.
func TestMovingDomainsAndWebSettingsToAnotherService(t *testing.T) {
	db := open(t, t.TempDir())
	r, domains, web := NewServices(db), NewDomains(db), NewWebSettings(db)
	old, _ := r.Create(ctx, model.NewService{Name: name("cairn"), Kind: model.KindExternal, External: "cairn-web-1:3000"})
	fresh, _ := r.Create(ctx, model.NewService{Name: name("cairn-2"), Kind: model.KindRepo, Source: "https://github.com/me/cairn", TakesOver: old})
	other, _ := r.Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindImage, Source: "me/blog"})
	add := func(id model.ServiceID, d string) {
		dn, _ := model.ParseDomainName(d)
		if err := domains.Add(ctx, id, model.DomainInput{Domain: dn, HTTPS: true}); err != nil {
			t.Fatal(err)
		}
	}
	add(old, "cairn.example.com")
	add(old, "www.cairn.example.com")
	add(other, "blog.example.com")
	web.Set(ctx, old, model.WebSettings{Maintenance: true, Advanced: "encode gzip"})
	web.Set(ctx, fresh, model.WebSettings{Advanced: "to be replaced"})
	if s, _ := r.Get(ctx, fresh); s.TakesOver != old {
		t.Fatalf("the mark is stored: %+v", s.TakesOver)
	}

	n, err := NewDomainMoves(db).Move(ctx, old, fresh)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	a, b, c := must(r.Get(ctx, old)), must(r.Get(ctx, fresh)), must(r.Get(ctx, other))
	if len(a.Domains) != 0 || len(b.Domains) != 2 || !b.Domains[0].HTTPS || len(c.Domains) != 1 || b.TakesOver != 0 {
		t.Fatalf("old=%v new=%v other=%v mark=%d", a.Domains, b.Domains, c.Domains, b.TakesOver)
	}
	if ws, _ := web.Get(ctx, fresh); !ws.Maintenance || ws.Advanced != "encode gzip" {
		t.Fatalf("the web settings came along: %+v", ws)
	}
	if ws, _ := web.Get(ctx, old); ws.Maintenance || ws.Advanced != "" {
		t.Fatalf("and left the old service: %+v", ws)
	}

	// 옛 서비스가 이미 없어도(지웠어도) 표시는 지워진다. 옛 것에 설정이 없으면 새 것의 설정을 건드리지 않는다
	again, _ := r.Create(ctx, model.NewService{Name: name("again"), Kind: model.KindRepo, Source: "https://github.com/me/x", TakesOver: 999})
	web.Set(ctx, again, model.WebSettings{Advanced: "kept"})
	if n, err := NewDomainMoves(db).Move(ctx, 999, again); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if s := must(r.Get(ctx, again)); s.TakesOver != 0 {
		t.Fatal("the mark is cleared even when there was nothing to move")
	}
	if ws, _ := web.Get(ctx, again); ws.Advanced != "kept" {
		t.Fatalf("%+v", ws)
	}
}

func must(s model.Service, err error) model.Service {
	if err != nil {
		panic(err)
	}
	return s
}

func TestMetricsAreKeptByTheMinuteThenByTheHour(t *testing.T) {
	db := open(t, t.TempDir())
	m := NewMetrics(db)
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 120; i++ { // 10시부터 2시간, 1분마다
		at := start.Add(time.Duration(i) * time.Minute)
		cpu := 10.0
		if i == 30 {
			cpu = 90
		}
		err := m.Add(ctx, at, map[model.ServiceID]model.MetricPoint{
			0: {CPU: cpu, MemUsed: 4 << 30, MemTotal: 8 << 30, DiskUsed: 50 << 30, DiskTotal: 100 << 30, NetIn: uint64(i) * 1000, NetOut: uint64(i) * 9000},
			7: {CPU: 1, MemUsed: uint64(100+i) << 20, MemTotal: 8 << 30},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.Range(ctx, start, start.Add(2*time.Hour))
	if err != nil || len(got[0]) != 120 || len(got[7]) != 120 || got[0][30].CPU != 90 || got[0][119].NetOut != 119*9000 {
		t.Fatalf("%d %d %v", len(got[0]), len(got[7]), err)
	}

	// 2일 뒤: 끝난 시간들은 1시간 값으로 묶인다 (평균·최고, 누적 바이트는 마지막)
	if err := m.Compact(ctx, start.Add(50*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Range(ctx, start, start.Add(2*time.Hour))
	if len(got[0]) != 2 || len(got[7]) != 2 {
		t.Fatalf("one point per hour: %d %d", len(got[0]), len(got[7]))
	}
	h := got[0][0]
	if !h.At.Equal(start) || h.CPU < 11.3 || h.CPU > 11.4 || h.CPUMax != 90 || h.MemTotal != 8<<30 || h.DiskUsed != 50<<30 || h.NetOut != 59*9000 {
		t.Fatalf("average, peak and the last counter: %+v", h)
	}
	if svc := got[7][1]; svc.MemMax != 219<<20 || svc.MemUsed != 189<<20+512<<10 {
		t.Fatalf("%+v", svc)
	}
	if err := m.Compact(ctx, start.Add(51*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.Range(ctx, start, start.Add(2*time.Hour)); len(again[0]) != 2 {
		t.Fatal("an hour is never compacted twice")
	}
	// 30일 뒤에는 남지 않는다
	m.Compact(ctx, start.Add(31*24*time.Hour))
	if gone, _ := m.Range(ctx, start, start.Add(2*time.Hour)); len(gone) != 0 {
		t.Fatalf("%v", gone)
	}
}

func TestAuditKeepsWhatHappened(t *testing.T) {
	db := open(t, t.TempDir())
	a := NewAudit(db)
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	a.Record(ctx, model.AuditEntry{At: at, Who: "lab", IP: "203.0.113.9", Action: "login", OK: true})
	a.Record(ctx, model.AuditEntry{At: at.Add(time.Minute), Who: strings.Repeat("x", 300), IP: "198.51.100.7", Action: "login", OK: false})
	a.Record(ctx, model.AuditEntry{At: at.Add(2 * time.Minute), System: true, Action: "service.down", Target: "blog", Service: 4, OK: false, Detail: "crashed"})
	got, err := a.Recent(ctx, at, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	if !got[0].System || got[0].Service != 4 || got[0].Detail != "crashed" || got[0].OK || got[2].Who != "lab" || !got[2].OK {
		t.Fatalf("newest first: %+v", got)
	}
	if len(got[1].Who) != 64 {
		t.Fatalf("what a stranger typed is cut: %d", len(got[1].Who))
	}
	if few, _ := a.Recent(ctx, at.Add(90*time.Second), 10); len(few) != 1 {
		t.Fatal("since")
	}
	a.Prune(ctx, at.Add(90*time.Second))
	if left, _ := a.Recent(ctx, at, 10); len(left) != 1 {
		t.Fatal("pruned")
	}
	if cut("가나다", 4) != "가" {
		t.Fatal("never cut in the middle of a character")
	}
}

func TestDomainTargetsAndComposeFields(t *testing.T) {
	db := open(t, t.TempDir())
	r, domains := NewServices(db), NewDomains(db)
	id, _ := r.Create(ctx, model.NewService{Name: name("shop"), Kind: model.KindCompose, Source: "https://github.com/me/shop", ComposeFile: "deploy/compose.yml", ComposeProject: "shop2"})
	dn, _ := model.ParseDomainName("shop.example.com")
	if err := domains.Add(ctx, id, model.DomainInput{Domain: dn, HTTPS: true, Target: model.DomainTarget{Service: "web", Port: 3000}}); err != nil {
		t.Fatal(err)
	}
	s := must(r.Get(ctx, id))
	if s.ComposeFile != "deploy/compose.yml" || s.Project() != "shop2" || s.Domains[0].Target != (model.DomainTarget{Service: "web", Port: 3000}) {
		t.Fatalf("%+v", s)
	}
	if err := domains.SetTarget(ctx, id, s.Domains[0].ID, model.DomainTarget{Service: "admin", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if s := must(r.Get(ctx, id)); s.Domains[0].Target.Service != "admin" || s.Domains[0].Target.Port != 8080 {
		t.Fatalf("%+v", s.Domains[0])
	}
	if err := domains.SetTarget(ctx, id+1, s.Domains[0].ID, model.DomainTarget{}); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("only its own addresses")
	}
	r.Update(ctx, id, model.ServiceSettings{Source: "https://github.com/me/shop", ComposeFile: "compose.yml"})
	if s := must(r.Get(ctx, id)); s.ComposeFile != "compose.yml" || s.ComposeProject != "shop2" {
		t.Fatal("the project name never changes with the settings")
	}
}

// 종류에 compose를 더하는 마이그레이션은 services 표를 다시 만든다 — 주소·배포 기록·웹서버 설정과 번호가 그대로여야 한다.
func TestRebuildingServicesKeepsEverythingAttached(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	r := NewServices(db)
	r.Create(ctx, model.NewService{Name: name("first"), Kind: model.KindImage, Source: "me/first"})
	r.Delete(ctx, 1)
	id, _ := r.Create(ctx, model.NewService{Name: name("blog"), Kind: model.KindRepo, Source: "https://github.com/me/blog"})
	dn, _ := model.ParseDomainName("blog.example.com")
	NewDomains(db).Add(ctx, id, model.DomainInput{Domain: dn, HTTPS: true})
	did, _ := NewDeployments(db).Start(ctx, id, model.ReasonManual)
	NewWebSettings(db).Set(ctx, id, model.WebSettings{Maintenance: true})
	NewSecrets(db).Set(ctx, id, model.ServiceSecrets{GitToken: "ghp_x", WebhookSecret: "s"})
	// 다시 만들기를 한 번 더 돌린다 (처음 열 때 이미 돌았다)
	if _, err := db.sql.Exec(`DELETE FROM schema_migrations WHERE version = 'migrations/014_kind_compose.sql'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = open(t, dir)
	r = NewServices(db)
	s := must(r.Get(ctx, id))
	if s.ID != 2 || len(s.Domains) != 1 || s.Source != "https://github.com/me/blog" {
		t.Fatalf("the same number and addresses: %+v", s)
	}
	if d, err := NewDeployments(db).Get(ctx, did); err != nil || d.ServiceID != id {
		t.Fatalf("deploy history stays: %v", err)
	}
	if ws, _ := NewWebSettings(db).Get(ctx, id); !ws.Maintenance {
		t.Fatal("web settings stay")
	}
	if sec, _ := NewSecrets(db).Get(ctx, id); sec.GitToken != "ghp_x" {
		t.Fatal("secrets stay")
	}
	next, err := r.Create(ctx, model.NewService{Name: name("shop"), Kind: model.KindCompose, Source: "https://github.com/me/shop"})
	if err != nil || next != 3 {
		t.Fatalf("compose is a kind now, numbers keep counting up: %d %v", next, err)
	}
	// 외래 키가 다시 켜져 있다 — 지우면 주소가 함께 지워진다
	r.Delete(ctx, id)
	if owner, ok := NewDomains(db).FindOwner(ctx, dn); ok {
		t.Fatalf("cascade is back on: %d", owner)
	}
}
