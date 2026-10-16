#!/usr/bin/env bash
# 인터넷이 되는 PC 에서 실행한다. 폐쇄망으로 옮길 Docker 이미지와 실행 파일을
# dist/airgap/ 아래에 모은다. 폐쇄망에서는 scripts/import-airgap.sh 로 되돌린다.
#
# 전제: 먼저 로컬 이미지를 빌드해 둔다.
#   docker compose -f docker-compose.yml -f docker-compose.build.yml build
#
# 사용법:
#   scripts/export-airgap.sh [ARTEX_IMAGE] [POSTGRES_IMAGE]
# 기본값:
#   ARTEX_IMAGE   = artex-ko:local
#   POSTGRES_IMAGE= postgres:16-alpine
set -euo pipefail

ARTEX_IMAGE="${1:-artex-ko:local}"
POSTGRES_IMAGE="${2:-postgres:16-alpine}"

# 저장소 루트로 이동(이 스크립트의 상위 디렉터리)
cd "$(dirname "$0")/.."
OUT="dist/airgap"
mkdir -p "$OUT"

echo "[1/4] 로컬 이미지 존재 확인: $ARTEX_IMAGE"
if ! docker image inspect "$ARTEX_IMAGE" >/dev/null 2>&1; then
  echo "  ! $ARTEX_IMAGE 가 없습니다. 먼저 빌드하세요:"
  echo "    docker compose -f docker-compose.yml -f docker-compose.build.yml build"
  exit 1
fi

echo "[2/4] postgres 이미지 확보: $POSTGRES_IMAGE"
docker image inspect "$POSTGRES_IMAGE" >/dev/null 2>&1 || docker pull "$POSTGRES_IMAGE"

echo "[3/4] 이미지 저장(docker save) → $OUT"
# 두 이미지를 하나의 tar 로 묶는다(로드 한 번으로 끝).
docker save -o "$OUT/artex-images.tar" "$ARTEX_IMAGE" "$POSTGRES_IMAGE"

echo "[4/4] 실행에 필요한 파일 복사 → $OUT"
cp docker-compose.yml docker-compose.airgap.yml "$OUT/"
cp scripts/import-airgap.sh "$OUT/"
# skills 는 compose 가 ./skills 로 마운트하므로 함께 옮긴다.
rm -rf "$OUT/skills"
cp -r skills "$OUT/skills"
# .env 템플릿(값은 폐쇄망에서 채운다). 실제 .env 가 있으면 참고용으로 복사하지 않는다.
if [ ! -f "$OUT/.env.airgap.example" ]; then
  cat > "$OUT/.env.airgap.example" <<'EOF'
# 폐쇄망 .env — 이 파일을 .env 로 복사해 값을 채우세요.
POSTGRES_PASSWORD=change-me-8chars+
ARTEX_ADMIN_PASSWORD=change-me-8chars+
ARTEX_LANG=ko
# 폐쇄망 LLM 엔드포인트(OpenAI 호환). 사내 vLLM/Ollama/TGI 등을 가리킨다.
ARTEX_LLM_PROVIDER=openai
ARTEX_LLM_BASE_URL=http://CHANGE-ME-llm-host:8000/v1
ARTEX_LLM_MODEL=CHANGE-ME-model-name
# 외부 아님: 비워 둡니다(폐쇄망에서는 Anthropic/OpenAI 로 직접 못 나감).
ANTHROPIC_API_KEY=
OPENAI_API_KEY=
EOF
fi

echo
echo "완료. '$OUT' 폴더 전체를 매체로 폐쇄망에 옮기세요."
echo "폐쇄망에서: cd 로 그 폴더 이동 후  ./import-airgap.sh"
du -sh "$OUT" 2>/dev/null || true
