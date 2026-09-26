package main

import (
	"context"

	"github.com/freezeChen/frz-tools/internal/adapters/runtime/systemd"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// systemdReporter 把 systemd 适配器私有的 Prepare 决策（unit 档位、探测到的版本、
// 降级说明）翻译成 application 层的 RuntimeDecision。
//
// 这层翻译只能放在装配层：适配器的 UnitDecision 类型不能进 application（适配器已经
// import application，反向引用会成环），而 RuntimeAdapter 端口也不该为了诊断信息长出
// 一个只有 systemd 能实现的字段。装配层本来就持有具体适配器，由它来做这次转换最自然。
type systemdReporter struct {
	adapter *systemd.Adapter
}

func (r systemdReporter) ReportRuntimePrepare(_ context.Context, spec *domain.ApplicationSpec, slot domain.Slot) (application.RuntimeDecision, bool) {
	// 按槽位取 unit 名：蓝绿的两个槽位各有一个 unit，各自的档位决策分开记
	// （空槽位就是单槽那条路）。
	decision, ok := r.adapter.UnitDecision(spec.UnitNameFor(slot))
	if !ok {
		// 适配器没有这次 Prepare 的记录（例如 Prepare 在别的进程里做过）：
		// 返回 ok=false，让调用方省略字段，而不是编一个看起来像真的空档位。
		return application.RuntimeDecision{}, false
	}
	return application.RuntimeDecision{
		UnitName:       decision.UnitName,
		UnitPath:       decision.UnitPath,
		Tier:           string(decision.Tier),
		SystemdVersion: decision.SystemdVersion,
		Degradations:   decision.Degradations,
		DecidedAt:      decision.DecidedAt,
	}, true
}
