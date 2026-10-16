#!/usr/bin/env bash
# 폐쇄망 호스트에서 실행한다. export-airgap 으로 만든 dist/airgap 폴더 안에서 돌린다.
# 이미지를 docker load 로 넣고, .env 가 준비돼 있으면 바로 기동한다.
#
# 사용법(옮겨 온 폴더 안에서):
#   ./import-airgap.sh          # load 만
#   ./import-airgap.sh up       # load 후 docker compose up -d 까지
set -euo pipefail

cd "$(dirname "$0")"
TAR="artex-images.tar"

if [ ! -f "$TAR" ]; then
  echo "! $TAR 가 현재 폴더에 없습니다. export-airgap 으로 만든 dist/airgap 폴더 안에서 실행하세요."
  exit 1
fi

echo "[1/3] 이미지 로드(docker load): $TAR"
docker load -i "$TAR"

echo "[2/3] 로드된 이미지 확인"
docker images | grep -E "artex-ko|postgres" || true

echo "[3/3] .env 확인"
if [ ! -f ".env" ]; then
  echo "  ! .env 가 없습니다. 템플릿을 복사해 값을 채우세요:"
  echo "    cp .env.airgap.example .env  &&  vi .env"
  echo "  특히 ARTEX_LLM_BASE_URL(사내 LLM 엔드포인트)를 반드시 지정해야 에이전트가 동작합니다."
  echo
  echo "  준비되면:"
  echo "    docker compose -f docker-compose.yml -f docker-compose.airgap.yml up -d"
  exit 0
fi

if [ "${1:-}" = "up" ]; then
  echo "기동: docker compose up -d"
  docker compose -f docker-compose.yml -f docker-compose.airgap.yml up -d
  echo
  echo "로그:  docker compose -f docker-compose.yml -f docker-compose.airgap.yml logs -f artex"
  echo "접속:  http://localhost:8787"
else
  echo "load 완료. 기동하려면:"
  echo "  docker compose -f docker-compose.yml -f docker-compose.airgap.yml up -d"
fi
