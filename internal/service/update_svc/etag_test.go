package update_svc

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// 未认证 GitHub API 的配额只有 60 次/小时且按出口 IP 计——共享出口（公司 NAT /
// 公共代理）上裸 GET 很快 403。条件请求（If-None-Match）命中 304 时**不消耗配额**，
// 重复检查因此只剩首次计费；304 必须原样返回缓存到的 release。
func TestFetchReleaseUsesETagConditionalRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("ETag", `"release-v1"`)
			_, _ = fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
			return
		}
		if r.Header.Get("If-None-Match") != `"release-v1"` {
			t.Errorf("call %d: If-None-Match = %q, want cached etag", n, r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)

	prev := apiBaseURL
	apiBaseURL = srv.URL + "/repos/" + githubRepo
	t.Cleanup(func() { apiBaseURL = prev })

	first, err := fetchRelease(ChannelStable)
	require.NoError(t, err)
	require.Equal(t, "v1.2.3", first.TagName)

	second, err := fetchRelease(ChannelStable)
	require.NoError(t, err)
	require.Equal(t, "v1.2.3", second.TagName, "304 must serve the cached release")
	require.Equal(t, int32(2), calls.Load())
}
