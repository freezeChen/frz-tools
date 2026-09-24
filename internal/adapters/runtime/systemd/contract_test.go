package systemd_test

import (
	"testing"

	"github.com/freezeChen/frz-tools/internal/adapters/runtime/systemd"
	"github.com/freezeChen/frz-tools/internal/application/runtimecontract"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// TestSystemdAdapterContract 在假 systemctl + 根前缀下跑同一套合约测试。
//
// 它验证的是适配器与合约的语义一致性（状态映射、幂等、就绪与状态分离），
// 而不是 systemd 本身的行为：真实 systemd 上的合约验证属 A7（Linux 容器）。
// 假主机的 unit 状态与 unit 文件是否存在挂钩，因此「未 Prepare 不能 Start」
// 这类断言走的是与真机同一条判据。
func TestSystemdAdapterContract(t *testing.T) {
	runtimecontract.Run(t, func(t *testing.T) runtimecontract.Harness {
		root := t.TempDir()
		host := newFakeHost(root)
		adapter := systemd.New(root, &stubResolver{values: secretValues()},
			systemd.WithRunner(host.run),
			systemd.WithGOOS("linux"),
			systemd.WithOwnerResolver(currentOwner),
		)

		return runtimecontract.Harness{
			Adapter: adapter,
			NewSpec: func(t *testing.T, name string) *domain.ApplicationSpec {
				return specFixture(t, name)
			},
			Serve: serveTCP,
		}
	})
}
