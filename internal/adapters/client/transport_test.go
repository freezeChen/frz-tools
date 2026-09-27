package client

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 远程形态下「连不上」与「证书不对」是两个人去修的两件事，必须分开报。
// 这个函数是那条分界的全部逻辑，因此逐类钉住。
func TestClassifyRemoteErrorSeparatesNetworkFromTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want v1.ErrorCode
	}{
		{
			name: "连接被拒",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			want: v1.CodeHostUnreachable,
		},
		{
			name: "对端证书不被信任",
			err:  x509.UnknownAuthorityError{Cert: &x509.Certificate{}},
			want: v1.CodeHostTLSFailed,
		},
		{
			name: "证书里的名字与地址不匹配",
			err:  x509.HostnameError{Certificate: &x509.Certificate{Subject: pkix.Name{CommonName: "old-name"}}, Host: "10.0.0.5"},
			want: v1.CodeHostTLSFailed,
		},
		{
			name: "证书过期",
			err:  x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired},
			want: v1.CodeHostTLSFailed,
		},
		{
			name: "对端根本不是 TLS",
			err:  tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
			want: v1.CodeHostTLSFailed,
		},
		{
			name: "对端要求客户端证书而本机没带",
			err:  errors.New(`remote error: tls: certificate required`),
			want: v1.CodeHostTLSFailed,
		},
		{
			name: "其它 TLS 失败",
			err:  errors.New("remote error: tls: handshake failure"),
			want: v1.CodeHostTLSFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRemoteError("10.0.0.5:9443", tc.err)
			if code := domain.CodeOf(got); code != tc.want {
				t.Fatalf("want %s, got %s (%v)", tc.want, code, got)
			}
			// 报错里必须带上地址：一个不知道「连的是哪台」的证书错误没法排查。
			if !strings.Contains(domain.MessageOf(got), "10.0.0.5:9443") {
				t.Fatalf("报错里应当带上地址：%v", got)
			}
		})
	}
}

// 本机 socket 那条路的报错保持不变：那条路上没有第二种可能，多说反而误导。
func TestLocalTransportErrorKeepsOriginalWording(t *testing.T) {
	c := New("/run/opsd/opsd.sock", 0)
	got := c.transportError(errors.New("dial unix: no such file"))
	if code := domain.CodeOf(got); code != v1.CodeInternal {
		t.Fatalf("want INTERNAL, got %s", code)
	}
	if !strings.Contains(domain.MessageOf(got), "/run/opsd/opsd.sock") {
		t.Fatalf("报错里应当带上 socket 路径：%v", got)
	}
}

func TestClientReportsWhetherItIsRemote(t *testing.T) {
	local := New("/run/opsd/opsd.sock", 0)
	if local.IsRemote() {
		t.Fatal("socket 客户端不该是远程")
	}
	if !strings.Contains(local.Target(), "/run/opsd/opsd.sock") {
		t.Fatalf("本机目标的描述应当带上 socket：%s", local.Target())
	}

	remote := NewRemote("10.0.0.5:9443", &tls.Config{}, 0)
	if !remote.IsRemote() {
		t.Fatal("NewRemote 建出来的应当是远程客户端")
	}
	if !strings.Contains(remote.Target(), "10.0.0.5:9443") {
		t.Fatalf("远程目标的描述应当带上地址：%s", remote.Target())
	}
	// 远程必须走 https：http.Transport 按 URL 的 scheme 决定要不要握手，
	// 用 http:// 的话配了 TLSClientConfig 也不会生效——那会静默降级成明文。
	if remote.scheme() != "https://opsd" {
		t.Fatalf("远程的 scheme 必须是 https，得到 %q", remote.scheme())
	}
	if local.scheme() != "http://opsd" {
		t.Fatalf("本机的 scheme 是 http，得到 %q", local.scheme())
	}
}
