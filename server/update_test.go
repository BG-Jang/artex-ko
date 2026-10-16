package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache 는 GitHub API 할당량을 지키는 계층이다: 미인증 API 는 IP 당 시간당 60 회뿐이고,
// 상단바의 "새 버전 있음" 표시는 페이지가 열릴 때마다 조회한다. 캐시가 한 번 무효화되면 사용자가 탭을 여러 개 열기만 해도
// 할당량이 금방 소진되고, 정작 업데이트를 확인하려 할 때는 조회가 불가능해진다.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 次查询只应回源 1 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 사용자가 "업데이트 확인" 을 누르면 실시간 결과를 받아야 한다. 그렇지 않으면 방금 배포된 버전이 캐시 만료 전까지 보이지 않는다.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force 应绕过缓存，期望回源 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 저장 시각을 만료 직전으로 되돌려, TTL 이 끝난 상황을 모사한다.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 过期后应重新回源，期望 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github 不可达")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	// 실패 결과도 잠시 캐시해야 한다. 그렇지 않으면 GitHub 에 닿지 않을 때 페이지를 열 때마다 타임아웃을 기다리게 된다.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	if calls != 1 {
		t.Errorf("错误应短时缓存，期望回源 1 次，实际 %d 次", calls)
	}

	// 다만 오류 TTL 은 성공 TTL 보다 확실히 짧아야 한다. 네트워크가 복구되면 빨리 자가 치유되어야 하기 때문이다.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("错误 TTL(%v) 必须短于成功 TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	if calls != 2 {
		t.Errorf("错误 TTL 过期后应重试，期望 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 방문자가 탭을 닫으면 요청이 취소된다. 이는 GitHub 에 문제가 있다는 뜻이 아니므로, "취소됨" 을
	// 캐시에 써서는 안 된다 — 그러면 이후 30 분 동안 모든 방문자가 영문 모를 오류를 받게 된다.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시를 만료시켜 원본에서 다시 가져오게 한다

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("调用方已取消时应把错误透传给它")
	}

	// 핵심 불변식: 취소된 그 한 번은 아무 흔적도 남기지 않는다 — 캐시에 "취소됨" 오류가 없고,
	// 직전의 좋은 결과도 그대로 보존되어 있다.
	if c.err != nil {
		t.Fatalf("取消错误不应写进缓存，得到 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("缓存应保留上一次的好结果，得到 %+v", c.rel)
	}

	// 그 취소는 새 데이터를 가져오지 못했으므로, 다음 방문자는 원본에서 다시 가져와야 한다 — 그리고 정상적으로 결과를 받아야 하며,
	// 직전의 취소에 영향을 받아서는 안 된다.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("取消之后的正常请求不应报错: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("应拿到正常结果，得到 %+v", rel)
	}
}
