-- Compose 프로젝트와 주소마다 대상 (2026-11-25).
ALTER TABLE services ADD COLUMN compose_file TEXT NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN compose_project TEXT NOT NULL DEFAULT '';
-- 주소가 가리키는 compose 서비스와 포트 — 비면 서비스의 앱
ALTER TABLE domains ADD COLUMN target_service TEXT NOT NULL DEFAULT '';
ALTER TABLE domains ADD COLUMN target_port INTEGER NOT NULL DEFAULT 0;
