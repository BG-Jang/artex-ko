package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("掩码值泄露了完整凭据: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("掩码值不应暴露地址主体: %q", got)
	}
	// 끝 6자리는 유지해야 사용자가 어떤 봇인지 알아볼 수 있다.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("应保留末 6 位作为辨识提示: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("掩码值必须能被 IsMasked 识别: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 짧은 자격 증명의 끝 6자리까지 노출하면, 사실상 자격 증명 전체가 노출되는 셈이다.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("长度 %d 的凭据不应给出尾部提示，得到 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("掩码值包含了原值: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s 应被掩码，得到 %q", k, s)
		}
	}
	// 비밀이 아닌 필드는 원본 그대로 보존해야 UI에 표시할 수 있다.
	if masked["port"] != float64(587) {
		t.Errorf("非凭据字段 port 不应改动: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// 채널 종류를 식별할 수 없을 때는, 자격 증명이 들어 있을 수 있는 원본을 되돌려주기보다 UI에 빈 설정을 보여주는 편이 낫다.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("未知渠道类型应返回空配置，得到 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// 마스킹은 표시 계층의 동작이며, 이를 이용해 DB에 있는 실제 값을 바꿔서는 안 된다.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig 修改了入参，会导致真实凭据被掩码值覆盖")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// 사용자가 method만 변경하면, 브라우저는 마스킹된 값 + 새 method를 제출한다.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("掩码字段应保留库中原值，得到 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("被修改的字段应生效，得到 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("空串应清空该字段，得到 %v", got)
	}
	// 언급되지 않은 필드는 보존한다 (부분 업데이트 의미).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("未提及的字段应保留，得到 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("未提及的字段应保留，得到 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("已提及的字段应更新，得到 %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap은 이 패키지에서 가장 중요한 보안 불변식을 검증한다:
// **발송 주소를 바꿀 때 기존 자격 증명을 함께 넘겨서는 안 된다.**
//
// 이 케이스들은 공격 형태의 입력(주소만 바꾸고 자격 증명은 언급하지 않음)을 사용한다.
// 이는 '방어 로직의 올바른 입력'이 아니다. 후자만 테스트하면 방어가 동작하지 않아도 전부 통과한다.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing은 누락됐다고 지목되어야 하는 자격 증명 키다.
		wantMissing string
	}{
		{
			name: "通用 Webhook 改地址想沿用 Authorization 头",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram 改 base_url 想把 Bot Token 发到自己的端点",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "邮件改 SMTP 主机想交出密码",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "邮件关掉 TLS 也必须重新表态密码",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// 마스킹 값 = '기존 자격 증명 재사용'이므로, 주소가 변경된 상황에서도 반드시 거부해야 한다.
			name:        "回传掩码凭据 + 新地址",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "钉钉改 Webhook 想沿用加签密钥",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("改地址却未重新表态凭据，应当被拒绝；得到配置 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("应返回专门的错误类型以便接口给出可操作提示，得到 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("应点名缺失的凭据键 %q，得到 %v", tc.wantMissing, target.Missing)
			}
			// 오류 메시지는 운영자가 어떻게 고쳐야 할지 안내할 수 있어야 한다.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("错误信息应提到 %q: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits는 반대 방향의 케이스다: 정상적인 수정이 잘못 차단되면 안 된다.
// 그렇지 않으면 이 방어 로직이 '너무 귀찮다'는 이유로 우회되거나 삭제될 수 있다.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "只改名字（配置原样回传）",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "只改请求方法，地址与凭据都不动",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "换地址并**同时**给新凭据",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "换地址并显式声明不再需要凭据",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram 改 chat_id（不是目的地）",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "邮件改收件人（不是目的地）",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("合法编辑被误拦: %v", err)
			}
			if merged == nil {
				t.Fatal("应返回合并结果")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance는 오판하기 쉬운 세부 사항을 검증한다:
// 프런트엔드가 제출하는 포트는 JSON number(float64)이고, DB에서 다시 읽어 와도 float64지만,
// 두 값의 타입이 다를 수 있다 (예: int vs float64). == 로 비교하면 '변경 없음'을 '변경됨'으로 잘못 판정하여,
// 이름만 바꾼 사용자에게 '비밀번호를 다시 입력하세요'라고 띄우게 된다. 잦은 거짓 경고는 이 방어 로직에 대한 신뢰를 떨어뜨린다.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 같은 포트를 int 형태로 제출한 경우.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("端口值相同（仅类型不同）不应被判为地址变更: %v", err)
	}
	// 실제로 포트가 바뀐 경우에는 반드시 차단해야 한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("端口变更应被拦下")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination은 '비워 둘 수 있는 대상 필드' 경로를 검증한다:
// Telegram의 base_url을 비우면 공식 API 주소를 사용한다는 뜻이다.
//
// 과거에는 여기서 두 번째 저장부터 채널이 영구적으로 저장에 실패했다:
//
//	새로 만들 때 DB에 base_url:""가 저장됨 (생성 경로는 프런트엔드가 제출한 config를 MergeConfig를 거치지 않고 그대로 저장)
//	→ 첫 번째 저장 시 MergeConfig가 빈 문자열을 명시적 비움으로 보고 해당 키를 delete함
//	→ 두 번째 저장 시 incoming은 여전히 ""인데 stored에는 이미 해당 키가 없어서 '주소 변경됨'으로 판정됨
//	→ bot_token은 마스킹된 에코 값 → 400 '대상 주소가 변경되었습니다. 자격 증명 필드도 다시 입력하세요'
//
// 사용자는 아무것도 바꾸지 않았는데, Bot Token을 다시 붙여넣지 않는 한 영원히 저장할 수 없게 된다.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// 프런트엔드 buildConfig()는 이 채널의 모든 필드 정의에 대해 값을 제출한다: 자격 증명은 마스킹 값으로 채워지고,
	// 빈 입력란은 빈 문자열을 제출한다. 여기서는 '변경된 키만 제출'이 아니라 그 출력을 그대로 재현한다.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 첫 번째 저장: 채널 이름만 바꾸고, config는 그대로 되돌려 보낸다.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("第一次保存被误拦: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("前提已变：空串应被 MergeConfig 删除——本用例要覆盖的正是「键消失之后」那一步")
	}

	// 두 번째 저장: 제출 내용이 이전과 완전히 동일하다 (사용자는 아무것도 바꾸지 않음).
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("第二次保存被误拦（用户什么都没改）: %v", err)
	}
	// 세 번째 저장: '한 번만 틀렸던 것'이 아니라 안정적으로 저장 가능한지 확인한다.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("第三次保存被误拦: %v", err)
	}
	// 자격 증명은 끝까지 보존되어야 하며, 빈 문자열 로직에 의해 함께 지워져서는 안 된다.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token 应沿用原值，得到 %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges는 위 케이스와 짝을 이루는 단언이다:
// 빈 문자열과 '키가 없음'을 동일하게 취급하더라도, 실제 주소 변경은 **절대 통과시켜서는 안 된다**.
// 이 두 방향 모두 실제 자격 증명 외부 전송 경로다. Telegram의 Bot Token은 URL 경로에 들어가므로,
// base_url을 바꾸면 곧 Token을 새 주소로 보내는 것과 같다.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 방향 1: '비어 있음'(공식 주소)에서 자체 구축 주소로 변경.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("从官方地址换到自建地址必须要求重新填写 Token")
	}

	// 방향 2: 자체 구축 주소를 비움(= 공식 API로 되돌림) 역시 주소 변경이다.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("清空自建地址（换回官方 API）同样是地址变更，必须要求重新填写 Token")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys와 같은 이유: 채널이 대상 키를 선언하지 않으면 PrepareConfigUpdate가 이를 보호하지 못한다.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("渠道 %s 未声明目的地键，改地址带出凭据的防护对它无效", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("渠道 %s 未声明凭据键", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// 컴파일러가 모든 채널에 SecretKeys 구현을 강제하지만, 여기서 한 번 더 확인한다: 마스킹 결과가 비어 있는 채널이 없어야 한다.
	// 빈 슬라이스를 반환하는 채널은 자격 증명이 브라우저에 평문으로 그대로 노출된다는 뜻이다.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("渠道 %s 未在测试中登记掩码预期", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("渠道 %s 未声明任何凭据字段，其配置会明文回显", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer는 감사에서 지적된 허점을 검증한다:
// 마스킹 센티넬을 **문자열이 아닌** 구조(예: webhook.headers는 객체)에 넣으면,
// MergeConfig는 '문자열이면서 접두사가 있는 경우'만 마스크로 인식한다. 따라서 리터럴 "__masked__"가 실제 헤더 값으로 DB에 저장되고,
// 이후 인증이 조용히 실패하지만 아무 오류도 나지 않는다.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// 객체 내부에 마스킹 센티넬을 끼워 넣은 경우.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("结构体内部夹带掩码哨兵应被拒绝（否则会把字面量存进库）")
	}
	// 객체 전체를 (실제 새 값으로) 제출하는 경우는 정상적으로 수락한다.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("正常提交新请求头不应被拦: %v", err)
	}
}
