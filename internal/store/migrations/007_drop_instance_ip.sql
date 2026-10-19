-- 설치형(systemd)을 지웠다 (2026-10-14). 컨테이너 IP는 설치형이 웹서버를 IP로 보내느라 기록했다 —
-- Docker 방식은 별칭으로 보내므로 쓰지 않는다.
ALTER TABLE services DROP COLUMN instance_ip;
