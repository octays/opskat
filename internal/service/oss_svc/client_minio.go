package oss_svc

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"

	"github.com/octays/opskat/internal/connpool"
)

// minioAdapter 把 *minio.Client 适配成窄接口 Client。
type minioAdapter struct {
	mc         *minio.Client
	httpClient *http.Client
	partSizeMB int

	// redirectDial 在桶实际位于另一区域（PermanentRedirect）时按响应给出的
	// 真实 endpoint 另拨客户端；nil 则不跟随（测试默认不设）。
	redirectDial func(endpoint string) (*minio.Client, error)
	// regionEndpoint 由「资产 endpoint + 301 响应头给出的桶真实区域」推导区域
	// endpoint；minio 不解析 <Endpoint>，带错误体的请求只有区域这条线索。
	// 默认 connpool.AWSRegionalEndpoint，非 AWS 形态不可推导。
	regionEndpoint func(host, region string) (string, bool)
	// bucketEndpoint 记忆「桶实际所在 endpoint」：适配层每次 IPC 调用新建，presign
	// 又不发请求无法自己发现重定向，只能靠在线操作留下的这份记忆直达。默认实例内
	// 私有；服务层用 withBucketEndpoints 换成跨调用共享的实例。
	bucketEndpoint bucketEndpointMemo
	redirectMu     sync.Mutex
	redirected     map[string]*minio.Client
}

func newMinioAdapter(mc *minio.Client, partSizeMB ...int) *minioAdapter {
	return newMinioAdapterWithHTTP(mc, http.DefaultClient, partSizeMB...)
}

func newMinioAdapterWithHTTP(mc *minio.Client, httpClient *http.Client, partSizeMB ...int) *minioAdapter {
	a := &minioAdapter{
		mc: mc, httpClient: httpClient, redirected: map[string]*minio.Client{},
		regionEndpoint: connpool.AWSRegionalEndpoint,
		bucketEndpoint: newHostBucketEndpointMemo(),
	}
	if len(partSizeMB) > 0 {
		a.partSizeMB = partSizeMB[0]
	}
	return a
}

func (a *minioAdapter) withRedirectDial(fn func(endpoint string) (*minio.Client, error)) *minioAdapter {
	a.redirectDial = fn
	return a
}

func (a *minioAdapter) withRegionEndpoint(fn func(host, region string) (string, bool)) *minioAdapter {
	a.regionEndpoint = fn
	return a
}

func (a *minioAdapter) withBucketEndpoints(m bucketEndpointMemo) *minioAdapter {
	a.bucketEndpoint = m
	return a
}

// bucketEndpointMemo 记忆「base host + 桶 → 实际 endpoint」。按 base host 隔离:
// 资产 endpoint 变更(组网/凭据切换)后旧记忆自动失效,重新解析。
type bucketEndpointMemo interface {
	Load(baseHost, bucket string) (string, bool)
	Store(baseHost, bucket, endpoint string)
}

type hostBucketEndpointMemo struct {
	mu sync.Mutex
	m  map[string]string
}

func newHostBucketEndpointMemo() *hostBucketEndpointMemo {
	return &hostBucketEndpointMemo{m: map[string]string{}}
}

func (m *hostBucketEndpointMemo) Load(baseHost, bucket string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ep, ok := m.m[baseHost+"\x00"+bucket]
	return ep, ok
}

func (m *hostBucketEndpointMemo) Store(baseHost, bucket, endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[baseHost+"\x00"+bucket] = endpoint
}

// clientForEndpoint 取重定向 endpoint 的客户端（首个桶区域解析结果复用）。
func (a *minioAdapter) clientForEndpoint(endpoint string) (*minio.Client, error) {
	a.redirectMu.Lock()
	defer a.redirectMu.Unlock()
	if c, ok := a.redirected[endpoint]; ok {
		return c, nil
	}
	c, err := a.redirectDial(endpoint)
	if err != nil {
		return nil, err
	}
	a.redirected[endpoint] = c
	return c, nil
}

// bucketClient 返回操作该桶应使用的客户端：桶→endpoint 记忆命中时直达（必要时
// 拨号），未命中用主客户端。记忆指向的 endpoint 拨不通是真实故障，原样返回错误。
func (a *minioAdapter) bucketClient(bucket string) (*minio.Client, error) {
	if a.redirectDial == nil {
		return a.mc, nil
	}
	if ep, ok := a.bucketEndpoint.Load(a.mc.EndpointURL().Host, bucket); ok {
		c, err := a.clientForEndpoint(ep)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return a.mc, nil
}

// withBucketRedirect 执行一次桶级操作；PermanentRedirect（桶在另一区域）时跟随
// 重定向重试一次。成功解析的桶→endpoint 记入跨调用记忆。
func (a *minioAdapter) withBucketRedirect(ctx context.Context, bucket string, op func(*minio.Client) error) error {
	c, err := a.bucketClient(bucket)
	if err != nil {
		return err
	}
	opErr := op(c)
	region, redirected := bucketRedirectRegion(opErr)
	if !redirected {
		return opErr
	}
	c, err = a.followBucketRedirect(ctx, bucket, "", region, opErr)
	if err != nil {
		return err
	}
	return op(c)
}

// followBucketRedirect 统一的重定向跟随：优先响应明确给出的 endpoint（presign 裸
// HTTP 路径的错误体里有），否则按 x-amz-bucket-region 给出的区域推导。不可跟随
// （未接线 / 不可推导）时返回原错误，让上层决定怎么呈现。
func (a *minioAdapter) followBucketRedirect(ctx context.Context, bucket, endpoint, region string, origErr error) (*minio.Client, error) {
	if a.redirectDial == nil {
		return nil, origErr
	}
	base := a.mc.EndpointURL().Host
	if endpoint == "" {
		derived, ok := a.regionEndpoint(base, region)
		if !ok {
			// 自定义 endpoint / 私有链路不能凭区域瞎改写：把桶真实区域连同
			// 改配置指引告诉用户，而不是透出原始 301 文案。
			return nil, fmt.Errorf("桶 %s 位于 %s 区域，当前 endpoint（%s）无法按区域自动寻址，请把资产的 endpoint/区域改为桶所在区域: %w", bucket, region, base, origErr)
		}
		endpoint = derived
	}
	logger.Ctx(ctx).Warn("oss bucket redirect followed",
		zap.String("bucket", bucket), zap.String("region", region), zap.String("endpoint", endpoint))
	c, err := a.clientForEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	a.bucketEndpoint.Store(base, bucket, endpoint)
	return c, nil
}

// bucketRedirectRegion 从 minio 错误里识别「桶在另一区域」并取出真实区域。
// GET/POST 等带错误体的请求 Code=PermanentRedirect；HEAD 无错误体，minio 按状态
// 码合成 Code="301 Moved Permanently"——两种形态都认。区域来自 x-amz-bucket-region
// 响应头（minio 填入 ErrorResponse.Region）；不带的（如阿里云）无法按区域跟随。
func bucketRedirectRegion(err error) (string, bool) {
	var e minio.ErrorResponse
	if !errors.As(err, &e) {
		return "", false
	}
	if e.Code != "PermanentRedirect" && e.StatusCode != http.StatusMovedPermanently {
		return "", false
	}
	if e.Region == "" {
		return "", false
	}
	return e.Region, true
}

func (a *minioAdapter) ListBuckets(ctx context.Context) ([]BucketItem, error) {
	bs, err := a.mc.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BucketItem, 0, len(bs))
	for _, b := range bs {
		out = append(out, BucketItem{Name: b.Name, CreationDate: b.CreationDate.Unix()})
	}
	return out, nil
}

func (a *minioAdapter) ListObjects(
	ctx context.Context, bucket, prefix string, maxKeys int, continuationToken string,
) (*ListObjectsPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := a.bucketClient(bucket)
	if err != nil {
		return nil, err
	}
	page, err := a.listOnce(ctx, c, bucket, prefix, maxKeys, continuationToken)
	if endpoint, redirect := permanentRedirectEndpoint(err); redirect {
		// 桶在另一区域（资产 endpoint 与桶实际区域不一致）。minio 客户端的常规
		// 操作自行解析桶区域，这条 Presign+裸 HTTP 的路径不会——按响应给出的
		// 真实 endpoint 重签重试一次；解析结果按 endpoint 缓存，翻页直达。
		c, dialErr := a.followBucketRedirect(ctx, bucket, endpoint, "", err)
		if dialErr != nil {
			return nil, dialErr
		}
		return a.listOnce(ctx, c, bucket, prefix, maxKeys, continuationToken)
	}
	if region, redirect := bucketRedirectRegion(err); redirect {
		// 错误体没给 endpoint（如 MinIO）：退回按响应头区域推导。
		c, dialErr := a.followBucketRedirect(ctx, bucket, "", region, err)
		if dialErr != nil {
			return nil, dialErr
		}
		return a.listOnce(ctx, c, bucket, prefix, maxKeys, continuationToken)
	}
	return page, err
}

// listOnce 用指定客户端签名并取一页列表。
func (a *minioAdapter) listOnce(
	ctx context.Context, mc *minio.Client, bucket, prefix string, maxKeys int, continuationToken string,
) (*ListObjectsPage, error) {
	params := url.Values{"list-type": {"2"}, "encoding-type": {"url"}, "delimiter": {"/"}, "prefix": {prefix}}
	if continuationToken != "" {
		params.Set("continuation-token", continuationToken)
	}
	if maxKeys > 0 {
		params.Set("max-keys", strconv.Itoa(maxKeys))
	}
	u, err := mc.Presign(ctx, http.MethodGet, bucket, "", time.Minute, params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newS3StatusError("list objects", resp, raw)
	}
	var result minio.ListBucketV2Result
	if err := xml.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.IsTruncated && result.NextContinuationToken == "" {
		return nil, fmt.Errorf("list objects response is truncated without a continuation token")
	}
	items := make([]ObjectItem, 0, len(result.Contents)+len(result.CommonPrefixes))
	for _, obj := range result.Contents {
		if obj.Key, err = decodeListedName(obj.Key, result.EncodingType); err != nil {
			return nil, err
		}
		items = append(items, toObjectItem(obj))
	}
	for _, p := range result.CommonPrefixes {
		name, err := decodeListedName(p.Prefix, result.EncodingType)
		if err != nil {
			return nil, err
		}
		items = append(items, ObjectItem{Key: name, IsPrefix: true})
	}
	return &ListObjectsPage{
		Items: items, IsTruncated: result.IsTruncated,
		NextContinuationToken: result.NextContinuationToken,
	}, nil
}

// decodeListedName 还原列表响应里的 key / 前缀。请求带了 encoding-type=url,服务端在响应里
// 以 EncodingType 声明它确实编码了;各家编码范围不同(R2 连 "/" 也编码成 %2F),只按声明解码。
func decodeListedName(name, encodingType string) (string, error) {
	if encodingType != "url" {
		return name, nil
	}
	decoded, err := url.QueryUnescape(name)
	if err != nil {
		return "", fmt.Errorf("list objects: decode %q: %w", name, err)
	}
	return decoded, nil
}

func (a *minioAdapter) StatObject(ctx context.Context, bucket, key string) (ObjectItem, error) {
	var item ObjectItem
	err := a.withBucketRedirect(ctx, bucket, func(mc *minio.Client) error {
		info, err := mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
		if err != nil {
			return err
		}
		item = toObjectItem(info)
		return nil
	})
	return item, err
}

func (a *minioAdapter) RemoveObject(ctx context.Context, bucket, key string) error {
	return a.withBucketRedirect(ctx, bucket, func(mc *minio.Client) error {
		return mc.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
	})
}

// CopyObject 的重试按目的桶寻址：S3 服务端拷贝本就要求同区域，跨桶移动
// (moveObjectWith)源与目的是同一桶，按目的桶跟随即覆盖。
func (a *minioAdapter) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	return a.withBucketRedirect(ctx, dstBucket, func(mc *minio.Client) error {
		_, err := mc.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: dstBucket, Object: dstKey},
			minio.CopySrcOptions{Bucket: srcBucket, Object: srcKey},
		)
		return err
	})
}

func (a *minioAdapter) RemoveObjects(ctx context.Context, bucket string, keys []string) error {
	return a.withBucketRedirect(ctx, bucket, func(mc *minio.Client) error {
		objCh := make(chan minio.ObjectInfo, len(keys))
		for _, k := range keys {
			objCh <- minio.ObjectInfo{Key: k}
		}
		close(objCh)
		var errs []minio.RemoveObjectError
		for rerr := range mc.RemoveObjects(ctx, bucket, objCh, minio.RemoveObjectsOptions{}) {
			errs = append(errs, rerr)
		}
		return aggregateRemoveErrors(errs)
	})
}

// aggregateRemoveErrors 把 minio 批量删除通道里的逐项错误聚合成一个显式报告;
// 空则 nil。绝不静默丢弃部分失败。
func aggregateRemoveErrors(errs []minio.RemoveObjectError) error {
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%s: %v", e.ObjectName, e.Err))
	}
	return fmt.Errorf("批量删除部分失败(%d): %s", len(errs), strings.Join(parts, "; "))
}

// PutObject 在 301 后重传：可 Seek 的 reader（文件上传）回卷到起点重传；不可回放
// 的流先用一次 HEAD 桶把重定向解析掉再上传（探测失败不阻塞，由上传本身暴露真错）。
func (a *minioAdapter) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	opts := minio.PutObjectOptions{ContentType: contentType}
	if a.partSizeMB > 0 {
		opts.PartSize = uint64(a.partSizeMB) * 1024 * 1024
	}
	upload := func(mc *minio.Client) error {
		_, err := mc.PutObject(ctx, bucket, key, r, size, opts)
		return err
	}
	if seeker, ok := r.(io.Seeker); ok {
		c, err := a.bucketClient(bucket)
		if err != nil {
			return err
		}
		err = upload(c)
		region, redirect := bucketRedirectRegion(err)
		if !redirect {
			return err
		}
		c, ferr := a.followBucketRedirect(ctx, bucket, "", region, err)
		if ferr != nil {
			return ferr
		}
		if _, serr := seeker.Seek(0, io.SeekStart); serr != nil {
			return fmt.Errorf("回卷上传流重传失败: %w", serr)
		}
		return upload(c)
	}
	c, err := a.uploadClient(ctx, bucket)
	if err != nil {
		return err
	}
	return upload(c)
}

// uploadClient 为不可回放的上传流预解析桶所在 endpoint：一次 HEAD 桶让重定向在这
// 一步被跟随掉。重定向类失败（含不可推导的指引错误）原样返回；其余探测失败（如
// 对象级令牌对 HEAD 桶 403）不阻塞上传，退回主客户端由上传本身暴露真实错误。
func (a *minioAdapter) uploadClient(ctx context.Context, bucket string) (*minio.Client, error) {
	probeErr := a.withBucketRedirect(ctx, bucket, func(mc *minio.Client) error {
		_, err := mc.BucketExists(ctx, bucket)
		return err
	})
	if probeErr != nil {
		if _, redirect := bucketRedirectRegion(probeErr); redirect {
			return nil, probeErr
		}
		if _, redirect := permanentRedirectEndpoint(probeErr); redirect {
			return nil, probeErr
		}
		logger.Ctx(ctx).Warn("oss upload endpoint probe failed, uploading via primary endpoint",
			zap.String("bucket", bucket), zap.Error(probeErr))
	}
	return a.bucketClient(bucket)
}

func (a *minioAdapter) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	get := func(mc *minio.Client) (io.ReadCloser, int64, error) {
		obj, err := mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return nil, 0, err
		}
		info, err := obj.Stat()
		if err != nil {
			_ = obj.Close()
			return nil, 0, err
		}
		return obj, info.Size, nil
	}
	c, err := a.bucketClient(bucket)
	if err != nil {
		return nil, 0, err
	}
	obj, size, err := get(c)
	region, redirect := bucketRedirectRegion(err)
	if !redirect {
		return obj, size, err
	}
	c, ferr := a.followBucketRedirect(ctx, bucket, "", region, err)
	if ferr != nil {
		return nil, 0, ferr
	}
	return get(c)
}

// presign 不发请求、无法自己发现重定向：桶→endpoint 记忆命中时直达(此前在线操作
// 已解析)，未命中保持主客户端——今天的行为,由后续在线操作纠正。
func (a *minioAdapter) presignWith(bucket string, sign func(mc *minio.Client) (*url.URL, error)) (*url.URL, error) {
	c, err := a.bucketClient(bucket)
	if err != nil {
		return nil, err
	}
	return sign(c)
}

func (a *minioAdapter) PresignGet(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	u, err := a.presignWith(bucket, func(mc *minio.Client) (*url.URL, error) {
		return mc.PresignedGetObject(ctx, bucket, key, expiry, nil)
	})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (a *minioAdapter) PresignPut(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	u, err := a.presignWith(bucket, func(mc *minio.Client) (*url.URL, error) {
		return mc.PresignedPutObject(ctx, bucket, key, expiry)
	})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (a *minioAdapter) BucketExists(ctx context.Context, bucket string) (bool, error) {
	var exists bool
	err := a.withBucketRedirect(ctx, bucket, func(mc *minio.Client) error {
		var err error
		exists, err = mc.BucketExists(ctx, bucket)
		return err
	})
	return exists, err
}

// toObjectItem 把 minio.ObjectInfo 映射为 DTO;"文件夹"前缀表现为 Key 以 "/" 结尾且 Size 为 0。
func toObjectItem(o minio.ObjectInfo) ObjectItem {
	item := ObjectItem{
		Key: o.Key, Size: o.Size, ETag: o.ETag,
		StorageClass: o.StorageClass, ContentType: o.ContentType,
	}
	if !o.LastModified.IsZero() {
		item.LastModified = o.LastModified.Unix()
	}
	if o.Size == 0 && len(o.Key) > 0 && o.Key[len(o.Key)-1] == '/' {
		item.IsPrefix = true
	}
	return item
}

// s3StatusError 保留非 200 响应的状态码、错误体摘要，以及（阿里云
// PermanentRedirect 时）桶实际所在区域的 endpoint——文本形式给用户看，
// 结构字段给重试逻辑用。
type s3StatusError struct {
	what     string
	status   string
	summary  string
	endpoint string
}

func newS3StatusError(what string, resp *http.Response, body []byte) error {
	e := &s3StatusError{what: what, status: resp.Status, summary: describeS3ErrorBytes(body)}
	var payload struct {
		Code     string `xml:"Code"`
		Endpoint string `xml:"Endpoint"`
	}
	_ = xml.Unmarshal(body, &payload)
	if payload.Code == "PermanentRedirect" && payload.Endpoint != "" {
		e.endpoint = payload.Endpoint
	}
	return e
}

func (e *s3StatusError) Error() string {
	return fmt.Sprintf("%s: unexpected HTTP status %s: %s", e.what, e.status, e.summary)
}

// permanentRedirectEndpoint 报告错误是否为「桶在另一区域」并给出真实 endpoint。
func permanentRedirectEndpoint(err error) (endpoint string, ok bool) {
	var e *s3StatusError
	if errors.As(err, &e) {
		return e.endpoint, e.endpoint != ""
	}
	return "", false
}

func describeS3Error(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "no error body"
	}
	return describeS3ErrorBytes(data)
}

func describeS3ErrorBytes(data []byte) string {
	if len(data) == 0 {
		return "no error body"
	}
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(data, &e) != nil || e.Code == "" {
		return strings.TrimSpace(string(data))
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}
