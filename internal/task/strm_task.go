package task

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"panhub/internal/storage"
	"panhub/internal/store"
	"panhub/internal/strm"
)

// STRM 生成：把挂载树里的媒体（含 .cas）在本地目标目录铺成同名 .strm。
//
// 这里刻意做成「先规划、再执行、最后汇报」三步，而不是边走边写：
//   - 目录不存在 / 无权限这类问题在规划阶段就能报出来，不会写一半
//   - 每个文件的结果都记录下来，UI 能显示「跳过 N 个」和原因
//   - 失败不会中断整个任务（单个文件失败只记一条）

// Plan 是一次 STRM 任务的执行计划。
type Plan struct {
	Items   []PlanItem
	Scanned int
	Skipped int
}

// PlanItem 是需要生成的一个 .strm。
type PlanItem struct {
	Source string // 挂载内全局路径，如 /139/电影/a.mkv.cas
	Name   string // 文件名
	Dst    string // 本地绝对路径
}

// SkippedItem 是被跳过的文件及原因。
type SkippedItem struct {
	Path   string
	Reason string
}

// RunResult 是任务执行结果。
type RunResult struct {
	Written int
	Skipped []SkippedItem
	Scanned int
}

func (r *RunResult) addSkip(p, reason string) {
	r.Skipped = append(r.Skipped, SkippedItem{Path: p, Reason: reason})
}

// runSTRM 遍历挂载目录，生成本地 .strm 文件。
func (r *Runner) runSTRM(ctx context.Context, t store.Task) (int, store.RunInfo, error) {
	root := storage.Join("/"+t.Mount, t.Dir)
	target := strings.TrimSpace(t.Target)
	info := store.RunInfo{Target: target}
	if target == "" {
		return 0, info, fmt.Errorf("任务 %q 没有设置 STRM 输出目录", t.Name)
	}
	// 解析成绝对路径：容器里跑时这里就是 /app/strm/...，和界面填的一一对应
	if abs, aerr := filepath.Abs(target); aerr == nil {
		target = abs
		info.Target = abs
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return 0, info, fmt.Errorf("无法创建输出目录 %s: %w", target, err)
	}
	mode := strings.ToLower(strings.TrimSpace(t.Mode))
	if mode == "" {
		mode = "proxy"
	}

	res := &RunResult{}
	plan := &Plan{}

	// ---------- 1. 规划 ----------
	var walk func(dir string) error
	walk = func(dir string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		entries, err := r.vfs.List(ctx, dir)
		if err != nil {
			return fmt.Errorf("列出目录 %s 失败: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir {
				if err := walk(e.Path); err != nil {
					return err
				}
				continue
			}
			res.Scanned++
			if !strm.IsMedia(e.Name) {
				continue
			}
			// 相对挂载根的子路径，用于在目标目录里保持同样的目录结构
			rel := strings.TrimPrefix(e.Path, "/"+t.Mount)
			dst := strm.LocalPath(target, path.Dir(rel), e.Name)
			plan.Items = append(plan.Items, PlanItem{Source: e.Path, Name: e.Name, Dst: dst})
		}
		return nil
	}
	if err := walk(root); err != nil {
		return 0, info, err
	}

	slog.Info("STRM 任务扫描完成",
		"task", t.Name, "scanned", res.Scanned, "media", len(plan.Items), "target", target)

	// ---------- 2. 执行 ----------
	for _, it := range plan.Items {
		select {
		case <-ctx.Done():
			return res.Written, info, ctx.Err()
		default:
		}
		content, err := r.linkFor(ctx, it.Source, mode)
		if err != nil {
			res.addSkip(it.Source, "生成播放链接失败: "+err.Error())
			slog.Warn("生成链接失败", "path", it.Source, "err", err)
			continue
		}
		changed, err := strm.WriteIfChanged(it.Dst, content)
		if err != nil {
			res.addSkip(it.Source, "写入 "+it.Dst+" 失败: "+err.Error())
			continue
		}
		if changed {
			res.Written++
			if info.Sample == "" {
				info.Sample = it.Dst
			}
			slog.Info("已写入 STRM", "dst", it.Dst)
		}
	}

	// ---------- 3. 汇报 ----------
	for _, s := range res.Skipped {
		slog.Warn("跳过", "path", s.Path, "reason", s.Reason)
	}
	if len(res.Skipped) > 0 && res.Written == 0 {
		return 0, info, fmt.Errorf("共扫描 %d 个文件，%d 个媒体全部失败，首个原因: %s（输出目录 %s）",
			res.Scanned, len(res.Skipped), res.Skipped[0].Reason, info.Target)
	}
	return res.Written, info, nil
}

// linkFor 按模式生成 STRM 内容：直链优先，取不到就回退到本服务代理。
func (r *Runner) linkFor(ctx context.Context, vpath, mode string) (string, error) {
	if mode == "link" {
		u, err := r.vfs.Link(ctx, vpath)
		if err == nil && u != "" {
			return u, nil
		}
		slog.Debug("直链不可用，回退代理", "path", vpath, "err", err)
	}
	u := "/api/stream?path=" + url.QueryEscape(vpath) + "&key=" + r.st.StreamKey()
	if r.baseURL != "" {
		u = strings.TrimSuffix(r.baseURL, "/") + u
	}
	return u, nil
}
