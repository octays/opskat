package connpool

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/octays/opskat/internal/assetconn"
	"github.com/octays/opskat/internal/model/entity/asset_entity"
	"github.com/octays/opskat/internal/pkg/netdial"
	"go.uber.org/zap"
)

func init() {
	assetconn.RegisterInvalidator("oss", func(_ context.Context, assetID int64) error {
		InvalidateOSS(assetID)
		return nil
	})
}

// buildMinioOptions 从 OSS 配置 + 解密后的密钥推导 minio 端点与选项(纯函数,单测)。
func buildMinioOptions(ctx context.Context, cfg *asset_entity.OSSConfig, secret string) (string, *minio.Options, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return "", nil, fmt.Errorf("oss endpoint is empty")
	}
	secure := cfg.UseSSL
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return "", nil, fmt.Errorf("parse oss endpoint: %w", err)
		}
		secure = u.Scheme == "https"
		endpoint = u.Host
	}
	// use_path_style is an explicit addressing-mode choice. BucketLookupAuto
	// falls back to path-style for providers it does not recognize (including
	// Tencent COS), so it would silently ignore the user's virtual-hosted choice.
	lookup := minio.BucketLookupDNS
	if cfg.UsePathStyle {
		lookup = minio.BucketLookupPath
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = netdial.Default().DialContext
	if cfg.ConnectTimeout > 0 {
		timeout := time.Duration(cfg.ConnectTimeout) * time.Second
		transport.DialContext = (&netdial.Dialer{Dialer: net.Dialer{Timeout: timeout}}).DialContext
		transport.TLSHandshakeTimeout = timeout
	}
	if cfg.ProxyChain != nil {
		dial, err := ProxyChainDialContext(ctx, cfg.ProxyChain)
		if err != nil {
			return "", nil, fmt.Errorf("resolve oss proxy chain: %w", err)
		}
		transport.DialContext = dial
	}
	if cfg.SkipTLSVerify {
		// explicit user setting for private S3-compatible endpoints (gosec G402 is excluded project-wide)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, secret, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    transport,
	}, nil
}

// DialOSS 新建一个 minio 客户端(HTTP 客户端,无需显式 Close)。
func DialOSS(ctx context.Context, cfg *asset_entity.OSSConfig, secret string) (*minio.Client, error) {
	endpoint, opts, err := buildMinioOptions(ctx, cfg, secret)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("dial oss: %w", err)
	}
	return client, nil
}

type ossPool struct {
	mu         sync.Mutex
	clients    map[int64]*minio.Client
	transports map[int64]http.RoundTripper
}

var globalOSSPool = &ossPool{clients: map[int64]*minio.Client{}, transports: map[int64]http.RoundTripper{}}

// GetOrDialOSS 返回缓存的 minio 客户端,没有则新建并缓存。
func GetOrDialOSS(ctx context.Context, assetID int64, cfg *asset_entity.OSSConfig, secret string) (*minio.Client, error) {
	c, _, err := GetOrDialOSSWithTransport(ctx, assetID, cfg, secret)
	return c, err
}

func GetOrDialOSSWithTransport(ctx context.Context, assetID int64, cfg *asset_entity.OSSConfig, secret string) (*minio.Client, http.RoundTripper, error) {
	logger.Ctx(ctx).Info("oss connection open start", zap.Int64("assetId", assetID))
	globalOSSPool.mu.Lock()
	defer globalOSSPool.mu.Unlock()
	if c, ok := globalOSSPool.clients[assetID]; ok {
		logger.Ctx(ctx).Info("oss connection open end", zap.Int64("assetId", assetID), zap.Bool("cached", true))
		return c, globalOSSPool.transports[assetID], nil
	}
	endpoint, opts, err := buildMinioOptions(ctx, cfg, secret)
	if err != nil {
		return nil, nil, err
	}
	c, err := minio.New(endpoint, opts)
	if err != nil {
		logger.Ctx(ctx).Error("oss connection open fail", zap.Int64("assetId", assetID), zap.Error(err))
		return nil, nil, err
	}
	if assetID > 0 {
		globalOSSPool.clients[assetID] = c
		globalOSSPool.transports[assetID] = opts.Transport
	}
	logger.Ctx(ctx).Info("oss connection open end", zap.Int64("assetId", assetID), zap.Bool("cached", false))
	return c, opts.Transport, nil
}

// InvalidateOSS 丢弃某资产的缓存客户端(配置更新/删除时调用)。
func InvalidateOSS(assetID int64) {
	logger.Default().Info("oss connection close start", zap.Int64("assetId", assetID))
	globalOSSPool.mu.Lock()
	delete(globalOSSPool.clients, assetID)
	delete(globalOSSPool.transports, assetID)
	globalOSSPool.mu.Unlock()
	logger.Default().Info("oss connection close end", zap.Int64("assetId", assetID))
}

// DialOSSEndpoint 以同一份凭据与传输配置拨一个「指定 endpoint」的 minio 客户端。
// 供 oss_svc 在收到 PermanentRedirect（桶在另一区域）时按响应里给出的真实
// endpoint 重签重试；endpoint 形如 oss-cn-beijing.aliyuncs.com（无 scheme，
// 沿用 cfg 的 TLS 选择），带 scheme 时以 scheme 为准。AWS 的 Endpoint 带桶名
// 前缀（mybucket.s3.us-west-2.amazonaws.com），拨号前先归一化。
func DialOSSEndpoint(ctx context.Context, cfg *asset_entity.OSSConfig, secret, endpoint string) (*minio.Client, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = normalizeRedirectEndpoint(endpoint)
	}
	redirected := *cfg
	redirected.Endpoint = endpoint
	// scheme 在 buildMinioOptions 里决定 secure；无 scheme 的重定向目标沿用原配置。
	if !strings.Contains(endpoint, "://") && strings.Contains(cfg.Endpoint, "://") {
		if u, err := url.Parse(cfg.Endpoint); err == nil {
			redirected.Endpoint = u.Scheme + "://" + endpoint
		}
	}
	redirected.Region = redirectRegion(endpoint)
	endpoint, opts, err := buildMinioOptions(ctx, &redirected, secret)
	if err != nil {
		return nil, err
	}
	return minio.New(endpoint, opts)
}

// awsS3CoreHost 匹配裸 AWS S3 区域 endpoint：s3[.dualstack|-fips][.-]<region>.amazonaws.com。
// 全局 s3.amazonaws.com 与桶名前缀形态均不匹配（前者无区域，后者由 normalizeRedirectEndpoint 先剥）。
var awsS3CoreHost = regexp.MustCompile(`^(s3(?:-fips)?)(?:\.dualstack)?[.-]([a-z0-9-]+)\.amazonaws\.com$`)

// redirectRegion 从重定向 endpoint 推导 V4 签名区域：oss-cn-beijing.aliyuncs.com →
// cn-beijing，s3.us-west-2.amazonaws.com → us-west-2。带着原区域（如 cn-hangzhou）
// 签新 endpoint 会被拒签。非 aliyuncs / 非 AWS S3 形态返回空串，交给 minio 自行解析桶区域。
func redirectRegion(endpoint string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if strings.HasSuffix(host, ".aliyuncs.com") {
		// 区域在首标签：oss-cn-beijing.aliyuncs.com → cn-beijing（内部形态
		// oss-cn-hangzhou-internal 同理）。曾先把 .aliyuncs.com 后缀裁掉再按
		// “.”切分——唯一的点被裁掉，恒返回空串，签名区域从未生效过。
		first := host
		if before, _, ok := strings.Cut(host, "."); ok {
			first = before
		}
		if strings.HasPrefix(first, "oss-") {
			return strings.TrimPrefix(first, "oss-")
		}
		return ""
	}
	if m := awsS3CoreHost.FindStringSubmatch(host); m != nil {
		return m[2]
	}
	return ""
}

// awsS3HostSuffix 在 host 尾部（标签边界处）定位裸 AWS S3 endpoint（含全局形态），
// 捕获组即该 endpoint 本身；仅当 host 以它开头（无桶名前缀）时不匹配。
var awsS3HostSuffix = regexp.MustCompile(`(?:^|\.)(s3(?:-fips)?(?:\.dualstack)?[.-][a-z0-9-]+\.amazonaws\.com|s3\.amazonaws\.com)$`)

// normalizeRedirectEndpoint 剥掉 AWS 重定向 endpoint 的桶名前缀：
// <Endpoint> 是虚拟主机形态（mybucket.s3.us-west-2.amazonaws.com），直接拨号会在
// DNS 寻址形态下叠成 mybucket.mybucket.s3...。尾部能定位到裸 AWS S3 endpoint 且
// 前面还有标签时剥掉前缀，其余形态（阿里云本就裸域名、私有链接、自定义）原样返回。
func normalizeRedirectEndpoint(endpoint string) string {
	m := awsS3HostSuffix.FindStringSubmatch(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"))
	if m == nil || m[0] == m[1] {
		return endpoint
	}
	scheme := ""
	if strings.HasPrefix(endpoint, "https://") {
		scheme = "https://"
	} else if strings.HasPrefix(endpoint, "http://") {
		scheme = "http://"
	}
	return scheme + m[1]
}

// AWSRegionalEndpoint 由「资产当前 endpoint + PermanentRedirect 响应给出的桶真实区域」
// 推导该区域的 AWS S3 endpoint（保留 dualstack / FIPS 前缀）。仅 AWS S3 形态可推导；
// 阿里云/自定义/私有链接返回 false——那些形态改写 endpoint 会绕开用户的组网选择，
// 由调用方转为提示用户改配置。
func AWSRegionalEndpoint(host, region string) (string, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
	if host == "s3.amazonaws.com" {
		return fmt.Sprintf("s3.%s.amazonaws.com", region), true
	}
	if m := awsS3CoreHost.FindStringSubmatch(host); m != nil {
		prefix := m[1]
		if strings.Contains(host, ".dualstack.") {
			prefix += ".dualstack"
		}
		return fmt.Sprintf("%s.%s.amazonaws.com", prefix, region), true
	}
	return "", false
}
