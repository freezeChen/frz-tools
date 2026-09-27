package client

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// transportError 把一次 HTTP 传输失败翻译成一句能直接照着改的报错。
//
// 远程形态下（迭代 5a）它必须区分两件事：**连不上**（网络、对端没在跑、端口不对）
// 与**连上了但 TLS 没过**（证书不受信、过期、SAN 不匹配、本机没带证书）。这两件事
// 是两个不同的人、在不同的地方修的；合成一句「连不上」等于把诊断成本转给运维。
//
// 本机 socket 形态保持原样：那条路上这些都是「opsd 没起来」，没有第二种可能。
func (c *Client) transportError(err error) error {
	if !c.IsRemote() {
		return domain.NewError(v1.CodeInternal, "cannot reach opsd at %s: %v", c.socket, err)
	}
	return classifyRemoteError(c.address, err)
}

// classifyRemoteError 是纯函数，便于直接对构造出来的错误做断言。
func classifyRemoteError(address string, err error) error {
	// 先看证书类错误。它们大多数是 x509 的类型化错误，可以直接 errors.As；
	// 只有 TLS alert（对端在对端报的）没有类型，只能看文本。
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostnameMismatch x509.HostnameError
		invalidCert      x509.CertificateInvalidError
		recordHeader     tls.RecordHeaderError
	)
	message := err.Error()
	switch {
	case errors.As(err, &unknownAuthority):
		return domain.NewError(v1.CodeHostTLSFailed,
			"%s 的证书不被信任：--ca-cert 指的是签发它的那个 CA 吗", address)
	case errors.As(err, &hostnameMismatch):
		return domain.NewError(v1.CodeHostTLSFailed,
			"%s 的证书里的名字与这个地址不匹配：证书的 SAN 必须包含你用来连它的名字（%v）",
			address, hostnameMismatch)
	case errors.As(err, &invalidCert):
		return domain.NewError(v1.CodeHostTLSFailed,
			"%s 的证书无效或已过期：%v", address, invalidCert)
	case errors.As(err, &recordHeader):
		// 对端回的不是 TLS：最常见的是把地址指到了一个纯 HTTP 端口。
		return domain.NewError(v1.CodeHostTLSFailed,
			"%s 看起来不是 TLS 端口（对端的应答不是 TLS）：确认它是不是 opsd 的 remote 监听", address)
	}

	// 对端要求客户端证书而本机没带：Go 的服务端会回一句 tls alert，
	// 客户端这边只有文本可看。这是最常见的一种「第一次连就失败」，值得单独说清楚。
	if strings.Contains(message, "certificate required") {
		return domain.NewError(v1.CodeHostTLSFailed,
			"%s 要求客户端证书，而本机没有提供：请用 --client-cert / --client-key 指定，"+
				"或检查私钥文件的权限", address)
	}
	if strings.Contains(message, "tls:") {
		return domain.NewError(v1.CodeHostTLSFailed, "与 %s 的 TLS 握手失败：%v", address, err)
	}

	// 剩下的是「根本没连上」。用 *net.OpError 的 op 区分不出来「连接被拒」与
	// 「DNS 解析不了」——两者都在这一档，报错原文里带着具体原因。
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return domain.NewError(v1.CodeHostUnreachable,
			"连不上 %s：%v（对端的 opsd 在跑吗？remote.listen 开的是这个地址吗？）", address, err)
	}
	return domain.NewError(v1.CodeHostUnreachable, "连不上 %s：%v", address, err)
}
