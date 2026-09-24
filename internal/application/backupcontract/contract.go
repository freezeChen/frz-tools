// Package backupcontract 是所有 BackupAdapter 实现都必须通过的合约测试。
//
// 它的价值与 runtimecontract 同源：不在于「测出 bug」，而在于钉住多个实现之间的语义
// 一致。2b 会加 postgres 与 mysql 两个适配器，如果它们与 files 对「一次备份」的理解
// 不同，那么「本地能跑、真机不能跑」就会变成常态。
//
// 合约的核心是一条**往返**：内容 → 备份 → 破坏 → 恢复 → 内容一致。
// 只断言「Backup 没报错」是不够的——一个只写了空归档的适配器同样不会报错。
package backupcontract

import (
	"bytes"
	"context"
	"testing"

	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Harness 是合约测试需要宿主提供的东西。
//
// 适配器无法凭空造出「一份有内容的真实资源」，也无法在隔离恢复之后被外部检查
// ——隔离恢复的产物按定义是临时的、由适配器自己销毁的。因此「内容是否一致」只能靠
// 宿主给出的指纹在**原地往返**里验证。
type Harness struct {
	Adapter application.BackupAdapter

	// NewPolicy 返回一份合法且可备份的策略。name 已经过清洗，可直接用作策略名；
	// 每次调用必须返回互不冲突的名字，否则子测试之间会互相污染。
	NewPolicy func(t *testing.T, name string) *domain.BackupPolicy

	// Populate 在策略声明的资源上写入一份可辨认的内容。
	// 返回的字符串必须能反映内容的全部可观察特征（长度、摘要、结构）。
	Populate func(t *testing.T, policy *domain.BackupPolicy) string

	// Fingerprint 返回资源当前内容的指纹，用于恢复后的比对。
	Fingerprint func(t *testing.T, policy *domain.BackupPolicy) string

	// Wipe 清掉资源上的内容，用来证明「恢复真的写回来了」而不是「什么都没做」。
	Wipe func(t *testing.T, policy *domain.BackupPolicy)
}

func Run(t *testing.T, factory func(t *testing.T) Harness) {
	t.Helper()

	counter := 0
	next := func(t *testing.T) (Harness, *domain.BackupPolicy) {
		counter++
		harness := factory(t)
		return harness, harness.NewPolicy(t, specName(counter))
	}

	t.Run("Validate 对合法策略通过且不改动资源", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		before := harness.Populate(t, policy)
		if err := harness.Adapter.Validate(ctx, policy); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if got := harness.Fingerprint(t, policy); got != before {
			t.Fatalf("Validate 不得改动资源：before=%q after=%q", before, got)
		}
	})

	t.Run("Validate 拒绝非法策略", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		broken := *policy
		broken.Resource.Paths = nil // files 类资源没有范围必然非法
		if harness.Adapter.Kind() == domain.BackupResourceFiles {
			if err := harness.Adapter.Validate(ctx, &broken); err == nil {
				t.Fatal("非法规格必须被 Validate 拒绝")
			}
		}

		// 与适配器种类无关的非法：nil 策略。
		if err := harness.Adapter.Validate(ctx, nil); err == nil {
			t.Fatal("nil 策略必须被 Validate 拒绝")
		}
	})

	t.Run("Preflight 不产生备份产物", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		before := harness.Populate(t, policy)
		report, err := harness.Adapter.Preflight(ctx, policy)
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if got := harness.Fingerprint(t, policy); got != before {
			t.Fatalf("Preflight 不得改动资源：before=%q after=%q", before, got)
		}
		// 预检必须给出可读的检查项：只回「过了/没过」的话，失败时用户不知道该修什么。
		if len(report.Checks) == 0 {
			t.Fatal("Preflight 必须给出至少一项检查")
		}
	})

	t.Run("Backup 产出的流能通过 Verify", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		// 先造出有内容的资源：策略声明的路径不存在时，备份本来就该失败，
		// 拿一个空资源去跑 backup 只会测出「适配器会报错」这件无关的事。
		harness.Populate(t, policy)
		stream := backupToBytes(t, harness, policy)
		if len(stream) == 0 {
			t.Fatal("备份流不能为空")
		}
		if err := harness.Adapter.Verify(ctx, policy, bytes.NewReader(stream)); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("Verify 拒绝损坏的流", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		// 先造出有内容的资源：策略声明的路径不存在时，备份本来就该失败，
		// 拿一个空资源去跑 backup 只会测出「适配器会报错」这件无关的事。
		harness.Populate(t, policy)
		stream := backupToBytes(t, harness, policy)
		if len(stream) == 0 {
			t.Fatal("备份流不能为空")
		}
		// 把后半段截断：自洽性检查必须发现它，否则「校验通过」这句话没有意义。
		damaged := stream[:len(stream)/2]
		if err := harness.Adapter.Verify(ctx, policy, bytes.NewReader(damaged)); err == nil {
			t.Fatal("截断的流必须被 Verify 拒绝")
		}
	})

	t.Run("隔离恢复不改动资源", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		before := harness.Populate(t, policy)
		stream := backupToBytes(t, harness, policy)

		if err := harness.Adapter.Restore(ctx, policy, bytes.NewReader(stream), domain.RestoreIsolated); err != nil {
			t.Fatalf("隔离恢复: %v", err)
		}
		if got := harness.Fingerprint(t, policy); got != before {
			t.Fatalf("隔离恢复不得改动真实资源：before=%q after=%q", before, got)
		}
	})

	// 本合约的核心：内容 → 备份 → 破坏 → 原地恢复 → 内容一致。
	t.Run("往返：备份后清空再恢复，内容一致", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		before := harness.Populate(t, policy)
		stream := backupToBytes(t, harness, policy)

		harness.Wipe(t, policy)
		if got := harness.Fingerprint(t, policy); got == before {
			t.Fatal("Wipe 之后指纹应当变化，否则这个用例证明不了恢复真的写回了内容")
		}

		if err := harness.Adapter.Restore(ctx, policy, bytes.NewReader(stream), domain.RestoreInPlace); err != nil {
			t.Fatalf("原地恢复: %v", err)
		}
		if got := harness.Fingerprint(t, policy); got != before {
			t.Fatalf("恢复后内容不一致：want %q, got %q", before, got)
		}
	})

	t.Run("Cleanup 幂等", func(t *testing.T) {
		harness, policy := next(t)
		ctx := context.Background()

		if err := harness.Adapter.Cleanup(ctx, policy, "op_contract"); err != nil {
			t.Fatalf("首次 Cleanup: %v", err)
		}
		if err := harness.Adapter.Cleanup(ctx, policy, "op_contract"); err != nil {
			t.Fatalf("重复 Cleanup 必须成功（幂等）: %v", err)
		}
	})

	t.Run("Kind 与策略一致", func(t *testing.T) {
		harness, policy := next(t)
		if harness.Adapter.Kind() != policy.Resource.Kind {
			t.Fatalf("Kind want %q, got %q", policy.Resource.Kind, harness.Adapter.Kind())
		}
	})
}

// backupToBytes 跑一次 Backup 并把流收进内存。
func backupToBytes(t *testing.T, harness Harness, policy *domain.BackupPolicy) []byte {
	t.Helper()

	var buf bytes.Buffer
	metadata, err := harness.Adapter.Backup(context.Background(), policy, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if metadata.ResourceKind != "" && metadata.ResourceKind != policy.Resource.Kind {
		t.Fatalf("元数据里的 resourceKind want %q, got %q", policy.Resource.Kind, metadata.ResourceKind)
	}
	return buf.Bytes()
}

// specName 生成符合备份策略字符集的 name。
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
