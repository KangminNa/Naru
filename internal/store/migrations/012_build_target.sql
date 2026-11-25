-- 저장소 서비스의 빌드 타깃 (2026-11-25). Dockerfile의 단계 이름 — 비면 맨 마지막 단계.
ALTER TABLE services ADD COLUMN build_target TEXT NOT NULL DEFAULT '';
