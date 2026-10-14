package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel은 범용 Webhook 어댑터이다. 사용자가 URL, 메서드, 요청 헤더, JSON 템플릿을 직접 설정한다.
// 이 어댑터 덕분에 Slack / Mattermost / Discord / 자체 구축 시스템마다 구현을 따로 쓸 필요가 없다.
// 이 플랫폼들은 모두 설정 가능한 템플릿 하나로 감당할 수 있다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 범용 Webhook에는 공식 제한이 없다. 0을 반환하여 기본적으로 제한하지 않으며, 상대의 능력에 맞게 사용자가 직접 정한다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url과 headers를 마스킹한다. 전달 주소 자체에 토큰이 들어 있는 경우가 많고, 사용자 정의 헤더에는 보통 인증 자격 증명이 들어 있다.
// 둘 다 API 응답에 나타날 수 있으므로 모두 막아야 한다.
// 대가로, 편집 시 그중 한 헤더를 바꾸려면 헤더 묶음 전체를 다시 입력해야 한다(마스킹 값은 「원래 값 유지」로 해석된다).
// 이 절충은 의도적이다. 여러 번 입력하는 편이 낫고, 자격 증명을 브라우저에 되돌려주지 않는다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 목적지는 url이다. url을 바꿀 때는 headers에 대해 반드시 다시 입장을 밝혀야 한다. 그렇지 않으면 원래의 Authorization 헤더가
// 새 주소로 그대로 전송되며, 이것이 바로 마스킹을 우회하는 주된 경로이다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate은 템플릿을 입력하지 않았을 때 쓰는 기본 요청 본문이다. 단순한 JSON 구조이며,
// 「JSON을 받아 저장하는」 대부분의 자체 구축 수신 측을 감당할 수 있다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData는 사용자 템플릿에 노출되는 컨텍스트이다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt은 이번 전달 시각(RFC3339)이며, 수신 측이 기록하는 데 쓴다.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel은 상태 변경의 읽기 쉬운 설명이다(예: 「대기 → 수정됨」). 상태 변경이 아니면 비어 있다.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL이 없습니다")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("대상 URL이 올바르지 않습니다: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드입니다: %s (GET/POST/PUT/PATCH 중에서 선택하세요)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 문법이 올바르지 않습니다: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET은 요청 본문을 싣지 않는다. 내용을 query에 넣는 것은 템플릿의 능력 범위를 벗어나고 GET의 의미에도 맞지 않으며,
	// 그래서 GET은 「호출되면 훅을 트리거하는」 종류의 수신 측에만 적합하다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 템플릿이 렌더링한 결과는 문자열 형태의 JSON이며, 여기서 json.RawMessage로 바꿔 그대로 보낸다.
		// 이중 이스케이프를 피하기 위해서다. 그렇지 않으면 사용자가 정성껏 만든 구조가 JSON 문자열 안에 감싸여 버린다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿의 렌더링 결과가 올바른 JSON이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 덮어쓰기는 허용하되, headers 이후에 적용하여 명시적 설정이 우선하도록 보장한다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 범용 Webhook은 본문을 자르지 않는다(수신 측은 사용자 자신의 서비스이며, 크기는 body_template이 결정한다).
	// 따라서 배치 전체를 전달 완료로 센다.
	return len(m.Items), nil
}

// renderWebhookBody는 사용자 템플릿(또는 기본 템플릿)으로 요청 본문을 렌더링한다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 문법이 올바르지 않습니다: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿을 렌더링하지 못했습니다: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate는 템플릿을 파싱한다.
//
// missingkey=zero는 없는 map 키를 오류 대신 0값으로 렌더링하게 한다. 다만 이 파일의 컨텍스트는 구조체이므로,
// 주된 역할은 .Items가 비어 있을 때 range가 오류를 내지 않게 하는 것이다. 실제로 막아야 하는 것은 .Items가 nil인 경우이다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs는 템플릿에 노출되는 도우미 함수이다.
var webhookTemplateFuncs = template.FuncMap{
	// json은 임의의 값을 JSON으로 직렬화한다.
	//
	// 이 함수는 있으면 좋은 정도가 아니라 필수이다. 이것이 없으면 사용자는 {{.Title}}을 그대로 삽입할 수밖에 없고,
	// 취약점 제목에 따옴표나 줄바꿈이 하나만 있어도 요청 본문 전체가 올바른 JSON이 아니게 된다. 수신 측은 이를 거부하며,
	// 오류 메시지는 「JSON 파싱 실패」만 가리키므로 제목 안의 따옴표를 떠올리기 어렵다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons는 JSON 조각을 다른 JSON 문자열 값 안에 넣을 때 쓴다(문자열 이스케이프를 한 번 더 적용한다).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥쪽 따옴표는 제거한다. 따옴표를 붙일지는 호출자가 직접 정한다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
