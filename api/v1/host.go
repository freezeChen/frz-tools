package v1

import "time"

type CreateHostRequest struct {
	Name    string            `json:"name"`
	Address string            `json:"address,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// Host 在 1c 只是身份与标签。address 刻意不加 omitempty：它为空表示「本机」，
// 是语义而不是缺失值，省掉字段会让本机记录看起来没有地址信息。
type Host struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Address   string            `json:"address"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type HostResponse struct {
	APIVersion string `json:"apiVersion"`
	Host       Host   `json:"host"`
}

type HostListResponse struct {
	APIVersion string `json:"apiVersion"`
	Items      []Host `json:"items"`
}

type CreateEnvironmentRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Environment 同样只有身份与标签，1c 不与应用建立关联（迭代 3/5 再做）。
type Environment struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type EnvironmentResponse struct {
	APIVersion  string      `json:"apiVersion"`
	Environment Environment `json:"environment"`
}

type EnvironmentListResponse struct {
	APIVersion string        `json:"apiVersion"`
	Items      []Environment `json:"items"`
}
