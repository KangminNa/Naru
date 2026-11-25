# Naru v2 — Go로 만든 관제 서버. 빌드도 컨테이너 안에서 한다 (로컬에 Go가 없어도 된다).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=2.0.0-dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/naru ./cmd/naru

# buildx: 저장소의 Dockerfile을 BuildKit으로 빌드한다 — 로컬의 docker build와 같은 빌더 (docker CLI는 넣지 않는다).
# 버전을 올리면 scripts/buildx-check.sh를 돌린다.
FROM docker/buildx-bin:0.38.2 AS buildx
# docker compose: Compose 종류를 compose 파일대로 빌드하고 띄운다 (빌드는 위의 buildx를 플러그인으로 쓴다).
FROM docker/compose-bin:v5.6.1 AS compose

FROM alpine:3
# git: 저장소를 가져온다 (토큰은 명령줄·설정 파일에 남기지 않는다)
RUN apk add --no-cache ca-certificates tzdata git
COPY --from=buildx /buildx /usr/local/libexec/naru/buildx
COPY --from=compose /docker-compose /usr/local/libexec/naru/compose
COPY --from=build /out/naru /usr/local/bin/naru
ENV NARU_DATA_DIR=/data NARU_LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["naru"]
