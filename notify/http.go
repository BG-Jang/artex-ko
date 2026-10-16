package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets는 메시지를 루프백 / 링크 로컬 주소로 전달하는 것을 허용할지 결정한다.
//
// 기본적으로 거부한다. 이 주소대는 IM 봇이나 공인 메일 서버가 있는 곳이 아니며, 접근할 수 있는 대상이 민감하다.
// 같은 장비에서 돌아가는 다른 서비스의 관리 포트, 그리고 클라우드 환경의 메타데이터 엔드포인트
// (169.254.169.254, 인스턴스 자격 증명을 읽어 낼 수 있다)가 여기에 해당한다. 전달 주소는 관리자가 설정하지만,
// XSS/CSRF로 탈취된 관리 세션이나 같은 JWT를 공유하는 다른 사람은 설정을 바꿔 응답 내용을 되읽을 수 있다.
// doJSON은 4xx/5xx 응답 본문의 앞 200바이트를 last_error에 기록하고, 전달 이력 API가 이를 다시 보여 주기 때문에
// 이것이 반쯤 눈먼 읽기 수단이 된다.
//
// 다만 「로컬 SMTP 릴레이」(127.0.0.1:25의 postfix)는 자체 구축 메일의 흔한 구성이라, 일괄 차단하면 사용자가 막힌다.
// 그래서 하드코딩으로 허용하지 않고, 명시적인 탈출구를 둔다.
// ARTEX_NOTIFY_ALLOW_LOCAL=1을 설정하면 허용된다.
//
// AllowLocalTargetsEnv를 외부에 공개하는 이유는 테스트가 이를 명확히 켤 수 있게 하기 위해서다. 이 패키지와 server 패키지의
// 테스트 사례는 127.0.0.1의 httptest 가짜 수신 측을 많이 쓰는데, 켜지 않으면 전부 이 보호 장치에 막힌다.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP는 대상 IP가 「기본적으로 전달을 허용하지 않는」 주소대에 속하는지 보고한다.
//
// 루프백, 링크 로컬(클라우드 메타데이터 169.254.169.254 포함), 미지정 주소, 멀티캐스트만 거부한다.
// RFC1918 사설망은 **거부하지 않는다**. 내부망에 자체 구축한 Mattermost나 SMTP 릴레이를 쓰는 것은 흔하고 정당한 사용법이라,
// 이것까지 막으면 실제 환경에서 기능이 바로 쓸 수 없게 된다. 이 절충은 의도된 것이다. 보호는 정말 민감한 대상을 막아야 하며,
// 정상적인 배포까지 함께 무력화해서는 안 된다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6(::ffff:127.0.0.1)는 IPv4로 되돌려서 판단해야 한다. 그렇지 않으면 검사를 우회한다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial은 http.Transport 다이얼러의 Control 훅이며, **연결이 수립될 때** 대상 주소를 검사한다.
//
// 설정을 저장할 때만 검증하지 않고 다이얼 단계에 두는 이유: 여기가 실제로 적용되는 지점이기 때문이다.
// 설정 검증을 우회하는 두 경우를 함께 막을 수 있다. DNS 리바인딩(검증 때는 공인 IP로 해석되고, 실제 연결 때는 내부 IP로 해석됨)과
// 리다이렉트(교차 호스트 리다이렉트는 이미 거부하지만, 같은 호스트 안의 리다이렉트로 경로가 다른 곳을 가리킬 수 있음)이다.
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소를 해석할 수 없습니다: %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬 또는 링크 로컬 주소 %s 로는 전송하지 않습니다 (로컬 서비스로 보내야 하면 %s=1 로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport는 기본 Transport에 다이얼 가드 하나만 추가한 것이다.
// Clone을 써서 기본 튜닝(연결 풀, HTTP/2, 타임아웃, proxy 등)을 모두 유지하며,
// 검사 하나를 추가하려고 다른 동작을 바꾸지 않는다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient는 모든 채널 전달이 공유하는 클라이언트이다.
//
// 프로젝트의 전역 출구 프록시(server 쪽 GlobalProxy)를 **일부러 재사용하지 않는다**. 그 프록시는 침투 테스트 대상 트래픽용이며,
// 불안정한 터널인 경우가 많다. 알림의 가용성이 대상 네트워크의 흔들림에 좌우되어서는 안 된다.
// IM 푸시는 직접 연결하면 된다. 타임아웃은 15초로 한다. 이보다 느린 상대는 사실상 장애 상태이다.
//
// 교차 호스트 리다이렉트는 거부한다. 이 기능의 전달 주소는 모두 「고정된 endpoint」 형태이므로 정상적으로는 다른 호스트로
// 리다이렉트되지 않는다. 그리고 이들 서비스의 자격 증명(딩톡의 access_token, 기업 위챗의 key, Telegram의 bot token)은
// **URL 안에 있다**. 교차 호스트 리다이렉트를 따라가면 자격 증명을 리다이렉트 대상에게 넘기는 것과 같다.
// 같은 호스트 안의 리다이렉트(예: 끝에 슬래시 추가)는 여전히 허용한다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리다이렉트가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("다른 호스트로의 리다이렉트는 허용하지 않습니다 (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit은 응답 본문을 읽는 크기를 제한한다. 상대가 비정상일 때 매우 큰 내용을 돌려줄 수 있지만,
// 우리에게 필요한 것은 오류 코드와 전달 이력에 표시할 짧은 오류 설명뿐이다.
const respBodyLimit = 8 << 10

// doJSON은 요청 한 번을 보내고 응답 본문을 반환한다(크기 제한 적용).
//
// payload가 nil이면 빈 본문을 보낸다(GET이나 플랫폼이 본문을 요구하지 않는 경우에 쓴다).
// headers의 키와 값은 그대로 덧붙이며, 범용 Webhook의 사용자 정의 헤더에 쓰인다.
//
// 오류 분류가 이 함수의 핵심 책임이다. 네트워크 계층 실패와 5xx/408/429는 「재시도 가능」으로 분류하고,
// 나머지 4xx는 「영구적 실패」로 분류한다. 403을 재시도하면 같은 오류를 로그에 세 번 찍을 뿐이다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 로컬 버그(설정 필드의 타입이 잘못됨)이며, 재시도해도 나아지지 않는다.
			return nil, Permanent(fmt.Errorf("요청 본문을 만들지 못했습니다: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL이 잘못되었다. 대개 사용자가 주소를 잘못 입력한 것이므로 영구적 실패이다.
		// 여기서도 err를 그대로 전달하면 안 된다. url.Parse의 오류 문자열에 전체 주소가 들어 있기 때문이다.
		return nil, Permanent(fmt.Errorf("요청 주소가 올바르지 않습니다: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거부, DNS 실패, 타임아웃. 대부분 일시적 장애이므로 백오프 재시도에 맡긴다.
		//
		// 오류 문자열은 반드시 마스킹한 뒤에 밖으로 내보내야 한다. 이유: http.Client.Do가 반환하는 *url.Error의 Error()는
		// `Op "전체URL": 하위 오류` 형태이고, 이 기능의 해당 서비스들은 자격 증명이 **URL 안에 있다**
		// (딩톡 access_token, 기업 위챗 key, 페이슈 hook id, Telegram /bot<token>/). 마스킹하지 않으면 자격 증명이
		// 이 오류 문자열을 타고 네 곳으로 새어 나간다. notification_deliveries의 last_error(평문으로 DB에 저장됨),
		// 전달 이력 API 응답(**채널 설정의 마스킹을 우회함**), 서버 로그, 그리고 테스트 발송 API가 프론트엔드에 돌려주는 502 문자열이다.
		return nil, fmt.Errorf("요청에 실패했습니다: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답을 읽지 못했습니다: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429(제한)와 408(타임아웃)은 재시도할 가치가 있다. 그 밖의 4xx는 설정 또는 권한 문제이므로 재시도해도 의미가 없다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대 서버가 요청을 제한하거나 응답이 지연되었습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 서버에 오류가 발생했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대 서버가 요청을 거부했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet은 응답 본문을 한 줄짜리 짧은 텍스트로 줄여 오류 정보에 쓴다. 응답에는 줄바꿈과 많은 공백이 들어 있을 수 있으며,
// 그대로 last_error에 넣으면 전달 이력 페이지의 레이아웃이 깨진다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget은 전달 주소를 「scheme://host/…」 형태로 줄여 오류 정보에 쓴다.
//
// 이것은 이 패키지의 유일한 주소 마스킹 기준이며, 의도적으로 **충분히 거칠게** 만들었다. scheme과 host를 제외한
// 나머지는 모두 버린다. 이유는, 어느 부분이 자격 증명인지 「범용적이면서 안전하게」 판단할 방법이 없기 때문이다.
//
//	딩톡     자격 증명이 query에 있음      /robot/send?access_token=xxx
//	기업 위챗 자격 증명이 query에 있음      /cgi-bin/webhook/send?key=xxx
//	페이슈   자격 증명이 **경로 끝 부분**에 있음 /open-apis/bot/v2/hook/<hook_id>
//	Telegram 자격 증명이 **경로 중간**에 있음    /bot<token>/sendMessage
//
// 「쓸모 있는 부분만 남기는」 방식을 쓰려면 채널마다 패치를 붙여야 하며, 어느 한 곳이라도 빠지면 곧 자격 증명 유출이다.
// host만 남겨도 문제 해결에는 충분하다(DNS 해석 실패, 연결 불가, 인증서 문제를 모두 짚어 낼 수 있다).
// 어느 봇인지는 채널 설정의 마스킹 끝자리 힌트로 알아볼 수 있다.
//
// 파싱에 실패하면 고정된 자리표시자를 반환한다. 원래 문자열은 절대 되돌려주지 않는다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소를 해석할 수 없음)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError는 전송 계층 오류에서 주소를 제거하고 하위 원인만 남긴다.
//
// *url.Error의 구조는 {Op, URL, Err}이며, Error()는 URL을 함께 출력한다. 여기서는 Err 필드를 명시적으로 가져와
// 그 Error() 메서드를 우회한다. 사후에 문자열을 치환하는 것보다 확실하다. 치환은 URL 인코딩/이스케이프의 여러 변형을
// 올바르게 처리해야 하므로 누락되기 쉽다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error가 아닌 오류(예: 리다이렉트 정책이 반환한 오류)에도 주소가 들어 있을 수 있으므로, 공통으로 마스킹을 거친다.
	return redactURLsInText(err.Error())
}

// redactURLsInText는 텍스트 안에 나타난 http(s) 주소를 마스킹된 형태로 바꾼다.
//
// 구조화된 필드를 얻을 수 없는 오류(리다이렉트 정책 오류, 서드파티 라이브러리의 사용자 정의 오류)의 폴백으로 쓴다.
// http/https 접두사만 인식하며, 공백과 따옴표로 분할한다. 주소에는 이 두 종류의 문자가 들어가지 않기 때문이다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
