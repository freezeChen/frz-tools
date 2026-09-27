// Package pki 只做一件事：把证书与私钥文件变成 crypto/tls 的配置，并在读之前
// 检查私钥文件的模式与属主。
//
// 它被 opsd（服务端）与 opsctl（客户端）共用，因此两边对「什么样的私钥才算合格」
// 的判据只有一份——一边松一边紧是最容易出的事，而松的那边不会报警。
package pki

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"syscall"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// maxPrivateKeyMode 是私钥文件允许的最宽模式。0644 的私钥等于把机器身份公开了，
// 而它不会自己变回去，所以这是硬失败而不是警告。
const maxPrivateKeyMode os.FileMode = 0o600

// CheckPrivateKey 校验私钥文件：模式不得宽于 0600，且属主必须是当前进程的有效用户。
//
// 「属主必须是运行用户」这条不是洁癖：私钥只要能被人读到就够了，而模式位在属主
// 不对时表达的是另一个人的意图。两条都不过时只报第一条，让报错指向最该先改的那处。
//
// 非 Unix 平台上没有 uid 的概念，那一半检查会跳过——本项目只面向 Linux 与 macOS，
// 这条分支存在只是为了让代码在别的平台上仍然能编译。
func CheckPrivateKey(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return domain.NewError(v1.CodeConfigInvalid, "无法读取私钥文件 %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return domain.NewError(v1.CodeConfigInvalid, "私钥路径 %s 不是一个普通文件", path)
	}
	if perm := info.Mode().Perm(); perm&^maxPrivateKeyMode != 0 {
		return domain.NewError(v1.CodeConfigInvalid,
			"私钥文件 %s 的模式是 %04o，宽于 %04o：它等价于把本机身份公开，请先 chmod 600",
			path, perm, maxPrivateKeyMode)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if euid := os.Geteuid(); int(stat.Uid) != euid {
			return domain.NewError(v1.CodeConfigInvalid,
				"私钥文件 %s 的属主 uid 是 %d，不是当前运行用户 %d，请 chown 到运行用户",
				path, stat.Uid, euid)
		}
	}
	return nil
}

// ServerTLSConfig 是 opsd 的远程监听配置：要求并校验客户端证书（mTLS）。
//
// 最低版本刻意定为 TLS 1.2 而不是 1.3：legacy 档的目标机是 CentOS 7，它的
// openssl 1.0.2 只到 TLS 1.2，要求 1.3 会让「用一台 CentOS 7 当客户端」直接连不上，
// 而那正是我们已经验证过的那一档。
//
// 这里**只验 CA，不按 CN 授权**：由这个 CA 签发的客户端证书都能完成握手，而
// 「这个身份被允许做什么」在 HTTP 层判、拒绝时给 403 + REMOTE_FORBIDDEN。
// 理由是可诊断性——若在握手期按 CN 拒掉，「证书没签对」与「身份没被授权」在客户端
// 看来会是同一句 TLS 握手失败，运维分不清该改哪一边。
func ServerTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if err := CheckPrivateKey(keyFile); err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "无法加载服务端证书与私钥: %v", err)
	}
	clientCAs, err := loadCertPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig 是 opsctl 的远程客户端配置。
//
// serverName 用来校验**对方**的证书（防中间人）。它取自运维写的地址，而不是证书里
// 的名字：判断「我连的是不是我要连的那台」只有调用方知道答案。因此给目标机签证书时，
// SAN 里必须包含你用来连它的那个名字——这条写进了迭代 5 的文档。
//
// 客户端证书是可选的：省略时仍然能建立 TLS，只是服务端（RequireAndVerifyClientCert）
// 会在握手时拒绝，报出 HOST_TLS_FAILED。让这个失败发生在对端、由我们翻译成
// 「本机没有带客户端证书」，比在本地报「参数没给全」更贴近真正的原因。
func ClientTLSConfig(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	if keyFile != "" {
		if err := CheckPrivateKey(keyFile); err != nil {
			return nil, err
		}
	}

	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		roots, err := loadCertPool(caFile)
		if err != nil {
			return nil, err
		}
		config.RootCAs = roots
	}
	if certFile != "" && keyFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, domain.NewError(v1.CodeHostTLSFailed, "无法加载客户端证书与私钥: %v", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "无法读取证书文件 %s: %v", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, domain.NewError(v1.CodeConfigInvalid, "%s 里没有可用的 PEM 证书", path)
	}
	return pool, nil
}

// CommonNameOf 取客户端证书的 CN。调用方已经过 RequireAndVerifyClientCert，
// 因此这里拿到的是一份**已验证**的身份。
func CommonNameOf(certificate *x509.Certificate) string {
	if certificate == nil {
		return ""
	}
	return certificate.Subject.CommonName
}
