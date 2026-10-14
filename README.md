# Naru

> Nginx Proxy Manager처럼 화면에서 프록시를 다루고, push하면 배포까지.

[English](README.en.md) · [소개 페이지](https://shelf.nakangmin.duckdns.org/) · MIT

도메인 하나 붙이려고 nginx 설정 파일을 고치고, certbot 크론을 걸고, 포트가 겹치지 않게 표를 그려 관리하는 일 —
그게 싫어서 만든 도구입니다. Naru는 **80/443을 물고 있는 리버스 프록시**이고, 도메인·인증서·연결 대상을 화면에서 관리합니다.

앱 배포는 그 위에 얹힌 기능입니다. 프록시가 어차피 컨테이너를 알아야 하니, 이왕이면 컨테이너를 띄우는 것까지 맡긴 것입니다.

## 왜 만들었나

Nginx Proxy Manager를 자주 썼습니다. 두 가지가 아쉬웠습니다.

- **Caddy판이 필요했습니다.** Caddy는 HTTPS 자동 발급·갱신과 HTTP/3가 기본인데, CLI와 설정 파일로만 다룹니다. 그걸 NPM처럼 화면에서 다루고 싶었습니다.
- **CI/CD도 해야 했습니다.** NPM은 도메인만 붙여줍니다. Naru는 push하면 빌드·배포·도메인 연결까지 합니다.

나루는 나루터입니다 — 요청이 도착해 각 서비스로 건너가고, 새 버전이 들어와 정박하는 곳. 예전 이름은 Shelf(v1, Node)였고,
v2에서 Go로 다시 만들면서 웹서버 엔진을 Caddy로 바꾸고 이름도 바꿨습니다. v1에서 넘어온 컨테이너(`shelf-*`)와 웹훅 주소는 그대로 이어받습니다.

> 지금 코드는 `v2` 브랜치에 있고, 2026-10-10부터 실서버가 v2로 돕니다. 정리가 끝나면 `main`으로 합칩니다.
> 설계와 진행 기록은 [v2 계획](docs/design/v2.md).

---

## 지금 되는 것

**웹서버 — Caddy가 엔진, 사용자는 화면만**

- 도메인을 적으면 그 서비스로 보냅니다. 컨테이너는 이름으로 바로 연결됩니다 — 호스트 포트를 열 필요가 없습니다.
- **HTTPS 자동** — 주소가 생기면 Caddy가 인증서를 받고 갱신합니다. **인증서가 생기면 그때부터** HTTP를 HTTPS로 넘깁니다(307).
  인증서가 없거나 끝나면 넘기지 않으므로 관리 화면이 자기 서버에서 잠기지 않습니다. 주소 옆에 "HTTPS · 언제까지"가 보입니다.
- 칸 없이 늘 켜져 있는 것: HTTP/2·HTTP/3, 웹소켓, 압축(gzip·zstd), 앱에 실제 접속 정보 전달(`X-Forwarded-*`). 모르는 주소는 404.
- Caddy 설정은 Naru가 통째로 그려서 관리 소켓(유닉스 소켓)으로 밀어 넣습니다 — 손으로 고칠 설정 파일이 없습니다.

**서비스마다 웹서버 설정** — 응답 헤더, 허용 IP, 비밀번호 보호(기본 인증), 점검 중, 경로별 연결(`/api`를 다른 서비스로), 고급 칸에 Caddyfile 지시어.
nginx 설정을 붙여 넣으면 칸으로 옮겨 줍니다. 적용하면 바로 웹서버에 맞춰 보고, 웹서버가 거절하면 이전 설정으로 되돌립니다.

**서비스 — 네 가지**

| 종류 | 무엇 |
|---|---|
| 저장소 | Git 저장소의 `Dockerfile`로 빌드해 컨테이너로. 모노레포는 빌드 경로로. 포트는 `EXPOSE`에서 찾습니다 |
| 이미지 | Docker Hub·GHCR 이미지를 받아 컨테이너로 |
| 정적 사이트 | 저장소의 폴더를 컨테이너 없이 웹서버가 직접 서빙. 숨김 파일(`.env`, `.github`…)은 공개하지 않습니다 |
| 외부 연결 | 이미 떠 있는 것(NAS, 공유기, 호스트의 프로그램)에 주소만. `localhost`는 이 서버를 뜻합니다 |

**배포**

- **push하면 배포** — 서비스마다 웹훅 주소와 시크릿. GitHub HMAC · GitLab 토큰 · `?secret=`(레지스트리). 등록한 브랜치만.
- **끊김 없이** — 새 컨테이너를 띄우고, 응답하면 그때 웹서버를 새 것으로 돌린 뒤 옛 것을 내립니다. 새 것이 죽으면 옛 것을 그대로 둡니다.
- 배포 중에 온 push는 끝난 뒤 한 번으로 합칩니다. 배포 단계와 전체 기록을 화면에서 실시간으로 봅니다.
- **되돌리기** — 빌드 없이 그때의 이미지·파일로. 빌드 이미지 3개, 정적 배포본 5개를 남깁니다.

**지켜보기 · 알림 · 진단**

- 30초마다 서비스·웹서버·Docker를 보고 **바뀔 때만** 알립니다 — Discord·Slack·일반 웹훅(서명 포함). 두 번 연속 문제여야 멈춤으로 봅니다.
- 홈 맨 위 **"주의가 필요한 것"** — 멈춘 앱, 다른 포트를 듣는 앱([포트 바꾸기] 버튼), 이 서버를 가리키지 않는 DNS, 곧 끝나는 인증서, 오지 않는 웹훅.
- **로그 탭** — 앱 출력과 받은 요청을 시간순으로 한 화면에. 서비스별 CPU·메모리.

**처음 설정 · 관리**

- `.env` 없이 첫 실행 마법사: 계정 → 관리 화면 주소(DNS 확인) → HTTPS 연락처. 마법사 주소는 서버 로그에만 찍힙니다.
- 한국어·영어 화면. 서버 CPU·메모리·디스크.

**없는 것** — HSTS를 화면에서 켜는 칸(v1에서 옮긴 주소만 켜져 있음), 주소 넘기기 전용 설정(고급 칸의 `redir`로),
인증서 직접 올리기·DNS 확인(와일드카드), TCP/UDP 포워딩, 여러 관리자 권한.

---

## 설치 (운영)

Docker와 Docker Compose가 있는 리눅스 서버에서:

```bash
git clone -b v2 https://github.com/KangminNa/Naru.git naru && cd naru/deploy/docker
cp env.example .env && chmod 600 .env        # ACME_EMAIL 등을 채운다
docker network create naru-net               # 앱 컨테이너와 웹서버가 함께 쓰는 네트워크
docker compose up -d --build
docker compose logs naru | grep setup         # 첫 설정 주소 — 로그를 볼 수 있는 사람만 관리자를 만들 수 있다
```

- 운영 구성은 **`deploy/docker/`에서만** 돌립니다. 저장소 맨 위의 `compose.yml`은 로컬 개발용입니다.
- 업데이트: `git pull && docker compose up -d --build` (`deploy/docker`에서). 웹서버는 그대로 돌고 Naru만 다시 뜹니다.
- 바깥에는 80/443만 열립니다. Naru 자신의 화면은 `127.0.0.1:8080`(서버 안)과 관리 주소로만 닿습니다.

계정을 잊었다면 서버 셸에서:

```bash
docker exec naru naru users
 docker exec naru naru passwd <아이디> '<새 비밀번호>'   # 맨 앞 공백: 셸 기록에 남지 않게. 그 계정의 모든 로그인이 끊긴다
docker exec naru naru reset                                # 계정 전체 삭제 → 다시 시작하면 첫 설정이 열린다
```

## 로컬에서 띄워 보기

Docker만 있으면 됩니다 (Go 설치 불필요).

```bash
git clone -b v2 https://github.com/KangminNa/Naru.git && cd Naru
docker network create naru-net
docker compose up -d --build                 # 프로젝트 이름 naru-dev — 운영과 겹치지 않는다
docker compose logs naru | grep setup
```

웹서버는 `127.0.0.1:8088`(HTTP)·`8443`(HTTPS)에 열고, 인증서는 Caddy 내부 CA로 받습니다.
`*.localhost` 주소는 브라우저가 알아서 이 컴퓨터로 보냅니다 — 예: 관리 주소를 `naru.localhost`로 두고 `http://naru.localhost:8088`.

---

## 고쳐 쓰기

MIT입니다. fork해서 고치든, 필요한 부분만 떼어 쓰든 상관없습니다. 고치기 쉽게 하려고 지킨 것들:

- **하는 일마다 객체, 관계는 인터페이스로만** — 패키지는 `model`(데이터)과 `contract`(인터페이스)만 import합니다. 구체 타입은 `app`(조립) 한 곳에서만 만납니다.
- **바깥은 도구 뒤에** — Docker·git·Caddy·SQLite·파일·HTTP는 각자 도구 패키지에 있어서, 테스트는 가짜로 바꿔 끼우고 진짜 조립 그대로 화면을 시험합니다.
- **문서가 코드를 강제합니다** — [객체 설계](docs/design/v2-objects.md)의 규칙 R1~R12를 `internal/archtest`가 검사합니다. 문서에 없는 타입이 생기면 테스트가 실패합니다.
- **의존성 둘** — `modernc.org/sqlite`(순수 Go SQLite), `golang.org/x/crypto`(scrypt·bcrypt). HTML은 서버가 만들고 자바스크립트 프레임워크가 없습니다.

```
cmd/naru/            서버 · 셸 복구 명령
internal/
├── model/ contract/ 데이터 · 인터페이스 전부
├── access settings services kinds deploy webhook webserver views websettings watch notify   하는 일마다
├── web/ cli/        들어오는 쪽
├── store/           SQLite (SQL은 여기에만)
├── docker git files caddy netcheck stats events system httppost                             바깥에 닿는 도구
├── app/             조립
└── archtest/        규칙 검사
```

```bash
scripts/go.sh test ./...        # Docker 안의 Go로
scripts/caddy-validate.sh       # 그린 Caddy 설정을 실제 Caddy가 받아들이는지
```

---

## 운영할 때 알아둘 것

- Naru는 `/var/run/docker.sock`을 마운트합니다. 호스트 Docker를 전부 제어할 수 있다는 뜻이고, 따라서 **관리자 계정은 설계상 root와 동급**입니다.
  본인 서버에서만 쓰고, 서버를 맡길 만한 사람에게만 계정을 주세요. 관리 화면 비밀번호는 길게.
- 서비스를 배포한다는 건 남의 코드를 내 서버에서 실행한다는 뜻입니다. 믿는 저장소만 올리세요.
  docker.sock을 받는 앱(Portainer 같은 것)을 올렸다면 허용 IP나 비밀번호 보호를 거세요 — 그 앱도 root와 같습니다.
- 시크릿(Git 토큰, 웹훅 시크릿, 환경 변수)은 데이터 폴더에 저장됩니다. 그 폴더를 보호하고 백업하세요. 화면·로그·웹서버 설정에는 나가지 않습니다.
- **80/443만 외부에 노출**하세요. Caddy 관리 API는 Caddy와 Naru만 보는 유닉스 소켓으로만 엽니다.
- 들어 있는 방어: scrypt 비밀번호, 로그인 5회 실패 시 15분 잠금, Secure/httpOnly/SameSite 쿠키, 다른 사이트에서 온 폼 거부, CSP,
  서명 확인한 웹훅과 본문 크기 제한, git 주소·브랜치·경로·이미지 이름 검증.

---

## License

MIT © Kangmin Na
