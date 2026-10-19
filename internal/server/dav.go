package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/net/webdav"

	"panhub/internal/storage"
	"panhub/internal/vfs"
)

// davFS 把 VFS 适配成 WebDAV 文件系统：这样挂载点自动获得
// PROPFIND / GET / PUT / MKCOL / MOVE / DELETE 全套能力。
type davFS struct {
	v *vfs.VFS
}

type davInfo struct{ e storage.Entry }

func (i davInfo) Name() string       { return i.e.Name }
func (i davInfo) Size() int64        { return i.e.Size }
func (i davInfo) ModTime() time.Time { return i.e.ModTime }
func (i davInfo) IsDir() bool        { return i.e.IsDir }
func (i davInfo) Sys() any           { return nil }
func (i davInfo) Mode() os.FileMode {
	if i.e.IsDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}

func (d *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	e, err := d.v.Stat(ctx, name)
	if err != nil {
		// webdav 只认 os.IsNotExist；不转换会把 404 变成 405。
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return davInfo{e: e}, nil
}

func (d *davFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	return d.v.Mkdir(ctx, name)
}

func (d *davFS) RemoveAll(ctx context.Context, name string) error {
	return d.v.Remove(ctx, name, true)
}

func (d *davFS) Rename(ctx context.Context, oldName, newName string) error {
	return d.v.Move(ctx, oldName, newName)
}

func (d *davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	e, err := d.v.Stat(ctx, name)
	if err != nil {
		if !(flag&os.O_CREATE != 0 && errors.Is(err, storage.ErrNotFound)) {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, fs.ErrNotExist
			}
			return nil, err
		}
		// 新建文件：先占位成"零字节文件"，写入完成时再真正上传。
		e = storage.Entry{Name: storage.Base(name), Path: name}
	}
	f := &davFile{ctx: ctx, v: d.v, name: name, entry: e, flag: flag}
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		f.buf = &bytes.Buffer{}
	}
	return f, nil
}

// davFile 同时支持读（流式 + Seek）和写（缓冲到内存后上传）。
type davFile struct {
	ctx   context.Context
	v     *vfs.VFS
	name  string
	entry storage.Entry
	flag  int

	rc  io.ReadCloser
	off int64
	buf *bytes.Buffer
}

func (f *davFile) Stat() (os.FileInfo, error) {
	if f.entry.IsDir || f.entry.Name == "" {
		e, err := f.v.Stat(f.ctx, f.name)
		if err != nil {
			return nil, err
		}
		f.entry = e
	}
	return davInfo{e: f.entry}, nil
}

func (f *davFile) Readdir(count int) ([]fs.FileInfo, error) {
	entries, err := f.v.List(f.ctx, f.name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, davInfo{e: e})
	}
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out, nil
}

func (f *davFile) Read(p []byte) (int, error) {
	if f.rc == nil {
		rc, err := f.v.Open(f.ctx, f.name, f.off)
		if err != nil {
			return 0, err
		}
		f.rc = rc
	}
	n, err := f.rc.Read(p)
	f.off += int64(n)
	return n, err
}

// Seek 通过重新打开并跳过字节实现，支持播放器的拖拽跳转。
func (f *davFile) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = f.off + offset
	case io.SeekEnd:
		size := f.entry.Size
		if size == 0 {
			if e, err := f.v.Stat(f.ctx, f.name); err == nil {
				size = e.Size
			}
		}
		target = size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if target < 0 {
		return 0, errors.New("negative seek")
	}
	if f.rc != nil {
		_ = f.rc.Close()
		f.rc = nil
	}
	f.off = target
	return target, nil
}

func (f *davFile) Write(p []byte) (int, error) {
	if f.buf == nil {
		return 0, errors.New("file not open for writing")
	}
	return f.buf.Write(p)
}

func (f *davFile) Close() error {
	if f.rc != nil {
		err := f.rc.Close()
		f.rc = nil
		return err
	}
	if f.buf != nil {
		data := f.buf.Bytes()
		if err := f.v.Upload(f.ctx, f.name, bytes.NewReader(data), int64(len(data))); err != nil {
			return err
		}
		f.buf = nil
	}
	return nil
}
