package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo 는 릴리스 소스다. 설정 항목으로 만들지 않고 하드코딩한다. 업데이트 소스를 설정 가능하게 하면
// 설정을 바꿀 수 있는 누구에게나 원격 코드 실행 통로를 주는 셈이며, 침투 테스트 플랫폼에서 이 틈은
// 절대 열어서는 안 된다.
const Repo = "Autumn-27/artex"

// latestURL 은 GitHub 의 "최신 정식 릴리스" 엔드포인트다. prerelease 와 draft 는 자동으로 건너뛴다.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts 는 업그레이드 경로가 접근할 수 있는 도메인을 제한한다. 아래 checkRedirect 와 함께 동작하여,
// 어느 한 홉이라도 목록 밖의 호스트로 리다이렉트되면 즉시 실패한다. 이는 DNS 오염 / 중간자 공격으로
// 바이너리가 바뀌는 것을 막는 첫 번째 관문이며, 두 번째 관문은 SHA256SUMS 대조다.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 자산이 실제로 저장되는 오브젝트 스토리지
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release 는 GitHub Release 에서 우리가 필요로 하는 필드다.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset 은 Release 에 첨부된 파일 하나다.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient 는 GitHub 도메인만 허용하는 HTTP 클라이언트를 만든다. proxy 가 비어 있으면 직접 연결한다.
//
// 일부러 기본 Transport 를 재사용하지 않는다. 업그레이드 경로는 반드시 TLS 를 강제하고 인증서를 검증해야 하며,
// 다른 곳에서 설정한 InsecureSkipVerify 등의 영향을 받으면 안 된다.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 패키지 전체를 내려받으므로 요청 단위 타임아웃으로 멈추면 안 된다
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리다이렉트 횟수가 너무 많습니다")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL 은 https 와 도메인 화이트리스트를 강제한다.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 가 아닌 주소를 거부합니다: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 도메인이 아닌 주소를 거부합니다: %s", u.Hostname())
	}
	return nil
}

// FetchLatest 는 최신 정식 릴리스를 조회한다.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 에 접속하지 못했습니다(시스템 설정에서 전역 프록시를 설정할 수 있습니다): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 인증되지 않은 GitHub API 는 IP 당 시간당 60 회로 제한되어, 출구 IP 를 공유하는 경우 쉽게 걸린다.
		return nil, fmt.Errorf("GitHub API 요청 제한에 걸렸습니다(시간당 60회). 잠시 후 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s 에 아직 정식 릴리스가 없습니다", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 응답 코드 %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 해석 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release 에 tag 가 없습니다")
	}
	return &rel, nil
}

// AssetName 은 현재 플랫폼에 해당하는 배포 패키지 이름을 반환하며, build.sh 의 package_binary 와 일치한다:
// artex-<버전>-<os>-<arch>.zip (버전 번호에는 v 접두사가 없다).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset 은 Release 에서 이름으로 자산을 찾는다.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
