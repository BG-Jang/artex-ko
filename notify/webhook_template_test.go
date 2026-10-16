package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 이 파일은 일반 Webhook 템플릿의 **능력 경계**를 고정한다.
//
// 이것이 패키지 전체에서 유일하게 '사용자가 제공한 문자열이 코드로 평가되는' 지점이므로, 무엇을 할 수 있고
// 무엇을 할 수 없는지 명확히 하고, 테스트로 그 성질을 고정해야 한다. 그렇지 않으면 나중에 누군가 템플릿 컨텍스트에
// 메서드를 추가하거나 FuncMap에 readFile을 넣어도, 능력 범위가 소리 없이 넓어지는데 diff는
// 무해한 작은 함수처럼 보이게 된다.

// TestTemplateContextHasNoMethods가 가장 중요하다.
//
// text/template는 exported 메서드를 호출할 수 있다({{.Foo}}는 필드도 가져오고 메서드도 호출할 수 있다). 따라서 템플릿 컨텍스트가
// exported 메서드를 가진 **어떤** 타입에라도 닿기만 하면, 그 메서드들이 템플릿 작성자에게 노출된다.
// 이 기능의 컨텍스트는 의도적으로 순수 데이터만 사용한다(exported 필드만 있고 메서드는 없음).
//
// 이 단언이 실패하면 누군가 webhookTemplateData / webhookItem에 메서드를 추가했다는 뜻이다.
// 허용하기 전에, 그 메서드를 템플릿이 노출하고 싶지 않은 무언가를 읽는 데 쓸 수 있는지 먼저 따져 보라.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s 暴露了 %d 个方法（%s）：text/template 可以调用它们，"+
				"等于把这些方法的能力开放给了模板作者", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal은 템플릿에 노출되는 함수 집합을 고정한다.
//
// FuncMap에 함수가 하나 추가될 때마다 능력도 하나 늘어난다. 현재는 json / jsons뿐이며, 그 역할은 값을 JSON 조각으로
// 직렬화하는 것이다. 파일을 읽거나, 요청을 보내거나, 명령을 실행할 수 없다.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("模板函数集合变了：得到 %v，期望 %v。新增函数前请确认它不会扩大能力面"+
			"（不能读写文件、不能发起网络请求、不能执行命令）", got, want)
	}
}

// TestTemplateCannotReachUnknownData는 템플릿의 범위 밖 접근을 다룬다:
// 존재하지 않는 것에 접근하면 반드시 실패해야 하며, 무언가를 에코해서는 안 된다. 실패 메시지도 내부 데이터를 드러내면 안 된다.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("访问不存在的字段应报错")
	}
	// 오류에는 템플릿 컨텍스트의 실제 내용(취약점 제목/요약)이 나타나면 안 된다.
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("模板错误泄露了消息内容 %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: 템플릿 작성 오류는 설정 오류에 해당하며, 재시도로 자동 복구되지 않는다.
// 재시도 가능으로 판정되면, 잘못된 템플릿 하나 때문에 모든 발송이 세 라운드의 백오프를 헛되이 반복하게 된다.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("模板语法错误应在保存时就被拦下")
	}
	// 검증을 우회해 직접 발송하더라도, 반복 재시도가 아니라 영구 실패로 판정해야 한다.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("坏模板应判永久失败，得到 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON은 '템플릿 렌더링 결과는 반드시 유효한 JSON이어야 한다'는 제약을 다룬다.
// 이는 '템플릿으로 순수 텍스트를 생성해서 다른 프로토콜을 유발하는' 용법도 함께 차단한다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 유효한 템플릿은 통과해야 한다.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("合法模板应通过校验: %v", err)
	}
	// JSON이 아닌 결과를 렌더링하면 (그대로 보내는 대신) 반드시 거부해야 한다.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("渲染出非 JSON 应判永久失败，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "올바른 JSON") {
		t.Errorf("错误信息应说明是 JSON 问题，得到 %v", err)
	}
}
