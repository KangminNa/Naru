#!/bin/sh
# 렌더러가 그린 골든 설정을 실제 Caddy가 받아들이는지 확인한다.
#   scripts/caddy-validate.sh
# 골든 파일은 internal/caddy/testdata/ — `scripts/go.sh test ./internal/caddy -update`로 다시 만든다.
# Caddy는 운영이 쓰는 버전(deploy/docker/compose.yml)으로 돌린다 — 버전을 올릴 때 이 스크립트를 돌린다.
set -e
cd "$(dirname "$0")/.."
image=$(sed -n 's/^ *image: *\(caddy:[0-9.]*\) *$/\1/p' deploy/docker/compose.yml | head -1)
[ -n "$image" ] || { echo "deploy/docker/compose.yml에서 Caddy 버전을 찾지 못했어요"; exit 1; }
echo "$image"
status=0
for f in internal/caddy/testdata/*.json; do
  name=$(basename "$f")
  if out=$(docker run --rm -v "$PWD/internal/caddy/testdata:/t:ro" "$image" caddy validate --config "/t/$name" 2>&1); then
    echo "ok    $name"
  else
    echo "FAIL  $name"; echo "$out" | tail -5; status=1
  fi
done
exit $status
