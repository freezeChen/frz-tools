package v1

// SetTargetsRequest 替换某个应用的部署目标列表（迭代 5b）。
//
// 刻意是**替换**而不是增量：这张表表达的是「现在应该在哪几台」。增量语义会让
// 「撤掉一台」变成一个必须显式表达的动作，而运维真正想说的就是「现在是这样」。
type SetTargetsRequest struct {
	Hosts     []string `json:"hosts"`
	UpdatedBy string   `json:"updatedBy,omitempty"`
}

// TargetHost 是一台部署目标主机。Address 从**本机的主机表**解析出来，空表示本机
// （与 5a 的 host 记录同一条规则）。
type TargetHost struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// ApplicationTargets 是一个应用的部署目标。它是**声明的意图**，不是探测结果：
// 一台机不在这个列表里，意思是「它本来就不该跑这个应用」。
type ApplicationTargets struct {
	Application string       `json:"application"`
	Hosts       []TargetHost `json:"hosts"`
}

type TargetsResponse struct {
	APIVersion string             `json:"apiVersion"`
	Targets    ApplicationTargets `json:"targets"`
}

// TargetsListResponse 一次给出所有应用的部署目标——跨主机的应用清单汇总。
type TargetsListResponse struct {
	APIVersion string               `json:"apiVersion"`
	Items      []ApplicationTargets `json:"items"`
}
