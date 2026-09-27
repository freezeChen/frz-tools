package config

import (
	"fmt"
	"net"
	"strings"

	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/pki"
)

// RemoteConfig 是 opsd 的远程监听（迭代 5a）。
//
// **不配这一段就完全不听 TCP**：默认形态与迭代 4 逐字节一致（只有 Unix socket）。
// 这与 Nginx 适配器「可以缺席，但缺席必须有明确说法」是同一条纪律——能力是显式声明的，
// 不存在「配了一半、以为开了远程、其实没开」的中间态（那种状态由下面的校验挡掉）。
type RemoteConfig struct {
	// Listen 是 TCP 监听地址，形如 "0.0.0.0:9443"。空表示不听远程。
	Listen string `yaml:"listen"`
	// CertFile / KeyFile 是**本机**的证书与私钥（服务端身份）。
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
	// ClientCAFile 是签发客户端证书的 CA。所有由它签发的客户端都能完成握手，
	// 能做什么由 Clients 决定。
	ClientCAFile string `yaml:"clientCAFile"`
	// Clients 是白名单：**不在名单里的 CN 一律拒绝**。
	Clients []RemoteClientConfig `yaml:"clients"`
}

// RemoteClientConfig 是一个被允许连进来的身份。
type RemoteClientConfig struct {
	// CN 是客户端证书的 Common Name。它就是身份本身。
	CN string `yaml:"cn"`
	// Scope 只有两个取值：read（只能打只读端点）与 write（全部端点）。
	// 更细的角色是 5c 的事，5a 刻意只做最小的那一档。
	Scope string `yaml:"scope"`
	// Applications 限制这个身份能碰哪些应用（按应用名）。省略表示这台机器上的全部应用。
	Applications []string `yaml:"applications"`
}

// RemoteEnabled 判定是否要听远程端口。
func (c *Config) RemoteEnabled() bool { return strings.TrimSpace(c.Remote.Listen) != "" }

// FindRemoteIdentity 按客户端证书的 CN 找白名单条目。找不到时返回 (零值, false)——
// 「不在名单」是拒绝，不是错误：拒绝的判定由调用方做，这样它才能给出同一句
// REMOTE_FORBIDDEN，而不必区分「没这条」与「排在这条后面」。
//
// 返回的是 domain.RemoteIdentity 而不是配置结构：调用方（HTTP 层）只需要
// 「这个身份被允许做什么」，不该看得见配置文件长什么样。
func (c *Config) FindRemoteIdentity(cn string) (domain.RemoteIdentity, bool) {
	for _, client := range c.Remote.Clients {
		if client.CN == cn {
			return client.identity(), true
		}
	}
	return domain.RemoteIdentity{}, false
}

func (r RemoteClientConfig) identity() domain.RemoteIdentity {
	return domain.RemoteIdentity{
		CN:           r.CN,
		Scope:        domain.AccessScope(r.Scope),
		Applications: r.ApplicationsAllowed(),
	}
}

// ApplicationsAllowed 返回应用白名单的副本，避免调用方改到配置。
func (r RemoteClientConfig) ApplicationsAllowed() []string {
	return append([]string(nil), r.Applications...)
}

// validateRemote 校验 remote 段。全部问题一次性列出来，与 Validate 的其它部分同形——
// 一次改完比改一条重启一次快得多。
func (c *Config) validateRemote() []string {
	var problems []string

	if !c.RemoteEnabled() {
		// 不听远程时，这一段的其他字段都不许给：「配了但不听」是最容易误读的状态，
		// 运维以为开了远程，其实没有，而这一点从任何输出里都看不出来。
		if c.Remote.CertFile != "" || c.Remote.KeyFile != "" ||
			c.Remote.ClientCAFile != "" || len(c.Remote.Clients) > 0 {
			problems = append(problems,
				"remote.listen 为空时，remote 段的其他字段都不许给：不配 listen 就是不听远程，"+
					"配了别的字段会让人以为远程已开启")
		}
		return problems
	}

	if _, _, err := net.SplitHostPort(c.Remote.Listen); err != nil {
		problems = append(problems, fmt.Sprintf(
			"remote.listen %q 必须写成 host:port（例如 0.0.0.0:9443）", c.Remote.Listen))
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"remote.certFile", c.Remote.CertFile},
		{"remote.keyFile", c.Remote.KeyFile},
		{"remote.clientCAFile", c.Remote.ClientCAFile},
	} {
		if err := requireAbsolute(field.name, field.value); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(c.Remote.Clients) == 0 {
		problems = append(problems,
			"remote.clients 不能为空：这是白名单，空名单等于谁都进不来（要关远程请去掉 listen）")
	}

	seen := map[string]int{}
	for i, client := range c.Remote.Clients {
		field := fmt.Sprintf("remote.clients[%d]", i)
		cn := strings.TrimSpace(client.CN)
		if cn == "" {
			problems = append(problems, field+".cn is required")
		} else if first, dup := seen[cn]; dup {
			// 重复的 CN 会让「以第几条为准」变成一个必须记住的规则，而两条内容
			// 不同时，实际生效的那条与写在后面那条看起来一样合理。
			problems = append(problems, fmt.Sprintf(
				"%s.cn %q 与 remote.clients[%d] 重复：同一个身份只能有一条规则", field, cn, first))
		} else {
			seen[cn] = i
		}
		if !domain.AccessScope(client.Scope).Valid() {
			problems = append(problems, fmt.Sprintf(
				"%s.scope %q 不是已知档位（只能是 read 或 write）", field, client.Scope))
		}
		for j, application := range client.Applications {
			if strings.TrimSpace(application) == "" {
				problems = append(problems, fmt.Sprintf("%s.applications[%d] 不能为空", field, j))
			}
		}
	}

	// 私钥文件的模式与属主在这里就查，不等到 ListenTLS：启动失败的消息里越早指出
	// 是哪个文件越好，而这一步的失败原因（权限）与后面那些（端口占着）完全无关。
	//
	// keyFile 为空时不查——那已经在上面报过「必填」，再报一条「读不到空路径」
	// 只会让运维在两句话之间猜哪句才是真正要改的。
	if c.Remote.KeyFile != "" {
		if err := pki.CheckPrivateKey(c.Remote.KeyFile); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}
