// 채널 필드 테이블과 설정 값 파싱 유틸리티.
//
// 페이지와 분리한 이유는 이것이 **뷰가 아니라 데이터**이기 때문이다: 채널마다 어떤 필드가 있고
// 각각 어떤 컨트롤을 써야 하는지, 그리고 폼 텍스트와 설정 값(JSON) 사이의 양방향 변환을 기술한다.
// 이 파일을 따로 두면 새 채널을 추가할 때 여기만 고치면 되고, 페이지 자체는 손댈 필요가 없다.
//
// 이 파일은 순수 데이터 모듈(client component 아님)이라 useTranslations를 호출할 수 없다.
// 그래서 문구는 여기에 하드코딩하지 않고 i18n 키(notifyPage 네임스페이스)로 저장하며,
// 이를 렌더링하는 컴포넌트(page.tsx / channel-form.tsx)가 t()로 해석한다. 키는 ASCII이고,
// 중국어 원문은 messages/zh.json에 그대로 남아 있다. 채널 타입 표시명은 notifyPage.kind.<kind>를 쓴다.

// 채널별 설정 필드 정의.
//
// 백엔드가 schema를 내려주지 않고 프론트엔드 필드 테이블을 따로 두는 것은 의도된 설계다. 백엔드는
// Validate(필수/형식)만 담당하고, UI에는 레이아웃과 컨트롤 타입이 필요하다. 둘은 관심사가 다르다.
// 유일한 결합점은 secret_keys다. 어떤 필드를 비밀번호 입력창으로 렌더링할지는 백엔드가 알려준다.
// 어떤 값이 자격 증명인지는 채널 구현체만 알기 때문이다(기업용 위챗 WeCom은 Webhook 전체가 자격 증명이고,
// 딩톡 DingTalk은 그중 secret 하나일 뿐이다). 새 채널을 추가할 때 여기 항목이 빠지면 폼이 비어 보일 뿐
// 조용히 틀리지는 않는다(아래 hasFields가 알려준다).
//
// label / help / options[].label은 notifyPage 네임스페이스의 i18n 키를 저장한다.
// placeholder는 리터럴 값을 유지한다(URL, 예시이므로 번역하지 않음).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  /** notifyPage 네임스페이스의 i18n 키 */
  label: string;
  kind: FieldKind;
  placeholder?: string;
  /** notifyPage 네임스페이스의 i18n 키 */
  help?: string;
  /** options[].label도 i18n 키다 */
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "field.dingtalk.webhook.label",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "field.dingtalk.secret.label",
      kind: "password",
      help: "field.dingtalk.secret.help",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "field.feishu.webhook.label",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "field.feishu.secret.label", kind: "password", help: "field.feishu.secret.help" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "field.wecom.webhook.label",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    {
      key: "url",
      label: "field.webhook.url.label",
      kind: "text",
      placeholder: "https://your-endpoint.example.com/hook",
    },
    {
      key: "method",
      label: "field.webhook.method.label",
      kind: "select",
      options: [
        { value: "POST", label: "field.webhook.method.opt.POST" },
        { value: "PUT", label: "field.webhook.method.opt.PUT" },
        { value: "PATCH", label: "field.webhook.method.opt.PATCH" },
        { value: "GET", label: "field.webhook.method.opt.GET" },
      ],
    },
    { key: "headers", label: "field.webhook.headers.label", kind: "kv", help: "field.webhook.headers.help" },
    {
      key: "body_template",
      label: "field.webhook.body_template.label",
      kind: "textarea",
      help: "field.webhook.body_template.help",
    },
  ],
  telegram: [
    { key: "bot_token", label: "field.telegram.bot_token.label", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "field.telegram.chat_id.label", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "field.telegram.base_url.label",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "field.telegram.base_url.help",
    },
  ],
  email: [
    { key: "host", label: "field.email.host.label", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "field.email.port.label",
      kind: "number",
      placeholder: "587",
      help: "field.email.port.help",
    },
    { key: "username", label: "field.email.username.label", kind: "text" },
    { key: "password", label: "field.email.password.label", kind: "password" },
    { key: "from", label: "field.email.from.label", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "field.email.to.label", kind: "list", help: "field.email.to.help" },
    { key: "tls", label: "field.email.tls.label", kind: "switch", help: "field.email.tls.help" },
  ],
};

// value는 필터링에 쓰는 min_severity 값이고, label은 notifyPage.severityOpt.*의 i18n 키다.
export const SEVERITY_OPTIONS = [
  { value: "", label: "severityOpt.all" },
  { value: "low", label: "severityOpt.low" },
  { value: "medium", label: "severityOpt.medium" },
  { value: "high", label: "severityOpt.high" },
  { value: "critical", label: "severityOpt.critical" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV는 "줄마다 KEY=VALUE" 형식의 텍스트 영역을 파싱한다.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs는 쉼표/공백으로 구분된 id 목록을 파싱한다. 구분자에는 전각 쉼표 "，"도 유지한다:
// 사용자가 다른 곳에서 전각 쉼표가 섞인 목록을 붙여넣을 수 있으며, 이는 표시 문구가 아니라 입력 파싱 로직이다.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords는 줄/쉼표로 구분된 키워드 목록을 파싱한다(취약점 유형 이름에는 공백이 있을 수 있으므로 줄 또는 쉼표로 자른다).
// 같은 이유로 전각 쉼표 "，"도 구분자로 유지한다(입력 파싱이며 표시용이 아님).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
