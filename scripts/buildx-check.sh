#!/bin/sh
# Naru 이미지에 넣은 buildx가 이 Docker에서 Naru가 기대하는 대로 빌드하는지 확인한다. buildx 버전을 올릴 때 돌린다.
#   scripts/buildx-check.sh
#   - BuildKit 문법(RUN --mount=type=cache)이 빌드된다
#   - stdin tar 묶음에 .dockerignore를 다시 적용하지 않는다 — ".env 파일" 칸이 넣은 .env가 남는다
#   - --iidfile로 받은 이미지 ID로 inspect가 된다
# 버전은 Dockerfile의 docker/buildx-bin 줄에서 읽고, Naru 이미지처럼 alpine + ca-certificates 위에서 돌린다
# (buildx는 레지스트리 토큰을 직접 받는다 — 인증서가 없으면 FROM에서 실패한다). 만든 이미지는 이름으로 지운다.
# 빌드 캐시는 지우지 않는다 (busybox 몇 MB — 비우려면 사람이 docker builder prune).
set -e
cd "$(dirname "$0")/.."
ver=$(sed -n 's/^FROM docker\/buildx-bin:\([0-9.]*\).*/\1/p' Dockerfile)
[ -n "$ver" ] || { echo "Dockerfile에서 buildx 버전을 찾지 못했어요"; exit 1; }
base=busybox:1.36
bin=docker/buildx-bin:$ver
tool=naru-buildx-check:tool
had() { docker image inspect "$1" >/dev/null 2>&1 && echo 1 || echo 0; }
had_base=$(had "$base"); had_bin=$(had "$bin"); had_alpine=$(had alpine:3)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
status=0

printf 'FROM %s AS buildx\nFROM alpine:3\nRUN apk add --no-cache ca-certificates\nCOPY --from=buildx /buildx /buildx\n' "$bin" |
  docker build -q -t "$tool" - >/dev/null

# build <이름> — $work/<이름> 폴더를 Naru처럼 stdin tar로 넘겨 빌드한다. 이미지 ID는 $work/out/<이름>.iid에
# (buildx는 ID 파일을 임시 파일로 쓰고 이름을 바꿔서 /dev/stdout으로는 받을 수 없다)
build() {
  (cd "$work/$1" && COPYFILE_DISABLE=1 tar --format ustar -cf - .) |
    docker run --rm -i --tmpfs /tmp -v /var/run/docker.sock:/var/run/docker.sock -v "$work/out:/out" \
      -e DOCKER_HOST=unix:///var/run/docker.sock -e DOCKER_CONFIG=/cfg --entrypoint /buildx "$tool" \
      build --progress=plain --provenance=false --sbom=false --pull --iidfile "/out/$1.iid" -t "naru-buildx-check:$1" - >"$work/$1.log" 2>&1
}

check() {
  if build "$1" && id=$(cat "$work/out/$1.iid" 2>/dev/null) && [ -n "$id" ] && docker image inspect "$id" >/dev/null 2>&1; then
    echo "ok    $2"
  else
    echo "FAIL  $2"; tail -5 "$work/$1.log"; status=1
  fi
}

mkdir -p "$work/mount" "$work/dotenv" "$work/out"
printf 'FROM %s\nRUN --mount=type=cache,target=/c echo hi > /c/x && cat /c/x\n' "$base" > "$work/mount/Dockerfile"
printf 'FROM %s\nCOPY .env /app/.env\nRUN grep -q FROM_NARU /app/.env\n' "$base" > "$work/dotenv/Dockerfile"
printf '.env\n' > "$work/dotenv/.dockerignore"
printf 'FROM_NARU=1\n' > "$work/dotenv/.env"

echo "buildx $ver · Docker $(docker version --format '{{.Server.Version}}')"
check mount  "RUN --mount=type=cache (BuildKit 문법)"
check dotenv ".dockerignore가 뺀 .env도 묶음대로 들어감 + 이미지 ID로 inspect"

docker rmi naru-buildx-check:mount naru-buildx-check:dotenv "$tool" >/dev/null 2>&1 || true
[ "$had_base" = 1 ] || docker rmi "$base" >/dev/null 2>&1 || true
[ "$had_bin" = 1 ] || docker rmi "$bin" >/dev/null 2>&1 || true
[ "$had_alpine" = 1 ] || docker rmi alpine:3 >/dev/null 2>&1 || true
exit $status
