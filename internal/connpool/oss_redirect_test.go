package connpool

import "testing"

// redirectRegion 推导 V4 签名区域：桶在 cn-beijing 而资产配 cn-hangzhou 时，
// PermanentRedirect 给出的 oss-cn-beijing.aliyuncs.com 必须以 cn-beijing 签名，
// 沿用旧区域会被阿里云拒签。AWS 的重定向 endpoint（s3.us-west-2.amazonaws.com）
// 同理必须以桶真实区域签名。非 aliyuncs / 非 AWS S3 形态返回空串（minio 自行解析）。
func TestRedirectRegion(t *testing.T) {
	cases := []struct{ endpoint, want string }{
		{"oss-cn-beijing.aliyuncs.com", "cn-beijing"},
		{"oss-cn-hangzhou-internal.aliyuncs.com", "cn-hangzhou-internal"},
		{"https://oss-ap-southeast-1.aliyuncs.com", "ap-southeast-1"},
		{"s3.us-west-2.amazonaws.com", "us-west-2"},
		{"s3-us-west-2.amazonaws.com", "us-west-2"},
		{"s3.dualstack.eu-central-1.amazonaws.com", "eu-central-1"},
		{"s3-fips.us-gov-west-1.amazonaws.com", "us-gov-west-1"},
		{"https://s3.ap-northeast-1.amazonaws.com", "ap-northeast-1"},
		// 全局 endpoint 无区域可推导；私有链接/自定义形态返回空串。
		{"s3.amazonaws.com", ""},
		{"bucket.vpce-1.s3.us-west-2.vpce.amazonaws.com", ""},
		{"s3.example.com", ""},
		{"storage.internal", ""},
	}
	for _, c := range cases {
		if got := redirectRegion(c.endpoint); got != c.want {
			t.Fatalf("redirectRegion(%q) = %q, want %q", c.endpoint, got, c.want)
		}
	}
}

// normalizeRedirectEndpoint 剥掉 AWS 重定向 endpoint 里携带的桶名前缀：
// AWS 的 <Endpoint> 是虚拟主机形态（mybucket.s3.us-west-2.amazonaws.com），
// 直接拨号会在 DNS 寻址形态下叠成 mybucket.mybucket.s3...。阿里云的 Endpoint
// 本就是裸区域域名，非 AWS 形态原样返回。
func TestNormalizeRedirectEndpoint(t *testing.T) {
	cases := []struct{ endpoint, want string }{
		{"mybucket.s3.us-west-2.amazonaws.com", "s3.us-west-2.amazonaws.com"},
		{"mybucket.s3.dualstack.us-west-2.amazonaws.com", "s3.dualstack.us-west-2.amazonaws.com"},
		{"mybucket.s3-fips.us-gov-west-1.amazonaws.com", "s3-fips.us-gov-west-1.amazonaws.com"},
		{"my-bucket.with-dots.s3.us-east-1.amazonaws.com", "s3.us-east-1.amazonaws.com"},
		// 已是裸 endpoint 或无法识别的形态：原样返回。
		{"s3.us-west-2.amazonaws.com", "s3.us-west-2.amazonaws.com"},
		{"mybucket.s3.amazonaws.com", "s3.amazonaws.com"},
		{"oss-cn-beijing.aliyuncs.com", "oss-cn-beijing.aliyuncs.com"},
		{"redirect.internal", "redirect.internal"},
		{"mybucket.storage.example.com", "mybucket.storage.example.com"},
	}
	for _, c := range cases {
		if got := normalizeRedirectEndpoint(c.endpoint); got != c.want {
			t.Fatalf("normalizeRedirectEndpoint(%q) = %q, want %q", c.endpoint, got, c.want)
		}
	}
}

// AWSRegionalEndpoint 由「当前 endpoint + 301 响应给出的桶真实区域」推导区域 endpoint：
// 仅对 AWS S3 形态可推导（保留 dualstack / FIPS 前缀），其余（阿里云/自定义/私有链接）
// 返回 false，由调用方转为可操作的错误提示。
func TestAWSRegionalEndpoint(t *testing.T) {
	cases := []struct {
		host, region, want string
		ok                 bool
	}{
		{"s3.amazonaws.com", "us-west-2", "s3.us-west-2.amazonaws.com", true},
		{"s3.us-east-1.amazonaws.com", "us-west-2", "s3.us-west-2.amazonaws.com", true},
		{"s3.dualstack.us-east-1.amazonaws.com", "eu-central-1", "s3.dualstack.eu-central-1.amazonaws.com", true},
		{"s3-fips.us-gov-west-1.amazonaws.com", "us-gov-east-1", "s3-fips.us-gov-east-1.amazonaws.com", true},
		{"oss-cn-hangzhou.aliyuncs.com", "cn-beijing", "", false},
		{"minio.internal:9000", "us-west-2", "", false},
		{"bucket.vpce-1.s3.us-west-2.vpce.amazonaws.com", "us-west-1", "", false},
	}
	for _, c := range cases {
		got, ok := AWSRegionalEndpoint(c.host, c.region)
		if ok != c.ok || got != c.want {
			t.Fatalf("AWSRegionalEndpoint(%q, %q) = (%q, %v), want (%q, %v)", c.host, c.region, got, ok, c.want, c.ok)
		}
	}
}
