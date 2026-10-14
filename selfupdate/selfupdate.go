// Package selfupdate 는 ARTEX 의 페이지 원클릭 업데이트를 구현한다: GitHub Release 에서 새 버전
// 바이너리를 받아 검증하고 스테이징한 뒤, 다음 시작 때 원자적으로 교체한다.
//
// 전체 역할 분담(start.sh / start.bat 참고):
//
//	기동 스크립트 = 단순 감시 루프. "프로세스 종료 후 종료 코드에 따라 재기동 여부만 결정"한다
//	본 패키지     = 실수하기 쉬운 로직 전부(다운로드 / SHA256 검증 / 스모크 테스트 / 교체 / 실패 롤백)
//
// 교체를 스크립트가 아니라 Go 에서 하는 이유는, sha256 검증과 스모크 테스트를 sh 와 bat 에서
// 각각 따로 구현해야 하기 때문이다(sha256sum / shasum / certutil). 그런데 이 부분이야말로 가장
// 틀리면 안 되는 단계다. 실행되지 않는 바이너리로 교체하면 감시 프로세스는 그것을 충실하게 계속
// 재기동하고, 사용자는 기기에 직접 들어가 수동으로 복구할 수밖에 없다.
//
// 업그레이드 한 번은 프로세스 시작을 세 번 거친다:
//
//	① 구버전 server 가 /api/update/apply 를 받음 → 다운로드 검증 → artex.new 스테이징 → exit 75
//	② 스크립트가 구버전을 다시 기동 → Bootstrap 이 artex.new 발견 → 검증+스모크 → 교체 → exit 75
//	③ 스크립트가 다시 기동, 이제 신버전 → Bootstrap 이 시도 1회 기록 → 시작 성공 후 표시 제거
//
// 어느 단계에서든 실패하면 구버전으로 되돌아간다: ② 에서 검증이 통과하지 못하면 스테이징 파일을
// 삭제하고 구버전을 계속 실행한다. ③ 에서는 표시를 지우기 전에 3 회 연속 살아남지 못하면(기동 직후
// 크래시) 자동으로 artex.old 를 되돌린다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart 는 "감시 프로세스가 나를 다시 기동하라"는 종료 코드(EX_TEMPFAIL)다. 기동 스크립트는
// 이 코드를 보면 즉시 다시 실행하며, 크래시 백오프에는 포함하지 않는다. 0 은 사용자가 정상 종료한
// 것(스크립트 루프 종료)이고, 그 밖의 값은 모두 크래시로 본다.
const ExitRestart = 75

// maxAttempts 는 교체 후 허용되는 기동 시도 횟수다. 신버전은 기동할 때마다 카운트를 +1 하며,
// settleDelay 동안 살아남으면 표시를 지운다. maxAttempts 번 연속 크래시하면 신버전이 아예 기동되지
// 않는다는 뜻이므로 자동 롤백한다.
const maxAttempts = 3

// Paths 는 한 번의 업그레이드에 관련된 모든 파일이며, 모두 **실행 파일이 있는 디렉터리** 아래에 둔다.
// 의도적으로 CWD 를 쓰지 않는다. 서비스로 실행할 때 작업 디렉터리는 / 나 임의의 경로일 수 있어,
// CWD 를 쓰면 스테이징 파일이 엉뚱한 곳에 생겨 교체 로직이 바로 무효가 된다.
type Paths struct {
	Dir     string // 실행 파일이 있는 디렉터리
	Current string // 현재 실행 중인 바이너리        artex      / artex.exe
	New     string // 스테이징된 새 버전            artex.new  / artex.new.exe
	Sum     string // 새 버전의 sha256(hex)  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 교체 전에 백업한 구버전      artex.old  / artex.old.exe
	Marker  string // 업그레이드 상태 표시 파일            artex.upgrade.json
}

// ResolvePaths 는 현재 실행 파일을 기준으로 업그레이드 경로 전체를 도출한다.
//
// Windows 에서는 .new/.old 에도 .exe 접미사가 붙어야 한다. 그렇지 않으면 스모크 테스트와 교체 후
// 실행이 모두 실패한다. 그래서 먼저 접미사를 떼고 조합해야 두 플랫폼의 이름 규칙이 대칭이 된다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 확인 실패: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows 에서는 ".exe", Unix 에서는 보통 비어 있다
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker 는 한 번의 교체 진행 상황을 기록하며, 신버전이 기동되지 않을 때 자동 롤백을 트리거하는 데 쓴다.
type marker struct {
	From     string `json:"from"`     // 업그레이드 전 버전
	To       string `json:"to"`       // 목표 버전
	Attempts int    `json:"attempts"` // 교체 후 기동을 시도한 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged 는 스테이징 파일을 정리한다. 교체 성공, 검증 실패, 사용자 취소 모두 이 함수를 거쳐,
// 남아 있는 artex.new 가 다음 기동 때 다시 시도되지 않도록 한다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions 는 두 버전 번호를 비교해 -1/0/1(a<b / a==b / a>b)을 반환한다.
// ok=false 는 적어도 한쪽이 비교 가능한 버전 번호가 아님을 뜻한다(예: 로컬 개발 빌드의 "dev" 또는
// git describe 가 만든 "0.3.7-2-gabc1234-dirty"). 이 경우 호출자는 원클릭 업데이트를 비활성화해야 한다.
// 그렇지 않으면 개발 중인 빌드를 정식 버전으로 "업그레이드"해서 커밋되지 않은 변경을 덮어쓰게 된다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion 은 "v0.3.7" / "0.3.7" 형식의 버전 번호를 [3]int 로 파싱한다.
//
// 순수한 세 부분 형식만 허용한다. build.sh 가 태그가 아닌 빌드에서 git describe 로 만든
// "0.3.7-2-gabc1234" 같은 접미사 붙은 버전은 반드시 비교 불가로 판정해야 하며, 0.3.7 로 취급해서는
// 안 된다. 그렇지 않으면 개발 빌드가 "이미 최신"으로 잘못 판단되거나 정식 버전에 덮어써진다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker 는 프로세스가 컨테이너 안에서 실행 중인지 알려준다. Docker 에서는 교체 결과가 컨테이너의
// 쓰기 가능 레이어에 기록되므로, `docker compose up -d` 로 컨테이너를 다시 만들면 이미지에 들어 있던
// 버전으로 되돌아간다. 이는 예상된 동작이다(그때 사용자는 어차피 새 이미지를 받고 있는 중이다).
// 다만 프런트엔드가 이를 근거로 안내 문구를 명확히 할 수 있어야 한다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
