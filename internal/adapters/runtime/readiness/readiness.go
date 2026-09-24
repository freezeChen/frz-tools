// Package readiness 是 RuntimeAdapter 实现共用的就绪探测语义：探测方式（tcp/http）、
// 超时与「连续成功次数」的计数。
//
// 它被 proc 与 systemd 两个适配器共用，而不是各自实现一份：两边语义必须一致，否则
// 「本地（proc）全绿、真机（systemd）挂」这类故障只会藏在合约测试的缝隙里。
// 只放与运行时无关的纯语义：这里不 import 任何适配器，也不碰文件系统。
package readiness

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/freezeChen/frz-tools/internal/domain"
)

// DefaultTimeout 是单次就绪探测（一次 TCP 连接或一次 HTTP 请求）的超时。
// 它必须是有限值：Health 会被上层轮询调用，一次探测挂住就等于整个查询接口挂住。
const DefaultTimeout = 3 * time.Second

// Check 探测一次就绪目标，返回是否就绪与人类可读的原因。
// 原因文本会进入 RuntimeHealth.Detail，因此它是对外可见的输出，不是日志文案。
func Check(ctx context.Context, target domain.SpecReadiness, timeout time.Duration) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch target.Type {
	case domain.ReadinessTCP:
		conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", target.Target)
		if err != nil {
			return false, "TCP 探测失败: " + err.Error()
		}
		_ = conn.Close()
		return true, "TCP 探测通过: " + target.Target

	case domain.ReadinessHTTP:
		request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target.Target, nil)
		if err != nil {
			return false, "构造请求失败: " + err.Error()
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false, "HTTP 探测失败: " + err.Error()
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return false, fmt.Sprintf("HTTP 探测返回 %d", response.StatusCode)
		}
		return true, fmt.Sprintf("HTTP 探测通过: %d", response.StatusCode)
	}
	// tcp/http 之外的类型在 manifest 校验阶段就会被拒；这里返回 false 而不是 panic，
	// 是为了让「规格绕过校验」这种误用也不至于把进程打崩。
	return false, "不支持的就绪检查类型: " + string(target.Type)
}

// Tracker 记录每个 key（适配器用 unit 名作 key）连续成功的次数。
//
// 计数只在内存里：它描述的是「这一次运行里连续通过了多少次」，重启后重新计数是对的，
// 把历史计数持久化反而会让重启后的第一次探测就报就绪。
type Tracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewTracker() *Tracker {
	return &Tracker{counts: map[string]int{}}
}

// Record 记录一次成功并返回连续成功次数。
func (t *Tracker) Record(key string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[key]++
	return t.counts[key]
}

// Reset 把计数清零：探测失败、或进程根本不在运行时调用，
// 否则「连续成功」会退化成「累计成功」。
func (t *Tracker) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[key] = 0
}

// Forget 丢掉某个 key 的全部状态。停止之后必须调用，
// 否则残留的计数会让下一次启动少探几次就报告就绪。
func (t *Tracker) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, key)
}
