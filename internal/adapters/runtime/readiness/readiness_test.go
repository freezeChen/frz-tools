package readiness_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freezeChen/frz-tools/internal/adapters/runtime/readiness"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 就绪语义是两个适配器共用的：这里断言的是对外可见的结果（是否就绪 + Detail 文本），
// 而不是内部实现，因为 Detail 会进 RuntimeHealth 交给用户看。
func TestCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("TCP 目标可达", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer listener.Close()

		ready, detail := readiness.Check(ctx, domain.SpecReadiness{
			Type: domain.ReadinessTCP, Target: listener.Addr().String(),
		}, readiness.DefaultTimeout)
		if !ready {
			t.Fatalf("可达的 TCP 目标必须报告就绪，detail=%q", detail)
		}
		if !strings.Contains(detail, listener.Addr().String()) {
			t.Fatalf("detail 应当带上目标地址，got %q", detail)
		}
	})

	t.Run("TCP 目标不可达", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		target := listener.Addr().String()
		_ = listener.Close() // 立刻关掉，得到一个确定没人监听的地址

		ready, detail := readiness.Check(ctx, domain.SpecReadiness{
			Type: domain.ReadinessTCP, Target: target,
		}, readiness.DefaultTimeout)
		if ready {
			t.Fatalf("不可达的 TCP 目标不得报告就绪，detail=%q", detail)
		}
		if !strings.Contains(detail, "TCP 探测失败") {
			t.Fatalf("detail 应当说明探测失败，got %q", detail)
		}
	})

	t.Run("HTTP 2xx 才就绪", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			want   bool
		}{
			{name: "200", status: http.StatusOK, want: true},
			{name: "204", status: http.StatusNoContent, want: true},
			{name: "404", status: http.StatusNotFound, want: false},
			{name: "503", status: http.StatusServiceUnavailable, want: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
				})}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				go func() { _ = server.Serve(listener) }()
				defer func() { _ = server.Close() }()

				ready, detail := readiness.Check(ctx, domain.SpecReadiness{
					Type: domain.ReadinessHTTP, Target: "http://" + listener.Addr().String() + "/healthz",
				}, readiness.DefaultTimeout)
				if ready != tc.want {
					t.Fatalf("status=%d: ready=%v detail=%q, want %v", tc.status, ready, detail, tc.want)
				}
			})
		}
	})

	t.Run("不支持的类型不报就绪", func(t *testing.T) {
		ready, detail := readiness.Check(ctx, domain.SpecReadiness{
			Type: domain.ReadinessType("exec"), Target: "/bin/true",
		}, readiness.DefaultTimeout)
		if ready {
			t.Fatal("未知就绪类型不得报告就绪")
		}
		if !strings.Contains(detail, "不支持的就绪检查类型") {
			t.Fatalf("detail 应当说明类型不支持，got %q", detail)
		}
	})

	t.Run("超时上界", func(t *testing.T) {
		// 对端永不响应：只有超时能结束这次探测。
		release := make(chan struct{})
		server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release })}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go func() { _ = server.Serve(listener) }()
		defer func() { close(release); _ = server.Close() }()

		started := time.Now()
		ready, _ := readiness.Check(ctx, domain.SpecReadiness{
			Type: domain.ReadinessHTTP, Target: "http://" + listener.Addr().String() + "/healthz",
		}, 50*time.Millisecond)
		elapsed := time.Since(started)

		if ready {
			t.Fatal("对端不应答时不得报告就绪")
		}
		if elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("探测必须被 timeout 界住（约 50ms），实际 %s", elapsed)
		}
	})
}

// 「连续成功」必须是连续的：失败清零、停止丢弃，否则会退化成「累计成功」，
// 于是应用只要历史上通过过两次，之后每次探活都会立刻报就绪。
func TestTracker(t *testing.T) {
	tracker := readiness.NewTracker()
	const key = "orders.service"

	if got := tracker.Record(key); got != 1 {
		t.Fatalf("第一次成功应当是 1，got %d", got)
	}
	if got := tracker.Record(key); got != 2 {
		t.Fatalf("第二次成功应当是 2，got %d", got)
	}

	tracker.Reset(key)
	if got := tracker.Record(key); got != 1 {
		t.Fatalf("清零后应当从 1 重新开始，got %d", got)
	}

	tracker.Forget(key)
	if got := tracker.Record(key); got != 1 {
		t.Fatalf("丢弃后应当从 1 重新开始，got %d", got)
	}

	// 不同的 key 互不影响：两个应用的计数不能串。
	if got := tracker.Record("billing.service"); got != 1 {
		t.Fatalf("另一个 key 应当独立计数，got %d", got)
	}
}

func TestTrackerIsConcurrencySafe(t *testing.T) {
	tracker := readiness.NewTracker()
	const (
		key       = "app"
		goroutine = 8
		perWorker = 100
	)

	var wg sync.WaitGroup
	results := make(chan int, goroutine*perWorker)
	for i := 0; i < goroutine; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				results <- tracker.Record(key)
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := map[int]bool{}
	for value := range results {
		if seen[value] {
			t.Fatalf("计数值 %d 重复出现：Record 必须在锁内递增", value)
		}
		seen[value] = true
	}
	if len(seen) != goroutine*perWorker {
		t.Fatalf("计数不连续：期望 %d 个不同的值，得到 %d", goroutine*perWorker, len(seen))
	}
}
