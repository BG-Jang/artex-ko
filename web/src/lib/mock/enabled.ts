// Mock 스위치. 빌드 시점에 주입되는 공개 변수입니다 (브라우저에서 읽으려면 NEXT_PUBLIC_ 접두사가 필요).
// Vercel 에서 NEXT_PUBLIC_MOCK=1 을 설정하면 사이트 전체가 mock 으로 동작하며 백엔드가 필요 없습니다.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
