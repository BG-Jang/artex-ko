package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa 는 norma v0.4.0 에서 도입된 「모델 기반 컨텍스트 압축」 메커니즘으로, 플랫폼 실험 기능으로
// 사용자가 시스템 설정에서 켜고 끈다. 내장 compaction 과 상호 배타적이다: noaadapter.Enable 이 유일한
// 진입점이며, 한 번 호출로 컨텍스트 인수기(Compactor), Compress 도구, 상시 프롬프트 세 조각을 함께 붙인다.
// Enable 을 호출하지 않으면 꺼진 상태이며(내장 compaction 이 그대로 동작한다), 스위치는 각 agent 가
// 주입한 noaEnabledFn 으로 해석된다. run 마다 한 번씩 읽으므로, 전환은 이후에 시작되는 run 에만 영향을 주고
// agent 를 다시 만들 필요가 없다.

// enableNoa 는 resolver 가 켜짐을 보고하면 noa 를 opts 에 연결한다. archiveRoot 는 압축 원문을
// 영구 보관하는 기본 디렉터리다(전역 workDir 을 사용하며, 모든 agent 가 <workDir>/noa 아래로 통일하고
// 작업/의도 디렉터리에 흩어지지 않는다). sessionID 는 그 아래 아카이브 하위 디렉터리 이름이 된다(전역 유일하므로
// 같은 기본 디렉터리 안에서 충돌하지 않는다).
//
// noa 는 실험 기능이다: 연결에 실패해도 실제 작업을 중단하면 안 된다. 오류가 나면 onWarn 으로 알리고 내장 압축으로 되돌아간다.
// 활성화에 성공하면 opts.Compaction 을 비워, agentcore 가 「두 개의 컨텍스트 관리자가 동시에 설정됨」 경고를 내지 않도록 한다.
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 압축 활성화 실패, 내장 압축으로 되돌림: " + err.Error())
		}
		return
	}
	// Compactor 가 Compaction 을 덮어쓰지만, 둘이 함께 있으면 agentcore 가 매번 경고하므로 명시적으로 비운다.
	opts.Compaction = nil
}
