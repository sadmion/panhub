// Package task 实现定时任务：目前是 STRM 同步，后续可以按同样套路
// 加「自动整理」「定时复制」等类型——只需在 Runner.run 的 switch 里加分支。
package task

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"panhub/internal/store"
	"panhub/internal/vfs"
)

// Runner 管理所有任务的定时执行。
type Runner struct {
	vfs     *vfs.VFS
	st      *store.Store
	title   string // 例如 panhub v0.1.0，写进日志
	baseURL string // STRM 里写绝对地址，Emby 才能拉到

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

// New 创建任务运行器。
func New(v *vfs.VFS, st *store.Store, title, baseURL string) *Runner {
	return &Runner{
		vfs:     v,
		st:      st,
		title:   title,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		cancels: map[string]context.CancelFunc{},
	}
}

// Start 为每个启用的任务拉起一个循环。
func (r *Runner) Start() {
	r.mu.Lock()
	r.stopped = false
	r.mu.Unlock()
	r.Reload()
}

// Stop 停止所有循环。
func (r *Runner) Stop() {
	r.mu.Lock()
	r.stopped = true
	cancels := r.cancels
	r.cancels = map[string]context.CancelFunc{}
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	r.wg.Wait()
}

// Reload 按当前任务列表重建所有循环（增删任务后调用）。
func (r *Runner) Reload() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	old := r.cancels
	r.cancels = map[string]context.CancelFunc{}
	r.mu.Unlock()
	for _, c := range old {
		c()
	}

	for _, t := range r.st.Tasks() {
		if !t.Enabled || t.Type != "strm" {
			continue
		}
		d, err := ParseInterval(t.Interval)
		if err != nil {
			slog.Warn("任务间隔无效，跳过调度", "task", t.Name, "interval", t.Interval, "err", err)
			continue
		}
		if d <= 0 {
			continue // manual（仅手动触发）
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.mu.Lock()
		r.cancels[t.ID] = cancel
		r.mu.Unlock()

		r.wg.Add(1)
		go func(t store.Task, d time.Duration) {
			defer r.wg.Done()
			// 启动后先跑一次
			r.execute(ctx, t)
			tk := time.NewTicker(d)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
					r.execute(ctx, t)
				}
			}
		}(t, d)
	}
}

// RunNow 立即执行一次（不受间隔限制）。
func (r *Runner) RunNow(id string) error {
	t, ok := r.st.GetTask(id)
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	if t.Type != "strm" {
		return fmt.Errorf("暂不支持的任务类型: %s", t.Type)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	count, info, err := r.runSTRM(ctx, t)
	if err != nil {
		r.st.RecordTaskRun(t.ID, "failed", err.Error(), count, info)
		return err
	}
	r.st.RecordTaskRun(t.ID, "success", "", count, info)
	return nil
}

func (r *Runner) execute(ctx context.Context, t store.Task) {
	count, info, err := r.runSTRM(ctx, t)
	if err != nil {
		slog.Error("任务失败", "task", t.Name, "err", err, "target", info.Target)
		r.st.RecordTaskRun(t.ID, "failed", err.Error(), count, info)
		return
	}
	slog.Info("任务完成", "task", t.Name, "strm_files", count, "target", info.Target, "sample", info.Sample)
	r.st.RecordTaskRun(t.ID, "success", "", count, info)
}

// ParseInterval 支持 30m / 6h / 1d，空或 manual 表示只手动执行。
func ParseInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "manual" || s == "off" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("无法解析间隔 %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("无法解析间隔 %q（示例：30m、6h、1d）", s)
	}
	if d < time.Minute {
		return 0, fmt.Errorf("间隔太小: %s", s)
	}
	return d, nil
}
