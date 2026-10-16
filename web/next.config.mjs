import createNextIntlPlugin from "next-intl/plugin";

import { fileURLToPath } from "node:url";

// 정적 내보내기: `NEXT_EXPORT=1 next build` 는 순수 정적 디렉터리를 web/out 에 생성하며,
// nginx web 루트 디렉터리에 그대로 넣어 실행할 수 있다. 개발(next dev)에서는 이 변수를 두지 않아 /api 리버스 프록시와 핫 리로드를 유지한다.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: 전체 사이트가 mock 으로 동작하며, 백엔드가 없어 /api 리버스 프록시가 필요 없다.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 상위 디렉터리의 lockfile 이 루트 디렉터리 추론과 리소스 경로 생성에 영향을 주지 않도록 한다.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // LAN IP 에서 dev 리소스(HMR)에 접근할 수 있게 허용한다. 필요에 따라 추가·삭제한다.
  // dev 단계에서 임의의 IPv4 출처가 /_next/* 와 HMR 에 접근하도록 열어 둔다(LAN IP 가 바뀌어도 영향받지 않는다).
  // 주의: Next 는 보안상 맨 "*" 를 금지하므로 구간 와일드카드를 써야 한다. "*.*.*.*" 는 임의의 IPv4 와 매칭된다.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 순수 정적 내보내기: Node 런타임 없음; 이미지 최적화 안 함; 각 라우트가 <route>/index.html 를 생성한다.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: 백엔드가 없어 /api 리버스 프록시가 필요 없다.
          images: { unoptimized: true },
        }
      : {
          // 개발: /api/* 를 Go 백엔드로 리버스 프록시한다(기본 :8787, AUTOPENTEST_API 로 덮어쓸 수 있음).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

// next-intl 플러그인. 요청 설정은 src/i18n/request.ts 에 둔다. i18n 경로 라우팅을
// 쓰지 않으므로 미들웨어는 추가하지 않는다(정적 내보내기 output: "export" 와 호환).
const withNextIntl = createNextIntlPlugin("./src/i18n/request.ts");

export default withNextIntl(nextConfig);
