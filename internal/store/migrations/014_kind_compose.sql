-- naru:rebuild — 외래 키를 끈 연결에서 돌린다 (SQLite의 테이블 다시 만들기 순서). 끄지 않으면 services를 지울 때
-- domains·deployments·web_settings가 함께 지워진다(ON DELETE CASCADE). 다 옮긴 뒤 foreign_key_check로 확인하고 커밋한다.
-- 종류에 compose를 더한다 (2026-11-25). 처음 표의 CHECK는 고칠 수 없어 표를 다시 만든다. 번호(id)는 그대로 — 웹훅 주소가 번호다.
CREATE TABLE services_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('repo', 'compose', 'image', 'static', 'external')),
  source TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT 'main',
  build_path TEXT NOT NULL DEFAULT '',
  container TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  upstream TEXT NOT NULL DEFAULT '',
  auto_deploy INTEGER NOT NULL DEFAULT 1,
  env TEXT NOT NULL DEFAULT '',
  volumes TEXT NOT NULL DEFAULT '',
  git_token TEXT NOT NULL DEFAULT '',
  webhook_secret TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT (unixepoch()),
  folder TEXT NOT NULL DEFAULT '',
  release TEXT NOT NULL DEFAULT '',
  instance TEXT NOT NULL DEFAULT '',
  stopped INTEGER NOT NULL DEFAULT 0,
  hook_at INTEGER NOT NULL DEFAULT 0,
  hook_result TEXT NOT NULL DEFAULT '',
  env_file TEXT NOT NULL DEFAULT '',
  networks TEXT NOT NULL DEFAULT '',
  takes_over INTEGER NOT NULL DEFAULT 0,
  build_target TEXT NOT NULL DEFAULT '',
  compose_file TEXT NOT NULL DEFAULT '',
  compose_project TEXT NOT NULL DEFAULT ''
);
INSERT INTO services_new (id, name, kind, source, branch, build_path, container, port, upstream, auto_deploy, env, volumes, git_token,
  webhook_secret, created_at, folder, release, instance, stopped, hook_at, hook_result, env_file, networks, takes_over, build_target,
  compose_file, compose_project)
SELECT id, name, kind, source, branch, build_path, container, port, upstream, auto_deploy, env, volumes, git_token,
  webhook_secret, created_at, folder, release, instance, stopped, hook_at, hook_result, env_file, networks, takes_over, build_target,
  compose_file, compose_project FROM services;
DROP TABLE services;
ALTER TABLE services_new RENAME TO services;
