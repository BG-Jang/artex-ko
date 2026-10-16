package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout은 각각 연결 수립과 SMTP 세션 전체를 제한한다.
// net/smtp 자체에는 타임아웃 메커니즘이 전혀 없다. 이 두 값을 설정하지 않으면 멈춘 상대 때문에
// 전달 goroutine이 영원히 대기하게 된다. dispatcher는 단일 goroutine으로 직렬 처리하므로,
// 결국 알림 시스템 전체가 멈춘다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel은 SMTP 이메일 전달을 구현한다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 이메일에는 플랫폼 제한이 없지만, 이것으로 도배하는 것은 바람직하지 않다. 느슨한 기본값을 준다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 마스킹한다. SMTP 호스트, 계정, 수신자는 비밀이 아니며, 이를 마스킹하면 편집만 번거로워진다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port는 비밀번호를 어느 서버에 넘길지 결정하고, tls는 전송을 암호화할지 결정한다. 셋 중 어느 하나라도 바뀌면
// 비밀번호를 다시 명시해야 한다. 덤으로 「TLS 끄기」 단계에서도 자격 증명을 명시적으로 함께 보내야 하며, 무심코 바꿀 수 없게 된다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소가 없습니다")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트가 올바르지 않습니다 (1-65535 범위여야 합니다)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("발신자 주소가 없습니다")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 최소 하나 필요합니다")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: 상대가 지원하면 업그레이드한다. 평문 세션에서는 자격 증명을 보내면 안 된다(아래 auth 설명 참고).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth는 암호화되지 않은 연결에서 자격 증명 전송을 거부한다(대상이 localhost인 경우는 예외).
			// 이것은 **올바른** 보안 동작이므로 우회해서는 안 되지만, 원인을 분명히 설명해야 한다.
			// 그렇지 않으면 사용자는 「unencrypted connection」만 보고 무엇을 해야 할지 알 수 없다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 전송을 거부했습니다: 연결이 암호화되지 않았습니다. TLS 를 켜거나, 465 포트(암시적 TLS)를 사용하거나, 「TLS 사용」을 체크하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("발신자 %s 주소가 거부되었습니다", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 주소가 거부되었습니다", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일 제출 실패: %w", err)
	}
	// Quit이 실패해도 「메일이 서버에 수신되었다」는 사실에는 영향이 없으므로, 그 오류는 무시한다.
	_ = client.Quit()
	// 이메일에는 길이 자르기가 없다(HTML 본문을 전부 보낸다). 따라서 배치 전체를 전달 완료로 센다.
	return len(m.Items), nil
}

// emailDial은 SMTP 연결을 수립한다.
//
// implicitTLS=true이면 465 포트처럼 「연결하자마자 TLS」 방식을 쓰고, false이면 25/587 포트에 평문으로 연결한 뒤
// STARTTLS를 한다. 둘을 섞으면 안 된다. 465 포트에 평문 greeting을 보내면 바로 연결이 끊어진다.
//
// 세션 기한은 **연결 시점에** 설정한다(사후에 덧붙이지 않는다). net/smtp의 Client는 기반 연결을
// 외부에서 접근할 수 없는 비공개 필드에 숨기므로, 연결을 넘긴 뒤에는 미리 설정한 deadline에 의존할 수밖에 없다.
// 이렇게 하면 핸드셰이크 단계의 블로킹도 함께 막을 수 있다.
// Control에는 HTTP 계열 채널과 같은 blockInternalDial 가드를 건다. 이것이 없으면 SMTP가 전체 SSRF 방어의 빈틈이 된다.
// host에 169.254.169.254나 127.0.0.1을 넣으면 그대로 연결되고, smtp.NewClient 핸드셰이크가 실패할 때는
// 상대가 돌려준 한 줄이 오류에 포함되어 last_error를 거쳐 전달 이력 API로 되돌아온다. 이는 반쯤 눈먼 읽기 수단이 된다.
// 「연결 거부와 타임아웃」의 소요 시간 차이로 포트를 탐지할 수도 있다. 다이얼 단계가 최종 적용 지점이며, DNS 리바인딩도 막는다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError는 SMTP 응답 코드에 따라 어느 단계의 실패를 「재시도 가능」과 「영구적 실패」로 나눈다.
//
// 반드시 구분해야 하는 이유: SMTP의 4xx와 5xx는 의미가 완전히 다르다.
//   - 4xx(450 회색 목록, 451 로컬 오류, 452 저장 공간 부족)는 **일시적** 거부이며,
//     정석은 나중에 재시도하는 것이다. 특히 회색 목록은 거의 모든 첫 전달에서 만나게 된다.
//   - 5xx(550 사용자 없음, 553 잘못된 주소)는 영구적 거부이므로 재시도해도 의미가 없다.
//
// 모두 영구적 실패로 판정하면, 회색 목록을 쓰는 메일 서버는 **모든** 푸시를 첫 시도 뒤에 failed로 떨어뜨린다.
// 그런데 이런 실패야말로 자동 재시도가 가장 필요한 상황이다.
// 응답 코드는 오류 문자열의 앞 세 자리 숫자로 얻는다. 코드를 얻지 못하면 재시도 가능으로 처리한다(파싱되지 않는다는 이유로
// 일시적 장애를 실패로 확정하는 것보다, 한 번 더 시도하는 편이 낫다).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode는 SMTP 오류 문자열에서 앞에 붙은 세 자리 응답 코드를 가져오며, 얻지 못하면 0을 반환한다.
// net/smtp는 오류 코드 필드를 외부에 공개하지 않으므로 문자열에서 가져와야 한다. 형식은 「450 4.7.1 ...」이다.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage는 완전한 RFC 5322 메일을 조립한다.
//
// 본문을 base64로 인코딩하는 이유는 두 가지이다. 첫째, SMTP는 한 줄이 1000바이트를 넘지 않도록 규정하는데,
// HTML 본문(특히 요약 메일)은 매우 긴 줄이 나오기 쉽다. 둘째, base64에서는 "."으로 시작하는 줄이 자연스럽게 나오지 않아,
// SMTP의 점(.) 이스케이프 처리를 생략할 수 있다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 비ASCII 제목은 반드시 RFC 2047 인코딩을 해야 한다. 그렇지 않으면 클라이언트에서 깨진 글자로 표시된다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 이메일에는 길이 상한이 없으므로 본문을 자르지 않는다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64는 76글자마다 줄을 나눈다. RFC 2045를 따른다.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
