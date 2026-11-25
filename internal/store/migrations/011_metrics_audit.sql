-- 서버 모니터링 2단계 (2026-11-25).
-- 자원 사용 기록: 1분 값(res=60)은 2일, 그 뒤로는 1시간 값(res=3600, 평균·최고)으로 30일. service 0은 서버.
CREATE TABLE metrics (
  at INTEGER NOT NULL,
  service INTEGER NOT NULL,
  res INTEGER NOT NULL,
  cpu REAL NOT NULL,
  cpu_max REAL NOT NULL,
  mem_used INTEGER NOT NULL,
  mem_max INTEGER NOT NULL,
  mem_total INTEGER NOT NULL,
  disk_used INTEGER NOT NULL DEFAULT 0,
  disk_total INTEGER NOT NULL DEFAULT 0,
  net_in INTEGER NOT NULL DEFAULT 0,
  net_out INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX metrics_at ON metrics(at);

-- 기록: 관리자가 한 일과 Naru가 본 일. 90일. 비밀 값은 넣지 않는다.
CREATE TABLE audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  system INTEGER NOT NULL,
  who TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  service INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 1,
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_at ON audit(at);
