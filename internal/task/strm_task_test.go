package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"panhub/internal/storage"
	"panhub/internal/store"
	"panhub/internal/vfs"
)

// fakeDriver 是一个只读的假驱动，用来在内存里模拟「网盘目录」。
type fakeDriver struct {
	files []storage.Entry
}

func (d *fakeDriver) Type() string             { return "fake" }
func (d *fakeDriver) Caps() storage.Caps       { return storage.Caps{Read: true} }
func (d *fakeDriver) Init(context.Context, map[string]any) error { return nil }

func (d *fakeDriver) List(_ context.Context, dir string) ([]storage.Entry, error) {
	out := []storage.Entry{}
	for _, f := range d.files {
		if storage.Parent(f.Path) == dir {
			out = append(out, f)
		}
	}
	return out, nil
}

func (d *fakeDriver) Stat(_ context.Context, p string) (storage.Entry, error) {
	for _, f := range d.files {
		if f.Path == p {
			return f, nil
		}
	}
	return storage.Entry{}, storage.ErrNotFound
}

// TestRunSTRMWritesLocalFiles 验证：普通媒体和 .cas 都会在本地落成 .strm，
// 目录结构保持一致，.cas 生成的 strm 名会去掉 .cas。
func TestRunSTRMWritesLocalFiles(t *testing.T) {
	drv := &fakeDriver{files: []storage.Entry{
		{Name: "电影", Path: "/电影", IsDir: true},
		{Name: "剧集", Path: "/剧集", IsDir: true},
		{Name: "a.mkv", Path: "/电影/a.mkv", Size: 100},
		{Name: "b.mkv.cas", Path: "/电影/b.mkv.cas", Size: 300},
		{Name: "c.mp4", Path: "/剧集/c.mp4", Size: 50},
		{Name: "readme.txt", Path: "/剧集/readme.txt", Size: 1},
	}}

	v := vfs.New()
	v.Add(&vfs.Mount{Name: "139", Type: "fake", Driver: drv})

	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	r := New(v, st, "test", "http://127.0.0.1:5211")

	n, info, err := r.runSTRM(context.Background(), store.Task{
		ID: "t1", Name: "test", Type: "strm", Mount: "139", Dir: "/", Target: target, Mode: "proxy",
	})
	if err != nil {
		t.Fatalf("任务失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("应生成 3 个 strm，实际 %d", n)
	}
	// 任务必须回报「真实写到哪」，容器部署时靠它定位
	if info.Target == "" || !filepath.IsAbs(info.Target) {
		t.Fatalf("RunInfo.Target 应为绝对路径，实际 %q", info.Target)
	}
	if info.Sample == "" {
		t.Fatal("RunInfo.Sample 应给出一个已写入的文件路径")
	}
	if _, err := os.Stat(info.Sample); err != nil {
		t.Fatalf("RunInfo.Sample 指向的文件不存在: %s (%v)", info.Sample, err)
	}

	for _, want := range []string{
		"电影/a.strm",
		"电影/b.strm", // .cas 去掉后缀
		"剧集/c.strm",
	} {
		p := filepath.Join(target, filepath.FromSlash(want))
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("缺少 %s: %v", want, err)
		}
		if !strings.Contains(string(data), "/api/stream?path=") {
			t.Fatalf("%s 内容不对: %s", want, data)
		}
	}
	// .cas 的源头路径必须带 .cas（播放时靠它触发还原）
	casStrm, _ := os.ReadFile(filepath.Join(target, "电影", "b.strm"))
	if !strings.Contains(string(casStrm), "b.mkv.cas") {
		t.Fatalf("CAS 的 strm 应指向 .cas 源文件，实际: %s", casStrm)
	}
	// 非媒体不该生成
	if _, err := os.Stat(filepath.Join(target, "剧集", "readme.strm")); err == nil {
		t.Fatal("非媒体文件不应生成 strm")
	}
}

// TestRunSTRMRejectsEmptyTarget 输出目录为空时要明确报错。
func TestRunSTRMRejectsEmptyTarget(t *testing.T) {
	v := vfs.New()
	v.Add(&vfs.Mount{Name: "m", Type: "fake", Driver: &fakeDriver{}})
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	r := New(v, st, "test", "")
	_, _, err := r.runSTRM(context.Background(), store.Task{Mount: "m", Dir: "/", Target: ""})
	if err == nil {
		t.Fatal("没有 target 时应当报错")
	}
}
