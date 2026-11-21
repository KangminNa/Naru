-- 서비스의 "다른 네트워크에도 붙이기" (2026-11-21). 앱 네트워크 말고 더 붙일 Docker 네트워크 이름들 — 빈칸으로 나눈다.
ALTER TABLE services ADD COLUMN networks TEXT NOT NULL DEFAULT '';
