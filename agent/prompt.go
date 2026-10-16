package agent

import (
	"bytes"
	"fmt"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem 은 agentKey 에 대해 렌더링된 시스템 프롬프트 본문(段 [A])을 반환한다.
// 우선순위: DB 에 저장된 템플릿(있는 경우)이 내장 기본 템플릿(def)보다 앞선다. 지금은 둘 다 Go 템플릿이다 ——
// 내장 기본값은 DB 에 그대로 시드되므로, 사용자가 프롬프트를 편집하기 전까지 두 경로의 렌더링 결과는 같다.
// 렌더링은 항상 수행한다(def 는 예전에는 미리 치환된 평문이었으나, 지금은 DB 템플릿과 같은 {{.Var}} 템플릿이다).
// 렌더링 오류가 나면 기본 템플릿으로, 그래도 실패하면 원본 기본 문자열로 떨어진다 —— agent 는 절대 반쯤 렌더링된
// 프롬프트로 시작하지 않는다. 호출자는 이 결과 뒤에 코드가 소유한 꼬리(trafficTool / 中间产物输出规约)를 덧붙이므로,
// 이 부분을 DB 본문에서 편집해 없앨 수 없다.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

// langDirective 는 출력 언어 꼬리 부분이다. 코드가 소유하는 세그먼트로, 모든 사용자 대면 agent 역할에서
// 렌더링된 본문과 artifact/traffic 꼬리 뒤에 붙는다. 따라서 DB 에서 편집된 프롬프트 본문이 이것을 떨어뜨릴 수 없다 ——
// artifactSpec 이 주는 보장과 같다. 에이전트의 「두뇌」(벤치마크된 중국어 추론 본문 段 [A])은 번역하지 않는다.
// 에이전트가 사용자에게 「보여 주는」 언어만 제한한다. 지시문을 중국어로 쓰는 이유는 본문과 같은 언어로 두어
// 모델의 추론 어조를 안정시키면서 선택한 표시 언어를 강제하기 위해서다 —— 이것이 현지화 방식이다:
// 동작은 보존하고, 사용자가 읽는 표면만 현지화한다. 원문 기술 문자열(명령, payload, 코드, URL, 로그/응답 발췌)은
// 증거와 재현 단계가 번역으로 훼손되지 않도록 명시적으로 그대로 보존한다.
//
// 종단 간 실행(L1) 이후 두 가지 드리프트 방지 절이 추가되었다: 실시간 모델이 (1) 계획자의 상황 요약에 중국어를
// 섞었고(중국어 두뇌 본문 段 [A] 를 그대로 따라 함), (2) 대상 앱/증거의 언어를 report_finding 의 구조화 필드에
// 섞었다. 지시문은 계획자 상황 요약을 사용자 대면 필드로 명시하고, 표시 필드에서 중국어 지시 언어와
// 대상/자료 언어를 모두 미러링하지 못하도록 금지한다.
//
// 표시 언어는 매개변수다(code = "en"/"ko"/"zh"/"es"). 지시문 자체는 중국어로 두되, "<language> 로 쓰라"는 토큰은
// 대상마다 바꾼다. 표시 언어가 중국어("zh")일 때는 중국어 출력이 목표이므로, 「사용자에게 중국어를 내보내지 말라」는
// 절 대신 중국어 표시를 그대로 긍정하는 절을 쓰되, 대상/자료 언어를 미러링하는 것은 여전히 금지한다.
func langDirective(code string) string {
	phrase := langPhrase[code]
	bare := langBare[code]
	if phrase == "" || bare == "" {
		phrase, bare = langPhrase[defaultOutputLang], langBare[defaultOutputLang]
	}
	if code == "zh" {
		// 표시 언어 자체가 중국어이므로 "중국어를 사용자에게 내보내지 말라"는 절은 없다.
		// 대신 중국어 표시를 확정하고, 대상/자료 언어 미러링 금지와 원문 보존은 유지한다.
		return fmt.Sprintf("\n\n**输出语言规约（本地化·最高优先级，不可被提示词正文覆盖）**：所有【展示给用户】的自然语言文字一律用【%s】书写——包括 record_fact 的 summary/detail、report_finding 的标题/描述/结论/修复建议、规划者(planner)的态势/情况总结、最终那一句话总结、以及对用户的聊天回复。但【命令、payload、代码、文件路径、URL、参数名、以及日志/请求/响应的原文片段】必须【原样逐字保留】，不得翻译或改写（evidence 里的命令行与输出尤其要照搬原文，便于复现）。**即使目标系统、它的页面、证据、日志或任何参考资料是英文或别的语言，面向用户的自然语言字段（标题/描述/结论/修复建议/总结/态势总结）仍必须用%s书写——不要镜像或照抄目标或资料的语言来写这些展示字段；只有上面列出的原文技术片段才保持原样。** 一句话：内部推理不限，但凡落到用户能看到的正文，必须全是%s（技术原文片段除外）。", phrase, bare, bare)
	}
	return fmt.Sprintf("\n\n**输出语言规约（本地化·最高优先级，不可被提示词正文覆盖）**：所有【展示给用户】的自然语言文字一律用【%s】书写——包括 record_fact 的 summary/detail、report_finding 的标题/描述/结论/修复建议、规划者(planner)的态势/情况总结、最终那一句话总结、以及对用户的聊天回复。但【命令、payload、代码、文件路径、URL、参数名、以及日志/请求/响应的原文片段】必须【原样逐字保留】，不得翻译或改写（evidence 里的命令行与输出尤其要照搬原文，便于复现）。**即使上面的系统/角色指令本身是用中文写的，也绝不能把中文输出给用户——面向用户的展示语言只有%s，不要让任何中文句子出现在用户可见的文字里。** **即使目标系统、它的页面、证据、日志或任何参考资料是英文、中文或别的语言，面向用户的自然语言字段（标题/描述/结论/修复建议/总结/态势总结）仍必须用%s书写——不要镜像或照抄目标或资料的语言来写这些展示字段；只有上面列出的原文技术片段才保持原样。** **你的分析/规划/思考用中文进行没关系，但那是【不可见的内部推理】，绝不能作为正文输出：给用户的可见回复从第一个字起就必须是%s，不要在前面垫一段中文的思考、说明或「我先怎样怎样」的铺垫；连澄清提问、缺少参数、「无法继续」之类的说明也一律直接用%s写。** 一句话：内部怎么想不限，但凡落到用户能看到的正文，必须全是%s（技术原文片段除外）。", phrase, bare, bare, bare, bare, bare)
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
