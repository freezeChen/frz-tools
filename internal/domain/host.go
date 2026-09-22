package domain

import (
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// Host 在 1c 只是身份与标签，不承载连接语义：address 为空表示本机。
// 远程主机、SSH 与 mTLS 属于迭代 5，届时才把这些字段变成真正的连接参数。
type Host struct {
	ID        string
	Name      string
	Address   string
	Labels    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (h *Host) Validate() error {
	if strings.TrimSpace(h.Name) == "" {
		return NewError(v1.CodeInvalidRequest, "主机名称不能为空")
	}
	return nil
}

// Environment 同理只是身份与标签，1c 不与应用建立关联。
type Environment struct {
	ID        string
	Name      string
	Labels    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (e *Environment) Validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return NewError(v1.CodeInvalidRequest, "环境名称不能为空")
	}
	return nil
}
