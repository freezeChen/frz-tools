// Package runtimecontract 是所有 RuntimeAdapter 实现都必须通过的合约测试。
//
// 它的价值不在「测出 bug」，而在钉住两个实现之间的语义一致性：proc 适配器是
// macOS 与 CI 上的替身，如果它的行为与 systemd 不同，本地测试通过就毫无意义。
package runtimecontract

import (
	"context"
	"testing"

	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Harness 是合约测试需要宿主提供的东西。适配器自己无法凭空造出「本机存在的
// 长驻命令」与「一个真实监听的就绪目标」，因此由调用方注入。
type Harness struct {
	Adapter application.RuntimeAdapter

	// NewSpec 返回一个合法且可启动的规格。name 已经过清洗，可直接用作应用名；
	// 每次调用必须返回互不冲突的应用名，否则子测试之间会互相污染。
	NewSpec func(t *testing.T, name string) *domain.ApplicationSpec

	// Serve 在规格声明的就绪目标上启动一个真实监听，使就绪检查能够通过。
	// 返回销毁函数。
	Serve func(t *testing.T, spec *domain.ApplicationSpec) func()
}

func Run(t *testing.T, factory func(t *testing.T) Harness) {
	t.Helper()

	// counter 保证每个子测试拿到不同的应用名。
	counter := 0
	next := func(t *testing.T) (*application.RuntimeAdapter, *domain.ApplicationSpec) {
		counter++
		harness := factory(t)
		spec := harness.NewSpec(t, specName(counter))
		adapter := harness.Adapter
		return &adapter, spec
	}

	t.Run("Validate 对合法规格通过且无副作用", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Validate(ctx, spec); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		// 「无副作用」的判据：校验之后进程仍未运行，也没有被 Prepare 过的痕迹
		// 影响状态判断。
		status, err := (*adapter).Status(ctx, spec)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status == domain.RuntimeActive {
			t.Fatalf("Validate 不得启动任何东西，got status=%s", status)
		}
	})

	t.Run("Validate 拒绝非法规格", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		broken := *spec
		broken.Exec.Argv = nil // argv 为空，spec.Validate 必然失败
		if err := (*adapter).Validate(ctx, &broken); err == nil {
			t.Fatal("非法规格必须被 Validate 拒绝")
		}
	})

	t.Run("未 Prepare 直接 Start 被拒绝", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Start(ctx, spec); err == nil {
			t.Fatal("未 Prepare 就 Start 必须被拒绝，否则会凭空跑起一个不受管的进程")
		}
	})

	t.Run("Prepare 幂等", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("first Prepare: %v", err)
		}
		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("second Prepare 必须幂等: %v", err)
		}
		status, err := (*adapter).Status(ctx, spec)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status == domain.RuntimeActive {
			t.Fatalf("Prepare 不得启动进程，got status=%s", status)
		}
	})

	t.Run("Health 在未启动时不就绪", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		health, err := (*adapter).Health(ctx, spec)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if health.Ready {
			t.Fatal("进程未运行时不得报告就绪")
		}
		if health.CheckedAt.IsZero() {
			t.Fatal("Health 必须带上检查时间，否则调用方无法判断结果的新鲜度")
		}
	})

	t.Run("Start 后状态为 active", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := (*adapter).Start(ctx, spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() {
			if err := (*adapter).Stop(ctx, spec); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}()

		status, err := (*adapter).Status(ctx, spec)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status != domain.RuntimeActive {
			t.Fatalf("want active, got %s", status)
		}
	})

	t.Run("Stop 后状态为 inactive", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := (*adapter).Start(ctx, spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := (*adapter).Stop(ctx, spec); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		status, err := (*adapter).Status(ctx, spec)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status != domain.RuntimeInactive {
			t.Fatalf("want inactive, got %s", status)
		}
	})

	t.Run("Stop 幂等：未运行时也不报错", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := (*adapter).Stop(ctx, spec); err != nil {
			t.Fatalf("对未启动的应用 Stop 必须成功: %v", err)
		}
		if err := (*adapter).Stop(ctx, spec); err != nil {
			t.Fatalf("重复 Stop 必须成功: %v", err)
		}
	})

	t.Run("Start 幂等：已运行时重复 Start 不报错", func(t *testing.T) {
		adapter, spec := next(t)
		ctx := context.Background()

		if err := (*adapter).Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := (*adapter).Start(ctx, spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = (*adapter).Stop(ctx, spec) }()

		if err := (*adapter).Start(ctx, spec); err != nil {
			t.Fatalf("重复 Start 必须成功（幂等），got %v", err)
		}
		status, err := (*adapter).Status(ctx, spec)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status != domain.RuntimeActive {
			t.Fatalf("want active, got %s", status)
		}
	})

	t.Run("就绪目标可达时报告就绪", func(t *testing.T) {
		harness := factory(t)
		counter++
		spec := harness.NewSpec(t, specName(counter))
		ctx := context.Background()

		stopServing := harness.Serve(t, spec)
		defer stopServing()

		if err := harness.Adapter.Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := harness.Adapter.Start(ctx, spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = harness.Adapter.Stop(ctx, spec) }()

		health, err := harness.Adapter.Health(ctx, spec)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !health.Ready {
			t.Fatalf("就绪目标可达时必须报告就绪，detail=%q", health.Detail)
		}
	})

	t.Run("就绪目标不可达时不报告就绪", func(t *testing.T) {
		harness := factory(t)
		counter++
		spec := harness.NewSpec(t, specName(counter))
		ctx := context.Background()

		// 刻意不 Serve：进程活着但没有就绪目标。
		if err := harness.Adapter.Prepare(ctx, spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := harness.Adapter.Start(ctx, spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = harness.Adapter.Stop(ctx, spec) }()

		status, err := harness.Adapter.Status(ctx, spec)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status != domain.RuntimeActive {
			t.Fatalf("进程应当在运行，got %s", status)
		}

		health, err := harness.Adapter.Health(ctx, spec)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if health.Ready {
			t.Fatal("就绪目标不可达时不得报告就绪——「进程活着」与「已就绪」是两件事")
		}
	})
}

// specName 生成符合 application 字符集的应用名。
func specName(index int) string {
	return "contract-" + itoa(index)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
