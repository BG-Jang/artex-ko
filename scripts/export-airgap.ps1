# 인터넷이 되는 Windows PC 에서 실행한다(PowerShell). 폐쇄망으로 옮길 Docker
# 이미지와 실행 파일을 dist\airgap\ 아래에 모은다. 폐쇄망에서는 import-airgap.sh 로 되돌린다.
#
# 전제: 먼저 로컬 이미지를 빌드해 둔다.
#   docker compose -f docker-compose.yml -f docker-compose.build.yml build
#
# 사용법(저장소 루트에서):
#   powershell -ExecutionPolicy Bypass -File scripts\export-airgap.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\export-airgap.ps1 -ArtexImage artex-ko:local -PostgresImage postgres:16-alpine
param(
  [string]$ArtexImage = "artex-ko:local",
  [string]$PostgresImage = "postgres:16-alpine"
)
$ErrorActionPreference = "Stop"

# 저장소 루트로 이동(이 스크립트의 상위 디렉터리)
Set-Location (Join-Path $PSScriptRoot "..")
$OUT = "dist\airgap"
New-Item -ItemType Directory -Force -Path $OUT | Out-Null

Write-Host "[1/4] 로컬 이미지 존재 확인: $ArtexImage"
docker image inspect $ArtexImage *> $null
if ($LASTEXITCODE -ne 0) {
  Write-Host "  ! $ArtexImage 가 없습니다. 먼저 빌드하세요:"
  Write-Host "    docker compose -f docker-compose.yml -f docker-compose.build.yml build"
  exit 1
}

Write-Host "[2/4] postgres 이미지 확보: $PostgresImage"
docker image inspect $PostgresImage *> $null
if ($LASTEXITCODE -ne 0) { docker pull $PostgresImage }

Write-Host "[3/4] 이미지 저장(docker save) -> $OUT"
docker save -o "$OUT\artex-images.tar" $ArtexImage $PostgresImage

Write-Host "[4/4] 실행에 필요한 파일 복사 -> $OUT"
Copy-Item docker-compose.yml, docker-compose.airgap.yml $OUT -Force
Copy-Item scripts\import-airgap.sh $OUT -Force
if (Test-Path "$OUT\skills") { Remove-Item "$OUT\skills" -Recurse -Force }
Copy-Item skills "$OUT\skills" -Recurse -Force

$envExample = "$OUT\.env.airgap.example"
if (-not (Test-Path $envExample)) {
@'
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
'@ | Set-Content -Encoding UTF8 $envExample
}

Write-Host ""
Write-Host "완료. '$OUT' 폴더 전체를 매체로 폐쇄망에 옮기세요."
Write-Host "폐쇄망(리눅스 호스트)에서: 그 폴더로 이동 후  ./import-airgap.sh"
