// Package storage 的登录扩展点：让驱动自己接管「账号密码换凭证」这件事。
package storage

import (
	"context"
	"errors"
)

// ErrLoginRequired 表示凭证缺失或已失效，需要用户重新登录。
var ErrLoginRequired = errors.New("需要重新登录")

// LoginRequest 是一次登录请求。Mode 由驱动约定，常见取值：
//   - password      账号 + 密码
//   - authorization 直接粘贴已有的 Authorization / Cookie
//   - cookies       用浏览器里的 Cookie 快速换取
//
// Fields 里的键由驱动的 Meta.LoginFields 声明，前端据此渲染输入框。
type LoginRequest struct {
	Mode   string            `json:"mode"`
	Fields map[string]string `json:"fields"`
}

// LoginResult 返回给前端：Session 会被合并进挂载配置并落盘。
type LoginResult struct {
	Session map[string]any `json:"session"`
	Profile map[string]any `json:"profile"`
	Message string         `json:"message,omitempty"`
}

// Get 读取一个字段（去空格）。
func (r LoginRequest) Get(key string) string {
	if r.Fields == nil {
		return ""
	}
	return trimSpace(r.Fields[key])
}

// Authenticator 由支持登录的驱动实现。
type Authenticator interface {
	Login(ctx context.Context, req LoginRequest) (*LoginResult, error)
}
