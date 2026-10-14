"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { api } from "@/lib/api";

/**
 * 상단 바의 "새 버전 있음" 알림: 전체 페이지 로드 시 한 번 조회하며, 업데이트가 있으면 버전 번호 옆에 표시합니다.
 * 클릭하면 시스템 설정 페이지의 「버전 및 업데이트」 카드로 바로 이동합니다.
 *
 * 백엔드가 GitHub 조회 결과를 30분간 캐시하므로, 마운트될 때마다 조회해도 안전합니다.
 * ——인증되지 않은 GitHub API 는 IP 당 시간당 60회뿐이라, 이 캐시가 없으면 탭을 여러 개 열었을 때 할당량이 소진되어
 * 나중에 실제로 업데이트를 확인하려 해도 조회할 수 없게 됩니다.
 *
 * 조회 실패는 모두 조용히 처리합니다: 상단 바는 오류를 표시하는 곳이 아니며, 사용자가 설정 페이지에서 「업데이트 확인」을 누르면 원인을 볼 수 있습니다.
 */
export function UpdateBadge() {
  const t = useTranslations("header");
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update 에 이미 "버전 번호 비교 가능" 여부가 포함되어 있으므로, 개발 빌드에서는 이 알림이 표시되지 않습니다.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 조용히 무시: 네트워크 없음 / GitHub 속도 제한 모두 상단 바에 오류를 띄우지 않아야 합니다.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={t("updateTitle", { latest })}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 호흡 점: 상단 바에 요소가 많아 텍스트만 있으면 놓치기 쉬우므로, 애니메이션으로 한눈에 보이도록 합니다. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">{t("updateBadge", { latest })}</span>
      <span className="sm:hidden">{t("updateBadgeShort")}</span>
    </Link>
  );
}
