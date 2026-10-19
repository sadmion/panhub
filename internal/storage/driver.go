// Package storage 定义所有网盘后端必须实现的驱动契约。
//
// 新增一个网盘 = 新建一个子包，实现 Driver 接口，在 init() 里
// storage.Register("名字", ...) 注册，然后在 cmd/panhub/main.go 里空导入。
// 详见 docs/ADD_A_DRIVER.md。
package storage

import (
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 驱动应返回这些哨兵错误，HTTP 层据此映射状态码。
var (
	ErrNotFound       = errors.New("not found")
	ErrNotSupported   = errors.New("not supported by this driver")
	ErrNotConfigured  = errors.New("driver not configured")
	ErrNotImplemented = errors.New("not implemented yet")
)

// Entry 是一个文件或目录。Path 始终是驱动内的绝对路径，以 "/" 开头。
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	ID      string    `json:"id,omitempty"`
}

// Caps 声明驱动支持哪些可选操作，前端据此禁用不可用的按钮。
type Caps struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Move   bool `json:"move"`
	Copy   bool `json:"copy"`
	Link   bool `json:"link"`
	Upload bool `json:"upload"`
}

// Driver 是每个后端都必须实现的最小接口。
type Driver interface {
	Type() string
	Caps() Caps
	Init(ctx context.Context, cfg map[string]any) error
	List(ctx context.Context, dir string) ([]Entry, error)
	Stat(ctx context.Context, p string) (Entry, error)
}

// Writer 是可写驱动额外实现的能力。
type Writer interface {
	Mkdir(ctx context.Context, p string) error
	Remove(ctx context.Context, p string, recursive bool) error
	Upload(ctx context.Context, p string, r io.Reader, size int64) error
}

// Mover 支持服务端重命名/移动。
type Mover interface {
	Move(ctx context.Context, from, to string) error
}

// Copier 支持服务端复制。
type Copier interface {
	Copy(ctx context.Context, from, to string) error
}

// Opener 支持以流的方式读取文件（代理播放/下载）。
type Opener interface {
	Open(ctx context.Context, p string) (io.ReadCloser, error)
}

// RangeOpener 让播放器可以跳转进度，而不必从第 0 字节开始下载。
type RangeOpener interface {
	OpenRange(ctx context.Context, p string, offset int64) (io.ReadCloser, error)
}

// Linker 在网盘能给出直链时返回它，用于 302 直连播放。
type Linker interface {
	Link(ctx context.Context, p string) (string, error)
}

// Factory 构造一个尚未 Init 的驱动实例。
type Factory func() Driver

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register 由驱动包的 init() 调用。
func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[typ]; dup {
		panic("storage: duplicate driver type " + typ)
	}
	registry[typ] = f
}

// Types 返回全部已注册的驱动类型名。
func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// New 按类型构造并初始化驱动。
func New(ctx context.Context, typ string, cfg map[string]any) (Driver, error) {
	regMu.RLock()
	f, ok := registry[typ]
	regMu.RUnlock()
	if !ok {
		return nil, ErrNotSupported
	}
	d := f()
	if err := d.Init(ctx, cfg); err != nil {
		return nil, err
	}
	return d, nil
}

// ---------- 路径工具 ----------

// Clean 归一化为以 "/" 开头的绝对路径，根目录为 "/"。
func Clean(p string) string {
	if p == "" {
		return "/"
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	c := path.Clean(p)
	if c == "." {
		return "/"
	}
	return c
}

// Join 拼接虚拟路径。
func Join(base, name string) string {
	return Clean(path.Join(Clean(base), name))
}

// Parent 返回父目录，根目录的父目录仍是根目录。
func Parent(p string) string {
	c := Clean(p)
	if c == "/" {
		return "/"
	}
	return Clean(path.Dir(c))
}

// Base 返回最后一段名字。
func Base(p string) string {
	c := Clean(p)
	if c == "/" {
		return ""
	}
	return path.Base(c)
}

// SortEntries 目录在前、然后按名字不区分大小写排序。
func SortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

// ---------- 配置读取工具 ----------

// String 读取字符串配置项。
func String(cfg map[string]any, key, def string) string {
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return def
		}
		return t
	default:
		return strings.TrimSpace(strconvQuote(t))
	}
}

func strconvQuote(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// Bool 读取布尔配置项，接受 true/"true"/"1"/1 等写法。
func Bool(cfg map[string]any, key string, def bool) bool {
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return def
		}
		return b
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return def
	}
}

// Int 读取整型配置项。
func Int(cfg map[string]any, key string, def int) int {
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return def
		}
		return n
	default:
		return def
	}
}

// MaskConfig 输出给前端前把敏感字段打码。
func MaskConfig(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "password") || strings.Contains(lk, "token") ||
			strings.Contains(lk, "cookie") || strings.Contains(lk, "secret") || strings.Contains(lk, "key") {
			if s, ok := v.(string); ok && s != "" {
				out[k] = "******"
				continue
			}
		}
		out[k] = v
	}
	return out
}
