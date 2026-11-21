-- 넘겨받기 (2026-11-21). 다음 배포가 성공하면 이 번호의 서비스에서 주소와 웹서버 설정을 옮겨 온다. 0이면 없음.
ALTER TABLE services ADD COLUMN takes_over INTEGER NOT NULL DEFAULT 0;
