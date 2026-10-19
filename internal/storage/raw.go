package storage

import "strings"

func trimSpace(s string) string { return strings.TrimSpace(s) }

// NewRaw 按类型构造驱动但不调用 Init，用于「登录」这类不依赖已有配置的操作。
func NewRaw(typ string) (Driver, bool) {
	regMu.RLock()
	f, ok := registry[typ]
	regMu.RUnlock()
	if !ok {
		return nil, false
	}
	return f(), true
}
