// Package vfs 把多个网盘驱动聚合成一棵统一的虚拟目录树：
//
//	/                → 列出所有挂载点
//	/local/...       → 交给 local 驱动
//	/mydav/...       → 交给 webdav 驱动
package vfs

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"

	"panhub/internal/storage"
)

// Mount 是一个挂载点。
type Mount struct {
	Name     string
	Type     string
	ReadOnly bool
	Driver   storage.Driver
	Config   map[string]any
}

// VFS 是挂载点的并发安全集合。
type VFS struct {
	mu sync.RWMutex
	m  map[string]*Mount
}

// New 创建空 VFS。
func New() *VFS { return &VFS{m: map[string]*Mount{}} }

// Add 添加或替换挂载点。
func (v *VFS) Add(mt *Mount) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[mt.Name] = mt
}

// RemoveMount 移除挂载点。
func (v *VFS) RemoveMount(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, name)
}

// Get 按名取挂载点。
func (v *VFS) Get(name string) (*Mount, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	mt, ok := v.m[name]
	return mt, ok
}

// Mounts 返回按名字排序的挂载点快照。
func (v *VFS) Mounts() []*Mount {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]*Mount, 0, len(v.m))
	for _, mt := range v.m {
		out = append(out, mt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve 把 "/mount/path" 拆成挂载点 + 驱动内路径。
func (v *VFS) Resolve(p string) (*Mount, string, error) {
	clean := storage.Clean(p)
	if clean == "/" {
		return nil, "/", nil
	}
	trimmed := strings.TrimPrefix(clean, "/")
	name, rest, _ := strings.Cut(trimmed, "/")
	mt, ok := v.Get(name)
	if !ok {
		return nil, "", storage.ErrNotFound
	}
	if rest == "" {
		return mt, "/", nil
	}
	return mt, storage.Clean("/" + rest), nil
}

// List 列出目录内容；根目录返回所有挂载点。
func (v *VFS) List(ctx context.Context, p string) ([]storage.Entry, error) {
	clean := storage.Clean(p)
	if clean == "/" {
		ms := v.Mounts()
		out := make([]storage.Entry, 0, len(ms))
		for _, mt := range ms {
			out = append(out, storage.Entry{Name: mt.Name, Path: "/" + mt.Name, IsDir: true})
		}
		return out, nil
	}
	mt, sub, err := v.Resolve(clean)
	if err != nil {
		return nil, err
	}
	entries, err := mt.Driver.List(ctx, sub)
	if err != nil {
		return nil, err
	}
	// 驱动返回的是挂载内相对路径，这里补上挂载名，对外统一成全局路径。
	prefix := "/" + mt.Name
	for i := range entries {
		entries[i].Path = prefix + entries[i].Path
	}
	return entries, nil
}

// Stat 查询单个路径。
func (v *VFS) Stat(ctx context.Context, p string) (storage.Entry, error) {
	clean := storage.Clean(p)
	if clean == "/" {
		return storage.Entry{Name: "/", Path: "/", IsDir: true}, nil
	}
	mt, sub, err := v.Resolve(clean)
	if err != nil {
		return storage.Entry{}, err
	}
	if sub == "/" {
		return storage.Entry{Name: mt.Name, Path: clean, IsDir: true}, nil
	}
	e, err := mt.Driver.Stat(ctx, sub)
	if err != nil {
		return storage.Entry{}, err
	}
	e.Path = clean
	return e, nil
}

// Open 以流方式打开文件（代理播放/下载）。
func (v *VFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	mt, sub, err := v.Resolve(p)
	if err != nil {
		return nil, err
	}
	if ro, ok := mt.Driver.(storage.RangeOpener); ok && offset > 0 {
		return ro.OpenRange(ctx, sub, offset)
	}
	op, ok := mt.Driver.(storage.Opener)
	if !ok {
		return nil, storage.ErrNotSupported
	}
	rc, err := op.Open(ctx, sub)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
			rc.Close()
			return nil, err
		}
	}
	return rc, nil
}

// Link 尝试取直链。
func (v *VFS) Link(ctx context.Context, p string) (string, error) {
	mt, sub, err := v.Resolve(p)
	if err != nil {
		return "", err
	}
	lk, ok := mt.Driver.(storage.Linker)
	if !ok {
		return "", storage.ErrNotSupported
	}
	return lk.Link(ctx, sub)
}

func (v *VFS) writable(p string) (*Mount, string, error) {
	mt, sub, err := v.Resolve(p)
	if err != nil {
		return nil, "", err
	}
	if mt.ReadOnly {
		return nil, "", ErrReadOnly
	}
	return mt, sub, nil
}

// ErrReadOnly 由只读挂载返回。
var ErrReadOnly = errors.New("mount is read-only")

// Mkdir 新建目录。
func (v *VFS) Mkdir(ctx context.Context, p string) error {
	mt, sub, err := v.writable(p)
	if err != nil {
		return err
	}
	w, ok := mt.Driver.(storage.Writer)
	if !ok {
		return storage.ErrNotSupported
	}
	return w.Mkdir(ctx, sub)
}

// Remove 删除文件或目录。
func (v *VFS) Remove(ctx context.Context, p string, recursive bool) error {
	mt, sub, err := v.writable(p)
	if err != nil {
		return err
	}
	if sub == "/" {
		return storage.ErrNotSupported // 不允许删挂载根
	}
	w, ok := mt.Driver.(storage.Writer)
	if !ok {
		return storage.ErrNotSupported
	}
	return w.Remove(ctx, sub, recursive)
}

// Upload 上传文件到目录 p/name。
func (v *VFS) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	mt, sub, err := v.writable(p)
	if err != nil {
		return err
	}
	w, ok := mt.Driver.(storage.Writer)
	if !ok {
		return storage.ErrNotSupported
	}
	return w.Upload(ctx, sub, r, size)
}

// Move 在同一挂载点内移动/重命名。
func (v *VFS) Move(ctx context.Context, from, to string) error {
	fm, fsub, err := v.writable(from)
	if err != nil {
		return err
	}
	tm, tsub, err := v.Resolve(to)
	if err != nil {
		return err
	}
	if fm.Name != tm.Name {
		return storage.ErrNotSupported // 跨挂载点后面再实现
	}
	mv, ok := fm.Driver.(storage.Mover)
	if !ok {
		return storage.ErrNotSupported
	}
	return mv.Move(ctx, fsub, tsub)
}

// Copy 复制文件（同挂载点）。
func (v *VFS) Copy(ctx context.Context, from, to string) error {
	fm, fsub, err := v.Resolve(from)
	if err != nil {
		return err
	}
	tm, tsub, err := v.writable(to)
	if err != nil {
		return err
	}
	if fm.Name != tm.Name {
		return storage.ErrNotSupported
	}
	cp, ok := fm.Driver.(storage.Copier)
	if !ok {
		return storage.ErrNotSupported
	}
	return cp.Copy(ctx, fsub, tsub)
}
