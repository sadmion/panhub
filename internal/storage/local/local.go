// Package local 把宿主机上的一个目录挂载成网盘。
package local

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"panhub/internal/storage"
)

func init() {
	storage.Register("local", func() storage.Driver { return &Driver{} })
	storage.RegisterMeta("local", "本地目录", []storage.Field{
		{Key: "root", Label: "目录路径", Type: "text", Required: true, Placeholder: "./data/local", Help: "宿主机上的目录，不存在会自动创建"},
	})
}

// Driver 实现本地目录驱动。
type Driver struct {
	root string
}

func (d *Driver) Type() string { return "local" }

func (d *Driver) Caps() storage.Caps {
	return storage.Caps{Read: true, Write: true, Move: true, Copy: true, Upload: true}
}

func (d *Driver) Init(_ context.Context, cfg map[string]any) error {
	root := storage.String(cfg, "root", "")
	if root == "" {
		return errors.New("local: 配置项 root 不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	d.root = abs
	return nil
}

// resolve 把虚拟路径映射到磁盘路径，并阻止 ../ 越权。
func (d *Driver) resolve(p string) (string, error) {
	rel := strings.TrimPrefix(storage.Clean(p), "/")
	full := filepath.Join(d.root, filepath.FromSlash(rel))
	back, err := filepath.Rel(d.root, full)
	if err != nil {
		return "", storage.ErrNotFound
	}
	if back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", storage.ErrNotFound
	}
	return full, nil
}

func (d *Driver) List(_ context.Context, dir string) ([]storage.Entry, error) {
	full, err := d.resolve(dir)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	out := make([]storage.Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, storage.Entry{
			Name:    info.Name(),
			Path:    storage.Join(dir, info.Name()),
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	storage.SortEntries(out)
	return out, nil
}

func (d *Driver) Stat(_ context.Context, p string) (storage.Entry, error) {
	full, err := d.resolve(p)
	if err != nil {
		return storage.Entry{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return storage.Entry{}, storage.ErrNotFound
		}
		return storage.Entry{}, err
	}
	return storage.Entry{
		Name:    info.Name(),
		Path:    storage.Clean(p),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

func (d *Driver) Mkdir(_ context.Context, p string) error {
	full, err := d.resolve(p)
	if err != nil {
		return err
	}
	return os.MkdirAll(full, 0o755)
}

func (d *Driver) Remove(_ context.Context, p string, recursive bool) error {
	full, err := d.resolve(p)
	if err != nil {
		return err
	}
	if full == d.root {
		return errors.New("local: 不允许删除挂载根目录")
	}
	if recursive {
		return os.RemoveAll(full)
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return storage.ErrNotFound
		}
		return err
	}
	return nil
}

func (d *Driver) Upload(_ context.Context, p string, r io.Reader, _ int64) error {
	full, err := d.resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// 先写临时文件再 rename，避免半截文件被播放器读到。
	tmp, err := os.CreateTemp(filepath.Dir(full), ".panhub-upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, full)
}

func (d *Driver) Move(_ context.Context, from, to string) error {
	src, err := d.resolve(from)
	if err != nil {
		return err
	}
	dst, err := d.resolve(to)
	if err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (d *Driver) Copy(_ context.Context, from, to string) error {
	src, err := d.resolve(from)
	if err != nil {
		return err
	}
	dst, err := d.resolve(to)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return storage.ErrNotSupported
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (d *Driver) Open(_ context.Context, p string) (io.ReadCloser, error) {
	full, err := d.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

func (d *Driver) OpenRange(_ context.Context, p string, offset int64) (io.ReadCloser, error) {
	full, err := d.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

var _ = time.Now
