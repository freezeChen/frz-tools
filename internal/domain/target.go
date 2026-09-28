package domain

import (
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// ApplicationTargets 是一个应用的**部署目标**：它应该跑在哪些主机上（迭代 5b）。
//
// 它表达的是意图而不是观察：一台机没在这个列表里，意思是「它本来就不该跑这个应用」，
// 而不是「它现在没跑」。批量发布只认这个列表——靠探测来推导目标，会把「挂了的机器」
// 悄悄从发布范围里去掉，而那正是最不能接受的一种失败。
type ApplicationTargets struct {
	// Application 是应用名。它是跨主机稳定、唯一的标识（本机的 applications 主键不是）。
	Application string
	// Hosts 是主机名列表，保持登记时的顺序——批次的波次顺序就是它。
	Hosts []string
}

// NormalizeTargets 清洗一份主机名列表：去掉空白项与重复项，保持首次出现的顺序。
//
// 去重是静默的（不报错）：运维从 shell 里拼出来的列表重复一次是很正常的事，为它
// 报错只会让人学会加一条「忽略重复」的旗标。但**顺序保留**是刻意的：波次顺序由
// 列表顺序决定，重排它等于偷偷改掉运维写的滚动顺序。
func NormalizeTargets(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		name := strings.TrimSpace(host)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// ValidateApplicationName 校验部署目标里的应用名。它必须是**名字**，不是本机 ID：
// 同一个 ID 在另一台机上指向别的东西，而这个名字在每台机上指同一个应用。
func ValidateApplicationName(name string) error {
	if strings.TrimSpace(name) == "" {
		return NewError(v1.CodeInvalidRequest, "应用名不能为空")
	}
	return nil
}
