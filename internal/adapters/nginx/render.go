package nginx

import (
	"strconv"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// RenderManaged 渲染受管配置的正文（迭代 4 规格 D7）。
//
// 它是**纯函数**：内容只由规格与目标槽位决定，因此在没有 nginx 的机器上也能逐字断言。
//
// 这里不需要任何转义：进入正文的每一个值都在 domain 层按字符集校验过——upstream 名与
// serverName 有各自的字符集规则（空白、分号、换行都进不来），端口是整数。这与 unit 渲染
// 那边要显式 checkRenderablePath 的情形不同（那边有路径这类自由文本）。
//
// target 是要接流量的那个槽位；releaseID / version 只进头注释——出问题时运维第一眼要看
// 的事实（「现在指向谁」）就在文件开头，与 unit 头注释记录档位/版本同一个理由。
func RenderManaged(spec *domain.ApplicationSpec, target domain.Slot, releaseID, version string) (string, error) {
	if spec == nil {
		return "", domain.NewError(v1.CodeManifestInvalid, "渲染 Nginx 配置需要非空的应用规格")
	}
	if !spec.BlueGreen() {
		return "", domain.NewError(v1.CodeManifestInvalid,
			"Nginx 配置只服务蓝绿应用：应用 %s 没有声明 exec.slots", spec.Application)
	}
	if !spec.Nginx.Configured() {
		return "", domain.NewError(v1.CodeManifestInvalid,
			"蓝绿应用 %s 必须声明 nginx 段（listen 是必填的对外端口）", spec.Application)
	}
	slot, ok := spec.SlotOf(target)
	if !ok {
		return "", domain.NewError(v1.CodeInvalidRequest, "槽位 %s 不存在于该应用的 exec.slots 里", target)
	}
	if len(slot.Ports) == 0 {
		return "", domain.NewError(v1.CodeManifestInvalid, "槽位 %s 没有声明端口", target)
	}
	// upstream 指向该槽位的**第一个**端口。多端口是允许的（管理端口、指标端口都可能），
	// 但 upstream 只能指一个——把「对外流量的那一个」定成第一个，比让工具去猜哪个「像」
	// 业务端口要诚实。
	port := slot.Ports[0]
	upstream := spec.Nginx.EffectiveUpstreamName(spec.Application)

	var b strings.Builder
	b.WriteString("# 本文件由 opsd 生成，请勿手工编辑；由蓝绿部署流程维护。\n")
	b.WriteString("# 现在指向：" + string(target))
	if version != "" {
		b.WriteString("（版本 " + version + "）")
	}
	if releaseID != "" {
		b.WriteString("（release " + releaseID + "）")
	}
	b.WriteString("\n")
	b.WriteString("# 切流＝改这里的 server 指向 + nginx -s reload；旧 worker 会在排空后自行退出。\n\n")

	b.WriteString("upstream " + upstream + " {\n")
	b.WriteString("    server 127.0.0.1:" + strconv.Itoa(port) + ";\n")
	b.WriteString("    keepalive 16;\n")
	b.WriteString("}\n\n")

	b.WriteString("server {\n")
	b.WriteString("    listen " + strconv.Itoa(spec.Nginx.Listen) + ";\n")
	if spec.Nginx.ServerName != "" {
		b.WriteString("    server_name " + spec.Nginx.ServerName + ";\n")
	}
	b.WriteString("    location / {\n")
	b.WriteString("        proxy_pass http://" + upstream + ";\n")
	b.WriteString("        proxy_http_version 1.1;\n")
	// keepalive 要求把 Connection 头清空，否则每个请求都会新建连接，
	// 而 upstream 里的 keepalive 只是「允许保持」，不保证会被用到。
	b.WriteString("        proxy_set_header Connection \"\";\n")
	b.WriteString("        proxy_set_header Host $host;\n")
	b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
	b.WriteString("        client_max_body_size 1m;\n")
	b.WriteString("        proxy_connect_timeout 5s;\n")
	b.WriteString("        proxy_read_timeout 60s;\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// ParseManaged 从受管配置正文里读回它指向哪个槽位。
//
// 它**只认自己写出去的那一行**（`server 127.0.0.1:<port>;`）：对账要的是「这份文件说的是
// 什么」，不是「nginx 现在跑的是什么」。解析失败（文件被人手工改过、格式不认识）返回空槽位
// ——调用方据此报告「读不出来」而不是猜一个。
func ParseManaged(content string, spec *domain.ApplicationSpec) (domain.Slot, bool) {
	port, ok := upstreamPort(content)
	if !ok {
		return "", false
	}
	for _, slot := range domain.Slots {
		slotSpec, present := spec.SlotOf(slot)
		if !present || len(slotSpec.Ports) == 0 {
			continue
		}
		// 只看第一个端口：与 RenderManaged 的规则同源（upstream 指向第一个）。
		if slotSpec.Ports[0] == port {
			return slot, true
		}
	}
	return "", false
}

// upstreamPort 从正文里取出 upstream 那个 server 条目的端口。
func upstreamPort(content string) (int, bool) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "server ") || !strings.HasSuffix(trimmed, ";") {
			continue
		}
		address := strings.TrimSuffix(strings.TrimPrefix(trimmed, "server "), ";")
		colon := strings.LastIndex(address, ":")
		if colon < 0 {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(address[colon+1:]))
		if err != nil {
			continue
		}
		return port, true
	}
	return 0, false
}
