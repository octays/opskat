package oss_svc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
)

// newRedirectCluster 起一对假 S3:primary 一律回 301 PermanentRedirect(带
// x-amz-bucket-region: us-west-2,AWS 形态),regional 正常服务请求。
// 返回 (primary, regional, *hits)——hits[0]/hits[1] 分别是两边收到的请求数。
func newRedirectCluster(t *testing.T, regional http.HandlerFunc) (*minio.Client, *minio.Client, *[2]int) {
	t.Helper()
	hits := &[2]int{}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[0]++
		w.Header().Set("x-amz-bucket-region", "us-west-2")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>PermanentRedirect</Code><Message>The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.</Message><Bucket>bucket</Bucket><Endpoint>bucket.s3.us-west-2.amazonaws.com</Endpoint></Error>`))
	}))
	t.Cleanup(primary.Close)
	regionalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[1]++
		regional(w, r)
	}))
	t.Cleanup(regionalSrv.Close)
	return newTestClient(t, primary), newTestClient(t, regionalSrv), hits
}

func newTestClient(t *testing.T, srv *httptest.Server) *minio.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	mc, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4("access", "secret", ""),
		Secure: false,
		Region: "us-east-1",
	})
	require.NoError(t, err)
	return mc
}

// dialTo 把 redirectDial 接到 regional 客户端,regionEndpoint 固定映射 us-west-2。
func wireRedirect(a *minioAdapter, regional *minio.Client) *minioAdapter {
	return a.withRedirectDial(func(string) (*minio.Client, error) { return regional, nil }).
		withRegionEndpoint(func(_, region string) (string, bool) {
			return "resolved-" + region, region == "us-west-2"
		})
}

// AWS S3 跨区域桶:GET/POST 类操作被 301 PermanentRedirect 拒绝,错误体不带可用的
// 路径(minio 不解析 <Endpoint>),唯一线索是 x-amz-bucket-region 响应头。适配层必须
// 按该区域推导区域 endpoint 重拨重试;解析结果跨调用记忆,第二个请求直达不再打 primary。
func TestMinioAdapterGetObjectFollowsRegionRedirect(t *testing.T) {
	primary, regional, hits := newRedirectCluster(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.Header().Set("ETag", `"etag"`)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("hello"))
		}
	})
	adapter := wireRedirect(newMinioAdapter(primary), regional)

	rc, size, err := adapter.GetObject(context.Background(), "bucket", "k.txt")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello", string(body))
	require.EqualValues(t, 5, size)

	// 第二次同桶操作:命中桶→endpoint 记忆,不再触发 primary 的 301。
	_, _, err = adapter.GetObject(context.Background(), "bucket", "k.txt")
	require.NoError(t, err)
	require.Equal(t, 1, hits[0])
}

// HEAD 请求(StatObject/BucketExists)没有错误体,minio 按状态码合成
// Code="301 Moved Permanently"——识别必须兼容两种 Code 形态。
func TestMinioAdapterStatObjectFollowsHeadRedirect(t *testing.T) {
	primary, regional, _ := newRedirectCluster(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	})
	adapter := wireRedirect(newMinioAdapter(primary), regional)

	item, err := adapter.StatObject(context.Background(), "bucket", "k.txt")
	require.NoError(t, err)
	require.Equal(t, "k.txt", item.Key)
}

// presign 不发请求、无法自己发现重定向:必须复用此前在线操作解析出的桶 endpoint,
// 否则签出的 URL 指向错误区域,浏览器打开即是用户报告的 PermanentRedirect 页面。
func TestMinioAdapterPresignGetUsesRememberedEndpoint(t *testing.T) {
	primary, regional, _ := newRedirectCluster(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><Name>bucket</Name><IsTruncated>false</IsTruncated></ListBucketResult>`))
	})
	adapter := wireRedirect(newMinioAdapter(primary), regional)

	// 先列一次桶:重定向在此被解析并记忆(AWS 的 <Endpoint> 带桶名前缀,原样传递)。
	_, err := adapter.ListObjects(context.Background(), "bucket", "", 100, "")
	require.NoError(t, err)

	link, err := adapter.PresignGet(context.Background(), "bucket", "k.txt", time.Minute)
	require.NoError(t, err)
	regionalHost := regional.EndpointURL().Host
	require.True(t, strings.Contains(link, regionalHost), "presigned link %s should target %s", link, regionalHost)
}

// 可 Seek 的上传体(文件)在 301 后回卷重传到区域 endpoint。
func TestMinioAdapterPutObjectSeekableRetriesRedirect(t *testing.T) {
	var regionalBody string
	primary, regional, hits := newRedirectCluster(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		regionalBody = string(body)
		w.Header().Set("ETag", `"etag"`)
		_, _ = w.Write([]byte(`<CompleteMultipartUploadResult><ETag>"etag"</ETag></CompleteMultipartUploadResult>`))
	})
	adapter := wireRedirect(newMinioAdapter(primary), regional)

	err := adapter.PutObject(context.Background(), "bucket", "k.txt", strings.NewReader("payload"), 7, "text/plain")
	require.NoError(t, err)
	require.Equal(t, 1, hits[0])
	// minio 对 PUT 采用流式分块签名,请求体带 chunk 框架,只断言载荷完整到达。
	require.Contains(t, regionalBody, "payload")
}

// 自定义 endpoint(MinIO/私链)无法按区域改写:不能瞎猜 endpoint,必须把桶真实区域
// 连同改配置指引一起告诉用户,而不是透出原始 301 文案。
func TestMinioAdapterRedirectNotDerivableSurfacesRegion(t *testing.T) {
	primary, regional, _ := newRedirectCluster(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	adapter := newMinioAdapter(primary).withRedirectDial(func(string) (*minio.Client, error) {
		return regional, nil
	}).withRegionEndpoint(func(_, _ string) (string, bool) { return "", false })

	_, err := adapter.StatObject(context.Background(), "bucket", "k.txt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "us-west-2")
	require.Contains(t, err.Error(), "区域")
}

// 跨调用记忆:同一资产(同一 base host)的桶→endpoint 对所有适配层实例可见——
// 适配层是每次 IPC 调用新建的,记忆必须挂在服务层共享实例上才能让 presign 受益。
func TestBucketEndpointMemoSharedAcrossAdapters(t *testing.T) {
	primary, regional, _ := newRedirectCluster(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	})
	memo := newHostBucketEndpointMemo()

	first := wireRedirect(newMinioAdapter(primary), regional).withBucketEndpoints(memo)
	_, err := first.StatObject(context.Background(), "bucket", "k.txt")
	require.NoError(t, err)

	// 新适配层实例(下一次 IPC 调用):凭共享记忆直达,presign 不再签向错误区域。
	second := wireRedirect(newMinioAdapter(primary), regional).withBucketEndpoints(memo)
	link, err := second.PresignGet(context.Background(), "bucket", "k.txt", time.Minute)
	require.NoError(t, err)
	require.True(t, strings.Contains(link, regional.EndpointURL().Host), "link %s", link)
}

// memo 按 base host 隔离:资产 endpoint 变更后旧记忆不生效,重新解析。
func TestBucketEndpointMemoKeyedByBaseHost(t *testing.T) {
	memo := newHostBucketEndpointMemo()
	memo.Store("a.internal", "b", "redirected-a")
	_, ok := memo.Load("b.internal", "b")
	require.False(t, ok)
	ep, ok := memo.Load("a.internal", "b")
	require.True(t, ok)
	require.Equal(t, "redirected-a", ep)
}
