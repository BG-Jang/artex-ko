import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// Sheet/Dialog의 onInteractOutside 닫힘 판정 헬퍼.
//
// 배경: Sheet 내부의 Radix 레이어(Select 드롭다운, DropdownMenu, Popover 등)는 Sheet 바깥으로
// portal 됩니다. 레이어가 열린 상태에서 오버레이/Sheet 바깥을 눌러 닫으려 하면, 이 pointerdown을
// Select와 Sheet 두 DismissableLayer가 동시에 처리합니다. Select가 먼저 닫히고 discrete 이벤트이므로
// React가 동기 flush 하여, Sheet 핸들러가 실행될 때 레이어의 data-state는 이미 closed로 바뀌어 있습니다.
// 따라서 "지금 이 순간" 레이어가 열려 있는지 검사하는 방식은 신뢰할 수 없습니다(실측 검증됨).
//
// 올바른 방법: Radix의 pointerdown 리스너는 버블링 단계에서 동작합니다. 우리는 capture 단계(그보다 먼저)에서
// "지금 레이어가 열려 있는지"를 미리 기록해 두고, onInteractOutside는 이 기록값을 읽어 닫힘을 허용할지 결정합니다.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: Radix 버블링 단계의 pointerdown 핸들러보다 먼저 기록
  );
}

// radixOverlayWasOpenAtPointerDown은 "가장 최근 pointerdown 발생 시점에 Radix 레이어가 열려 있었는지"를 반환합니다.
// Sheet/Dialog는 이를 기준으로 판단합니다: 레이어가 열려 있을 때 오버레이를 누르면 레이어만 닫고 자신은 닫지 않습니다.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText는 텍스트를 클립보드에 기록하고 성공 여부를 반환합니다.
// 배경: navigator.clipboard는 보안 컨텍스트(HTTPS / localhost)에서만 사용 가능합니다. IP + HTTP로
// 접속하면 undefined이므로 execCommand("copy")로 폴백합니다.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 폴백 방식으로 계속 진행
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
