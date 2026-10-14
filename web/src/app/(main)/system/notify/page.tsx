"use client";

import * as React from "react";

import { BellIcon, PlusIcon, SendIcon, Trash2Icon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { NotificationChannel, NotificationFilter, NotificationMeta } from "@/lib/types";

import {
  CHANNEL_FIELDS,
  type ChannelForm,
  emptyForm,
  parseIDs,
  parseKeywords,
  parseKV,
  SEVERITY_OPTIONS,
} from "./_components/channel-fields";
import { ConfigField, FilterSummary } from "./_components/channel-form";
import { DeliveryList } from "./_components/delivery-list";
import { formatBacklog, StatTile } from "./_components/stat-tile";

// 이 페이지는 오케스트레이션만 담당한다: 데이터 로드, 폼 상태 관리, API 호출.
// 필드 정의와 파싱은 _components/channel-fields.ts, 컨트롤과 필터 요약은
// _components/channel-form.tsx, 전달 기록은 _components/delivery-list.tsx에 있다 —
// 나눈 이유는 각각 단독으로 읽을 수 있게 하기 위해서이며, 한 파일에 몰아 두면 이 페이지가 1100줄 가까이 되기 때문이다.
export default function NotifyPage() {
  const t = useTranslations("notifyPage");
  // 채널 타입 표시명: 알려진 6종은 i18n(notifyPage.kind.<kind>)으로 표시하고,
  // 백엔드가 알 수 없는 타입을 주면 그 key를 그대로 표시한다(기존 `?? ch.kind` 의미 유지).
  const kindLabel = (k: string) => (k in CHANNEL_FIELDS ? t(`kind.${k}`) : k);
  const [meta, setMeta] = React.useState<NotificationMeta | null>(null);
  const [channels, setChannels] = React.useState<NotificationChannel[]>([]);
  const [tab, setTab] = React.useState<"channels" | "deliveries">("channels");

  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<NotificationChannel | null>(null);
  const [form, setForm] = React.useState<ChannelForm>(emptyForm("dingtalk"));
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  const [globalSaving, setGlobalSaving] = React.useState(false);
  const [baseURL, setBaseURL] = React.useState("");
  const [digestMin, setDigestMin] = React.useState("");

  const load = React.useCallback(() => {
    api
      .notifyMeta()
      .then((m) => {
        setMeta(m);
        setBaseURL(m.public_base_url);
        setDigestMin(m.digest_interval_min);
      })
      .catch((e) => toast.error(t("toast.loadConfigFailed", { msg: (e as Error).message })));
    // 채널 목록 로드 실패는 반드시 알려야 한다: 조용히 실패하면 「채널이 하나도 없음」으로 보여서,
    // 사용자는 설정이 사라진 줄 알게 된다. 직접 에러를 띄우는 것보다 훨씬 더 사용자를 불안하게 만든다.
    api
      .notifyChannels()
      .then(setChannels)
      .catch((e) => toast.error(t("toast.loadChannelsFailed", { msg: (e as Error).message })));
  }, [t]);
  React.useEffect(() => {
    load();
  }, [load]);

  function setF(patch: Partial<ChannelForm>) {
    setForm((f) => ({ ...f, ...patch }));
  }
  function setCfg(key: string, value: unknown) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function openAdd() {
    setEditing(null);
    setForm(emptyForm(meta?.kinds[0]?.kind ?? "dingtalk"));
    setOpen(true);
  }

  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    // filter는 백엔드에서 Go 구조체이므로 항상 객체로 직렬화된다(null이 되지 않으므로 기본값 처리가 필요 없다).
    const f = ch.filter;
    setForm({
      name: ch.name,
      kind: ch.kind,
      mode: ch.mode,
      enabled: ch.enabled,
      ratePerMin: String(ch.rate_per_min),
      // 백엔드가 돌려준 config의 자격 증명은 마스킹 값이다. 그대로 폼에 넣고, 제출할 때 그대로 돌려보내면
      // 백엔드가 이를 보고 DB의 원래 값을 유지한다.
      config: { ...ch.config },
      minSeverity: f.min_severity ?? "",
      includeText: (f.vulnclass_include ?? []).join("\n"),
      excludeText: (f.vulnclass_exclude ?? []).join("\n"),
      taskIDsText: (f.task_ids ?? []).join(","),
      assetIDsText: (f.asset_ids ?? []).join(","),
      onStatusChange: f.on_status_change ?? false,
    });
    setOpen(true);
  }

  // buildConfig는 폼 상태를 채널 config로 변환한다.
  //
  // 규칙은 하나이며, 값은 두 종류다:
  //   - 마스킹 값("__masked__...")은 그대로 돌려보냄 → 백엔드는 「이 필드는 바뀌지 않았으니 DB의 원래 값을 유지」로 해석
  //   - 그 밖의 값은 모두 사용자 입력으로 제출하며, 빈 문자열이면 「이 필드를 비움」을 뜻함
  //
  // 「자격 증명은 비워 두면 건너뜀」 같은 특별 처리를 하지 않는 이유는, 그러면 사용자가 잘못 설정한 키를
  // **지울 수 없게** 되기 때문이다. 화면에는 「이것을 삭제하겠다」를 표현할 방법이 없다. 현재 규칙에서는
  // 입력창을 비우는 것이 곧 해당 필드를 비우는 것이므로, 의미가 하나이고 사용자가 통제할 수 있다.
  // 마스킹 값은 입력창에 나타나지 않으므로(ConfigField 참고), 「입력창에 글자가 있음」은 항상
  // 「사용자가 직접 입력한 것」과 같다.
  function buildConfig(): Record<string, unknown> {
    const defs = CHANNEL_FIELDS[form.kind] ?? [];
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const raw = form.config[d.key];
      if (d.kind === "switch") {
        out[d.key] = raw === true;
        continue;
      }
      if (typeof raw === "string" && raw.startsWith("__masked__")) {
        out[d.key] = raw;
        continue;
      }
      if (d.kind === "number") {
        const n = Number(raw);
        out[d.key] = Number.isFinite(n) && n > 0 ? n : 0;
        continue;
      }
      if (d.kind === "kv") {
        out[d.key] = parseKV(String(raw ?? ""));
        continue;
      }
      if (d.kind === "list") {
        out[d.key] = String(raw ?? "")
          .split(/[\s,，]+/)
          .map((s) => s.trim())
          .filter(Boolean);
        continue;
      }
      out[d.key] = String(raw ?? "").trim();
    }
    return out;
  }

  function buildFilter(): NotificationFilter {
    return {
      min_severity: form.minSeverity || undefined,
      vulnclass_include: parseKeywords(form.includeText),
      vulnclass_exclude: parseKeywords(form.excludeText),
      task_ids: parseIDs(form.taskIDsText),
      asset_ids: parseIDs(form.assetIDsText),
      on_status_change: form.onStatusChange,
    };
  }

  async function saveForm() {
    if (!form.name.trim()) {
      toast.error(t("toast.nameRequired"));
      return;
    }
    setSaving(true);
    try {
      const payload = {
        name: form.name.trim(),
        kind: form.kind,
        mode: form.mode,
        enabled: form.enabled,
        config: buildConfig(),
        filter: buildFilter(),
        rate_per_min: form.ratePerMin.trim() === "" ? undefined : Number(form.ratePerMin),
      };
      if (editing) {
        await api.notifyUpdateChannel(editing.id, payload);
        toast.success(t("toast.saved"));
        setOpen(false);
      } else {
        await api.notifyCreateChannel(payload);
        toast.success(t("toast.channelAdded"));
        setOpen(false);
      }
      load();
    } catch (e) {
      toast.error(t("toast.saveFailed", { msg: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  async function testChannel() {
    if (!editing) return;
    setTesting(true);
    try {
      const r = await api.notifyTestChannel(editing.id);
      toast.success(t("toast.testSent", { ms: r.latency_ms }));
    } catch (e) {
      // 백엔드가 채널의 원본 에러를 그대로 돌려준다. 설정을 점검할 수 있는 유일한 단서이므로 가공 없이 그대로 보여준다.
      toast.error(t("toast.testFailed", { msg: (e as Error).message }), { duration: 12000 });
    } finally {
      setTesting(false);
    }
  }

  async function removeChannel(ch: NotificationChannel) {
    try {
      await api.notifyDeleteChannel(ch.id);
      toast.success(t("toast.deleted", { name: ch.name }));
      setOpen(false);
      load();
    } catch (e) {
      toast.error(t("toast.deleteFailed", { msg: (e as Error).message }));
    }
  }

  async function toggleEnabled(ch: NotificationChannel) {
    try {
      await api.notifyUpdateChannel(ch.id, { enabled: !ch.enabled });
      load();
    } catch (e) {
      toast.error(t("toast.actionFailed", { msg: (e as Error).message }));
    }
  }

  async function toggleGlobal(on: boolean) {
    setGlobalSaving(true);
    try {
      await api.setSettings({ notify_enabled: on });
      setMeta((m) => (m ? { ...m, enabled: on } : m));
      toast.success(on ? t("toast.globalOn") : t("toast.globalOff"));
    } catch (e) {
      toast.error(t("toast.actionFailed", { msg: (e as Error).message }));
    } finally {
      setGlobalSaving(false);
    }
  }

  async function saveGlobal() {
    setGlobalSaving(true);
    try {
      const patch: Record<string, unknown> = { notify_public_base_url: baseURL.trim() };
      const n = Number(digestMin);
      if (Number.isFinite(n) && n > 0) patch.notify_digest_interval_min = n;
      await api.setSettings(patch);
      toast.success(t("toast.saved"));
      load();
    } catch (e) {
      toast.error(t("toast.saveFailed", { msg: (e as Error).message }));
    } finally {
      setGlobalSaving(false);
    }
  }

  const fields = CHANNEL_FIELDS[form.kind] ?? [];
  const secretKeys = new Set(meta?.kinds.find((k) => k.kind === form.kind)?.secret_keys ?? []);
  const defaultRate = meta?.kinds.find((k) => k.kind === form.kind)?.default_rate_per_min ?? 0;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">{t("title")}</h1>
          <p className="text-muted-foreground text-sm">{t("subtitle")}</p>
        </div>
        {meta && (
          // div를 쓰고 label을 쓰지 않는 이유: Switch 자체에 aria-label이 있고, 바깥에 label을 한 겹 더 씌우면
          // 어떤 네이티브 컨트롤과도 연결되지 않으면서, 텍스트를 클릭하면 전환될 것처럼 보이게 된다.
          <div className="flex shrink-0 items-center gap-2 text-sm">
            <span className="text-muted-foreground">{t("globalSwitch")}</span>
            <Switch
              checked={meta.enabled}
              disabled={globalSaving}
              onCheckedChange={toggleGlobal}
              aria-label={t("aria.globalSwitch")}
            />
          </div>
        )}
      </div>

      {meta && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile
            label={t("stats.channels")}
            value={`${meta.stats.channels_on} / ${meta.stats.channels}`}
            hint={t("stats.channelsHint")}
          />
          <StatTile label={t("stats.sentToday")} value={String(meta.stats.sent_today)} />
          <StatTile label={t("stats.pending")} value={String(meta.stats.pending)} />
          <StatTile
            label={t("stats.failed")}
            value={String(meta.stats.failed)}
            tone={meta.stats.failed > 0 ? "red" : undefined}
          />
          <StatTile
            label={t("stats.backlog")}
            value={formatBacklog(meta.stats.backlog_age_ms, t)}
            // 적체 「나이」가 적체 「건수」보다 훨씬 유용하다: 3건이 쌓여 있어도 3초 전 것일 수도, 3시간 전 것일 수도 있다.
            hint={meta.stats.backlog_age_ms > 5 * 60_000 ? t("stats.backlogStuck") : undefined}
            tone={meta.stats.backlog_age_ms > 5 * 60_000 ? "red" : undefined}
          />
        </div>
      )}

      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">{t("global.title")}</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="n-base">{t("global.baseUrl")}</Label>
            <Input
              id="n-base"
              placeholder="https://artex.example.com"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">{t("global.baseUrlHint")}</p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="n-digest">{t("global.digest")}</Label>
            <Input
              id="n-digest"
              type="number"
              min={1}
              max={1440}
              placeholder="30"
              value={digestMin}
              onChange={(e) => setDigestMin(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">{t("global.digestHint")}</p>
          </div>
          <div className="sm:col-span-2">
            <Button onClick={saveGlobal} disabled={globalSaving}>
              {t("global.save")}
            </Button>
          </div>
        </CardContent>
      </Card>

      <Tabs value={tab} onValueChange={(v) => setTab(v as "channels" | "deliveries")} className="flex flex-col gap-4">
        <TabsList>
          <TabsTrigger value="channels">{t("tab.channels")}</TabsTrigger>
          <TabsTrigger value="deliveries">{t("tab.deliveries")}</TabsTrigger>
        </TabsList>

        <TabsContent value="channels">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <button
              type="button"
              onClick={openAdd}
              className="text-foreground/70 border-foreground/70 hover:bg-muted/60 hover:shadow-sm flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition"
            >
              <PlusIcon className="size-6" />
              <span className="text-sm">{t("addChannel")}</span>
            </button>

            {channels.map((ch) => (
              <Card
                key={ch.id}
                onClick={() => openEdit(ch)}
                className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
              >
                <CardHeader>
                  <div className="flex items-center gap-2">
                    <BellIcon className="text-muted-foreground size-4 shrink-0" />
                    <CardTitle className="truncate text-base">{ch.name}</CardTitle>
                    {/* 카드 전체가 클릭 가능하므로(편집 진입), 이 두 컨트롤은 각자 이벤트 버블링을 막아야 한다.
                        그렇지 않으면 토글/삭제가 편집도 함께 트리거한다. stopPropagation은 컨트롤 자신에 달고,
                        div로 한 겹 감싸지 않는다: div로 감싸면 「상호작용 가능해 보이지만 역할이 없는」 정적 요소가 생겨
                        a11y 경고를 유발하고 의미상으로도 맞지 않는다. */}
                    <div className="ml-auto flex items-center gap-2">
                      <Switch
                        checked={ch.enabled}
                        onCheckedChange={() => toggleEnabled(ch)}
                        onClick={(e) => e.stopPropagation()}
                        aria-label={t("aria.enable")}
                      />
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label={t("aria.delete")}
                        onClick={(e) => {
                          e.stopPropagation();
                          // void로 Promise를 명시적으로 버린다: removeChannel이 자체적으로 catch하고 toast를 띄우므로,
                          // 여기서는 await가 필요 없다(onClick은 async가 아니다).
                          void removeChannel(ch);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="grid gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{kindLabel(ch.kind)}</Badge>
                    <Badge variant="outline">{ch.mode === "digest" ? t("mode.digest") : t("mode.realtime")}</Badge>
                    {!ch.enabled && <Badge variant="outline">{t("disabled")}</Badge>}
                  </div>
                  <FilterSummary filter={ch.filter} />
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="deliveries">
          <DeliveryList channels={channels} />
        </TabsContent>
      </Tabs>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? editing.name : t("sheet.addTitle")}</SheetTitle>
            <SheetDescription>
              {kindLabel(form.kind)}
              {defaultRate > 0 ? t("sheet.rateLimited", { rate: defaultRate }) : t("sheet.rateUnlimited")}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label>{t("sheet.kindLabel")}</Label>
                <Select
                  value={form.kind}
                  onValueChange={(v) => {
                    // 타입을 바꾸면 자격 증명 필드 세트가 통째로 바뀌므로, 이전 설정을 병합해서 가져오면 안 된다.
                    setF({ kind: v, config: {} });
                  }}
                  disabled={!!editing}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(meta?.kinds ?? []).map((k) => (
                      <SelectItem key={k.kind} value={k.kind}>
                        {kindLabel(k.kind)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {editing && <p className="text-muted-foreground text-xs">{t("sheet.kindLocked")}</p>}
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-name">{t("sheet.nameLabel")}</Label>
                <Input
                  id="n-name"
                  placeholder={t("sheet.namePlaceholder")}
                  value={form.name}
                  onChange={(e) => setF({ name: e.target.value })}
                />
              </div>

              {fields.length === 0 ? (
                <p className="text-muted-foreground text-sm">{t("sheet.noFields")}</p>
              ) : (
                fields.map((d) => (
                  <ConfigField
                    key={d.key}
                    def={d}
                    value={form.config[d.key]}
                    isSecret={secretKeys.has(d.key)}
                    onChange={(v) => setCfg(d.key, v)}
                  />
                ))
              )}

              <div className="grid gap-2">
                <Label>{t("sheet.modeLabel")}</Label>
                <Select value={form.mode} onValueChange={(v) => setF({ mode: v as "realtime" | "digest" })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="realtime">{t("sheet.modeRealtime")}</SelectItem>
                    <SelectItem value="digest">{t("sheet.modeDigest")}</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">{t("sheet.modeHint")}</p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-rate">{t("sheet.rateLabel")}</Label>
                <Input
                  id="n-rate"
                  type="number"
                  min={0}
                  placeholder={defaultRate > 0 ? String(defaultRate) : t("sheet.ratePlaceholder")}
                  value={form.ratePerMin}
                  onChange={(e) => setF({ ratePerMin: e.target.value })}
                />
                <p className="text-muted-foreground text-xs">{t("sheet.rateHint")}</p>
              </div>

              <div className="border-t pt-4">
                <p className="mb-3 text-sm font-medium">{t("filter.title")}</p>
                <div className="grid gap-4">
                  <div className="grid gap-2">
                    <Label>{t("filter.minSeverity")}</Label>
                    <Select
                      value={form.minSeverity || "all"}
                      onValueChange={(v) => setF({ minSeverity: v === "all" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITY_OPTIONS.map((o) => (
                          <SelectItem key={o.value || "all"} value={o.value || "all"}>
                            {t(o.label)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-inc">{t("filter.includeLabel")}</Label>
                    <Textarea
                      id="n-inc"
                      placeholder={t("filter.includePlaceholder")}
                      value={form.includeText}
                      onChange={(e) => setF({ includeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">{t("filter.includeHint")}</p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-exc">{t("filter.excludeLabel")}</Label>
                    <Textarea
                      id="n-exc"
                      placeholder={t("filter.excludePlaceholder")}
                      value={form.excludeText}
                      onChange={(e) => setF({ excludeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">{t("filter.excludeHint")}</p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-tasks">{t("filter.taskIds")}</Label>
                    <Input
                      id="n-tasks"
                      placeholder="1, 2, 3"
                      value={form.taskIDsText}
                      onChange={(e) => setF({ taskIDsText: e.target.value })}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-assets">{t("filter.assetIds")}</Label>
                    <Input
                      id="n-assets"
                      placeholder="10, 11"
                      value={form.assetIDsText}
                      onChange={(e) => setF({ assetIDsText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">{t("filter.idsHint")}</p>
                  </div>
                  <div className="flex items-center gap-2 text-sm">
                    <Switch
                      checked={form.onStatusChange}
                      onCheckedChange={(v) => setF({ onStatusChange: v })}
                      aria-label={t("aria.statusChange")}
                    />
                    {t("filter.onStatusChange")}
                  </div>
                </div>
              </div>

              <div className="flex items-center gap-2 text-sm">
                <Switch
                  checked={form.enabled}
                  onCheckedChange={(v) => setF({ enabled: v })}
                  aria-label={t("aria.enable")}
                />
                {t("sheet.enable")}
              </div>
            </div>

            <div className="flex gap-2 pt-2 pb-6">
              <Button onClick={saveForm} disabled={saving}>
                {editing ? t("sheet.save") : t("sheet.add")}
              </Button>
              {editing && (
                <Button variant="outline" onClick={testChannel} disabled={testing}>
                  <SendIcon /> {t("sheet.sendTest")}
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
