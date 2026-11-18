-- 저장소 서비스의 .env 파일 칸 (2026-11-18). 빌드할 때 빌드 폴더의 .env로 넣는다 — 비밀이다.
ALTER TABLE services ADD COLUMN env_file TEXT NOT NULL DEFAULT '';
