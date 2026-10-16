import type { useTranslations } from "next-intl";

import { Card, CardContent } from "@/components/ui/card";

// 이 파일은 client 페이지에서 참조되지만 자체는 순수 표시용/순수 함수이며, useTranslations를 직접 호출하지 않는다.
// formatBacklog는 i18n 단위 문구가 필요하므로 호출자가 t를 주입한다(tasks/approval-records 선례).
type Translator = ReturnType<typeof useTranslations>;

export function StatTile({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: string }) {
  return (
    <Card size="sm" className="gap-1">
      <CardContent>
        <p className="text-muted-foreground text-xs">{label}</p>
        <p className={`text-lg font-semibold ${tone === "red" ? "text-rose-600" : ""}`}>{value}</p>
        {hint && <p className={`text-xs ${tone === "red" ? "text-rose-600" : "text-muted-foreground"}`}>{hint}</p>}
      </CardContent>
    </Card>
  );
}

// formatBacklog는 적체 밀리초를 사람이 알아보기 쉬운 단위로 렌더링한다.
export function formatBacklog(ms: number, t: Translator): string {
  if (!ms) return "—";
  if (ms < 60_000) return t("unit.sec", { n: Math.round(ms / 1000) });
  if (ms < 3_600_000) return t("unit.min", { n: Math.round(ms / 60_000) });
  return t("unit.hour", { n: (ms / 3_600_000).toFixed(1) });
}
