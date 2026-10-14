"use client";

// LLM 재시도 설정의 공용 부품: 다섯 계층 재시도 각각의 「횟수 + 간격」.
//
// 다섯 계층은 안에서 바깥으로: 연결 수립(SDK) → 빈 응답(SDK) → 같은 provider 안전 구간 → 폴링 서킷 브레이커 → 의도 재실행.
// 앞의 세 계층은 엔드포인트에 종속되므로 모델 설정마다 전역 기본값을 덮어쓸 수 있고, 뒤의 두 계층은 프로세스 단위라 전역에 하나만 있다.
//
// 모든 입력은 같은 「비워 두면 = 설정 안 함」 의미를 따르며, 백엔드 db.RetryRule과 일치한다:
//   횟수   빈 값/0 = 내장 기본값 | -1 = 이 계층 재시도 끔 | >0 = 이 횟수 사용
//   간격   빈 값/0 = 이 계층 원래의 지수 백오프 | >0 = 대신 이 고정 밀리초 간격 사용

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

// 계층별 「횟수를 비웠을 때의 기본값」이며, 입력창 placeholder에만 쓴다. 그 밖의 문구(제목 / 트리거 / 설명 등)는
// i18n(llmPage.retryLayer.<계층>.*)을 거쳐 실행 시점의 언어로 렌더링한다.
export const RETRY_LAYERS = {
  connect: { defAttempts: 3 },
  empty: { defAttempts: 2 },
  stream: { defAttempts: 2 },
  breaker: { defAttempts: 3 },
  intent: { defAttempts: 2 },
} satisfies Record<string, { defAttempts: number }>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** 밀리초를 사람이 읽기 쉬운 단위로 바꾼다. 입력창 옆에 되짚어 보여주는 용도이며, 0을 세지 않도록 하기 위함이다. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 제어형 숫자 입력: 빈 문자열 ↔ 0 변환, 중간 상태("-", "1e")는 로컬에 그대로 두고 부모에게 알리지 않는다. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 부모가 값 세트 전체를 바꾸면(정책을 읽어왔을 때, 설정을 전환했을 때) 따라간다. 직접 타이핑할 때는 여기에 오지 않는데,
  // 그때는 value가 이미 로컬 텍스트를 parse한 결과와 같기 때문이다.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 한 계층 재시도의 두 가지 설정값. idPrefix는 같은 페이지에 여러 번 나타날 때 label의 htmlFor를 유지하기 위해 쓴다. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 설정 드로어 안의 compact 버전: 펼친 설명은 생략하고 「어떤 에러가 이 계층까지 오는지」 한 문장만 남긴다 */
  compact?: boolean;
}) {
  const t = useTranslations("llmPage");
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{t(`retryLayer.${layer}.title`)}</Label>
          <span className="text-muted-foreground text-xs">{t(`retryLayer.${layer}.where`)}</span>
        </div>
        {/* 어떤 에러가 이 계층까지 오는지 상태 코드 단위로 적는다. 설정값을 넣었는데 효과가 없다면, 대개 에러가 애초에 이 계층에 걸리지 않기 때문이다. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">{t("retry.triggerLabel")}</span>:{" "}
          {t(`retryLayer.${layer}.trigger`)}
        </p>
        {!compact && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">{t("retry.skipsLabel")}</span>:{" "}
            {t(`retryLayer.${layer}.skips`)}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{t(`retryLayer.${layer}.desc`)}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {t(`retryLayer.${layer}.attemptsLabel`)}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={t("retry.defaultN", { n: meta.defAttempts })}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            {t("retry.intervalMs")}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder={t("retry.defaultBackoff")}
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">
            {human ? t("retry.fixed", { human }) : t(`retryLayer.${layer}.defInterval`)}
          </span>
        </div>
      </div>
      {!compact && (
        <p className="text-muted-foreground text-xs">
          {t("retry.emptyLine", { hint: t(`retryLayer.${layer}.offHint`) })}
        </p>
      )}
    </div>
  );
}

/** 모델 설정 드로어의 세 계층 덮어쓰기 설정(엔드포인트에 종속된 세 계층). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  const t = useTranslations("llmPage");
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">{t("retry.override.label")}</Label>
        <p className="text-muted-foreground text-xs">{t("retry.override.hint")}</p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「재시도와 백오프」 탭: 다섯 계층의 전역 기본값. */
export function RetryPolicyPanel() {
  const t = useTranslations("llmPage");
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(t("retry.toast.loadFailed", { msg: (e as Error).message }));
    } finally {
      setLoading(false);
    }
  }, [t]);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // 백엔드가 범위를 벗어난 값을 구간 안으로 잘라서 돌려주므로, 돌려받은 값으로 바로 갱신한다. 보이는 것이 곧 저장되는 것이다.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success(t("retry.toast.saved"));
    } catch (e) {
      toast.error(t("retry.toast.saveFailed", { msg: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> {t("retry.loading")}
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        {t.rich("retry.intro", {
          chain: (c) => <span className="text-foreground">{c}</span>,
          emph: (c) => <span className="text-foreground">{c}</span>,
        })}
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          {t("retry.save")}
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          {t("retry.resetAll")}
        </Button>
      </div>
    </div>
  );
}
