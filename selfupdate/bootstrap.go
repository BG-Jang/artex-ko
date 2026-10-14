package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv 는 스모크 테스트로 실행되는 자식 프로세스가 Bootstrap 을 바로 건너뛰게 한다.
//
// 엄밀히 말하면 이 값이 없어도 문제는 생기지 않는다. 자식 프로세스의 os.Executable() 은 artex.new 이고,
// 거기서 도출되는 경로는 모두 .new 접두사를 가져 실제 업그레이드 파일에 닿지 않는다. 그러나 이런 우연에
// 의존하는 것은 지나치게 취약하다. 명시적으로 단락시키면 의도가 분명하고, 자식 프로세스가 불필요하게
// 디스크를 조회하는 것도 막을 수 있다.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action 은 Bootstrap 이 main 에 내리는 지시다.
type Action int

const (
	// Continue: 평소처럼 server 를 시작한다.
	Continue Action = iota
	// Restart: 즉시 ExitRestart 로 종료하여 감시 스크립트가 다시 기동하게 한다.
	Restart
)

// State 는 이번 기동 시점의 업그레이드 상태를 나타내며, /api/update/check 가 프런트엔드에
// "직전 업그레이드가 성공했는지 롤백되었는지"를 사실대로 알려주는 데 쓴다.
type State struct {
	Pending     bool   // 교체 후 아직 안정성이 확인되지 않음
	RolledBack  bool   // 이번 기동에서 방금 자동 롤백을 실행함
	FailedStage bool   // 스테이징 파일의 검증/스모크 테스트를 통과하지 못해 폐기됨
	Detail      string // 사용자에게 보여줄 한 줄 설명
}

// Bootstrap 은 main 의 맨 처음에 실행되며, 포트 리슨이나 데이터베이스 오픈보다 먼저 호출되어야 한다.
//
// 세 가지 상황:
//
//	① 스테이징 파일 artex.new 가 존재   → 검증 + 스모크 테스트, 통과하면 교체 후 재시작 요청; 통과하지 못하면 폐기하고 구버전 계속 실행
//	② 표시 파일만 남아 있음             → 방금 교체가 끝난 상태이므로 시도 1회를 누적; 연속 실패 횟수가 충분하면 롤백
//	③ 아무것도 없음                     → 정상 기동
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 자체 초기화를 건너뜀: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged 는 "스테이징 파일이 존재하는" 상황을 처리한다. 검증을 통과하면 교체하고, 실패하면 폐기한다.
//
// 이곳은 업그레이드 경로 전체에서 실행 파일을 덮어쓰는 유일한 지점이자 마지막 관문이다. 스모크 테스트가
// 다운로드 손상, 아키텍처 오선택, 동적 링크 누락 같은 문제를 걸러낸다. 실행되지 않는 바이너리를 통과시키면
// 감시 스크립트는 지치지 않고 계속 재기동하고, Go 코드는 아예 실행될 기회를 얻지 못하므로 자동 롤백도 불가능해진다.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 스테이징된 새 버전이 검증을 통과하지 못해 폐기하고 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전 검증에 실패해 폐기했습니다: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 교체 실패, 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "버전 교체에 실패했습니다: " + err.Error()}
	}

	// 교체 성공. 표시 파일은 남겨 두고, 다음 기동(이때 실행되는 것이 신버전이다)에서 안정성을 확인하도록 넘긴다.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표시 파일 기록 실패(자동 롤백 기능 상실): %v", err)
	}
	log.Printf("[update] %s 로 교체 완료, 재시작을 위해 종료합니다(exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback 은 "교체 후 기동"을 처리한다. 시도 횟수를 누적하고, 한도를 넘으면 구버전을 되돌린다.
//
// 카운트는 Go 코드가 실행된 뒤에만 증가하므로, 이 함수가 커버하는 것은 "실행은 되지만 초기화 중 크래시하는"
// 장애(설정 비호환, 포트 점유, DB 마이그레이션 실패 등)다. "exec 자체가 불가능한" 경우는 교체 전 스모크
// 테스트가 막으며, 두 메커니즘이 합쳐져 완전한 보호를 이룬다.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// 롤백까지 실패했다면 더 이상 재시작하지 않는다. 그렇지 않으면 무한 재시작에 빠진다.
			// 표시 파일을 지우고 현재 상태 그대로 기동하게 한다. 기동이 안 되더라도 사용자는 최소한 로그에서 원인을 확인할 수 있다.
			log.Printf("[update] 신버전이 %d 회 연속 기동에 실패했고 롤백도 실패했습니다: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전 시작에 실패하고 롤백도 실패했습니다: " + err.Error()}
		}
		log.Printf("[update] 신버전이 %d 회 연속 기동에 실패하여 %s 로 롤백했습니다. 재시작을 위해 종료합니다(exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전 시작에 실패해 이전 버전으로 롤백했습니다(%s)", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표시 파일 갱신 실패: %v", err)
	}
	log.Printf("[update] 신버전 기동 중(%d/%d 번째 시도), 안정적으로 실행되면 업그레이드를 확정합니다",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle 은 신버전이 안정적으로 실행 중임을 확인하고 업그레이드 표시 파일을 지운다.
//
// main 이 HTTP 리슨을 시작한 뒤 지연 호출한다. 이 시간 동안 살아남아야 인정되며, 그렇지 않으면 표시 파일이
// 그대로 남아 다음 기동에서도 시도 횟수가 계속 누적되어 롤백이 트리거될 때까지 이어진다.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 업그레이드 후의 기동이 아니므로 할 일이 없다
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 업그레이드 표시 파일 삭제 실패: %v", err)
		return
	}
	log.Printf("[update] 신버전이 안정적으로 실행되어 업그레이드를 완료했습니다(이전 버전은 %s 로 보관)", p.Old)
}

// SettleDelay 는 "신버전이 살아남았다"고 판정하는 데 필요한 실행 시간이다.
const SettleDelay = 30 * time.Second

// verifyStaged 는 스테이징 파일을 검증한다. 먼저 SHA256 을 대조한 다음, 실제로 한 번 기동해 본다.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기 실패: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산 실패: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 이 일치하지 않습니다(다운로드가 손상됐거나 변조됨)")
	}
	return smokeTest(p.New)
}

// smokeTest 는 -h 로 새 바이너리를 실행해, 현재 시스템에서 실제로 실행 가능한지 확인한다.
// 다운로드 잘림, 아키텍처 오선택(exec format error), 의존성 누락 등 한 부류의 문제를 걸러낼 수 있다.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여 실패: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리가 응답하지 않음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap 은 현재 바이너리를 스테이징된 새 버전으로 교체한다.
//
// Unix 와 Windows 모두 실행 중인 실행 파일의 rename 을 허용한다(Windows 가 금지하는 것은 삭제와 덮어쓰기이고
// rename 은 그 대상이 아니다). 따라서 여기서는 플랫폼별로 분기할 필요도 없고, 자기 자신을 먼저 멈출 필요도 없다.
func swap(p Paths) error {
	// Windows 의 rename 은 이미 존재하는 대상을 덮어쓰지 않으므로, 이전 업그레이드가 남긴 .old 를 먼저 지워야 한다.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("이전 백업 %s 정리 실패: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업 실패: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 교체에 실패했지만 현재 버전은 이미 옮겨진 상태이므로 원래 위치로 되돌려야 한다. 그렇지 않으면 다음 기동 때 실행 파일이 없다.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전 설치 실패(%v), 현재 버전 복구도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 설치 실패: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback 은 swap 이 백업한 구버전을 되돌린다.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("롤백할 백업이 없습니다(%s): %w", p.Old, err)
	}
	// 기동되지 않는 신버전은 바로 지우지 않고 .failed 로 옮겨 원인 조사용으로 남긴다.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 이동 실패: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("이전 버전 복구 실패: %w", err)
	}
	return nil
}

// Rollback 은 /api/update/rollback 의 구현으로, 이전 버전으로 능동적으로 되돌린다.
// 교체만 수행하며, 재시작 역시 감시 스크립트에 맡긴다(호출자는 이후 ExitRestart 로 종료한다).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("롤백할 이전 버전이 없습니다(" + p.Old + " 없음)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 롤백을 거부합니다: %w", err)
	}
	// 현재 버전과 백업을 맞바꾼다. 롤백 후에도 다시 되돌릴 수 있게 하기 위함이다.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 이동 실패: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 설치 실패: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] 롤백 후 백업 정리 실패(실행에는 영향 없음): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup 은 롤백 가능한 이전 버전이 있는지 알려주며, 프런트엔드가 롤백 버튼을 표시할지 결정하는 데 쓴다.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
