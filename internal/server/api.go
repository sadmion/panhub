package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"panhub/internal/storage"
	"panhub/internal/store"
	"panhub/internal/task"
	"panhub/internal/vfs"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// extraMime 补齐系统 mime 表里缺失的媒体类型，播放器据此判断直接播放。
var extraMime = map[string]string{
	".mkv":  "video/x-matroska",
	".ts":   "video/mp2t",
	".m2ts": "video/mp2t",
	".flv":  "video/x-flv",
	".rmvb": "video/vnd.rn-realvideo",
	".m4v":  "video/x-m4v",
	".iso":  "application/octet-stream",
	".flac": "audio/flac",
	".ape":  "audio/x-ape",
}

// ---------- 认证 ----------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !s.tokens.checkPassword(req.Password) {
		writeError(w, http.StatusUnauthorized, "密码错误")
		return
	}
	tok, exp := s.tokens.issue()
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires_at": exp})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.tokens.revoke(bearer(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 状态与驱动 ----------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	snap := s.st.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.version,
		"started_at": s.started,
		"uptime":     time.Since(s.started).Round(time.Second).String(),
		"mounts":     len(snap.Mounts),
		"tasks":      len(snap.Tasks),
		"stream_key": snap.StreamKey,
		"base_url":   s.cfg.Server.PublicBaseURL,
		"addr":       s.cfg.Server.Addr,
	})
}

func (s *Server) drivers(w http.ResponseWriter, _ *http.Request) {
	ms := storage.AllMeta()
	sort.Slice(ms, func(i, j int) bool { return ms[i].Type < ms[j].Type })
	writeJSON(w, http.StatusOK, map[string]any{"drivers": ms})
}

// ---------- 挂载点 ----------

type mountView struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	ReadOnly bool           `json:"read_only"`
	Caps     storage.Caps   `json:"caps"`
	Config   map[string]any `json:"config"`
}

func (s *Server) listMounts(w http.ResponseWriter, _ *http.Request) {
	ms := s.v.Mounts()
	out := make([]mountView, 0, len(ms))
	for _, m := range ms {
		out = append(out, mountView{
			Name:     m.Name,
			Type:     m.Type,
			ReadOnly: m.ReadOnly,
			Caps:     m.Driver.Caps(),
			Config:   storage.MaskConfig(m.Config),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mounts": out})
}

type mountReq struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	ReadOnly bool           `json:"read_only"`
	Config   map[string]any `json:"config"`
}

func (s *Server) buildMount(req mountReq, old *vfs.Mount) (*vfs.Mount, error) {
	if !nameRe.MatchString(req.Name) {
		return nil, errors.New("挂载名只能包含字母、数字、下划线和短横线，且以字母或数字开头（最长 32）")
	}
	if req.Type == "" {
		if old != nil {
			req.Type = old.Type
		} else {
			return nil, errors.New("必须选择驱动类型")
		}
	}
	cfg := req.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	// 编辑时如果前端提交的是打码值，则沿用旧配置里的真实值。
	if old != nil {
		merged := map[string]any{}
		for k, v := range old.Config {
			merged[k] = v
		}
		for k, v := range cfg {
			if sv, ok := v.(string); ok && sv == "******" {
				continue
			}
			merged[k] = v
		}
		cfg = merged
	}
	drv, err := storage.New(context.Background(), req.Type, cfg)
	if err != nil {
		return nil, fmt.Errorf("驱动初始化失败: %w", err)
	}
	return &vfs.Mount{Name: req.Name, Type: req.Type, ReadOnly: req.ReadOnly, Driver: drv, Config: cfg}, nil
}

func (s *Server) createMount(w http.ResponseWriter, r *http.Request) {
	var req mountReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	if _, exists := s.v.Get(req.Name); exists {
		writeError(w, http.StatusConflict, "挂载名已存在")
		return
	}
	mt, err := s.buildMount(req, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.v.Add(mt)
	if err := s.st.UpsertMount(store.Mount{Name: mt.Name, Type: mt.Type, ReadOnly: mt.ReadOnly, Config: mt.Config}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": mt.Name})
}

func (s *Server) updateMount(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	old, ok := s.v.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "挂载点不存在")
		return
	}
	var req mountReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	if req.Name == "" {
		req.Name = name
	}
	if req.Name != name {
		writeError(w, http.StatusBadRequest, "不支持修改挂载名，请删除后重建")
		return
	}
	mt, err := s.buildMount(req, old)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.v.Add(mt)
	if err := s.st.UpsertMount(store.Mount{Name: mt.Name, Type: mt.Type, ReadOnly: mt.ReadOnly, Config: mt.Config}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) deleteMount(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	if _, ok := s.v.Get(name); !ok {
		writeError(w, http.StatusNotFound, "挂载点不存在")
		return
	}
	s.v.RemoveMount(name)
	if err := s.st.DeleteMount(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 文件浏览 ----------

func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	p := storage.Clean(r.URL.Query().Get("path"))
	entries, err := s.v.List(r.Context(), p)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "entries": entries})
}

func (s *Server) fsStat(w http.ResponseWriter, r *http.Request) {
	p := storage.Clean(r.URL.Query().Get("path"))
	e, err := s.v.Stat(r.Context(), p)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) fsLink(w http.ResponseWriter, r *http.Request) {
	p := storage.Clean(r.URL.Query().Get("path"))
	u, err := s.v.Link(r.Context(), p)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": u})
}

func (s *Server) fsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.v.Mkdir(r.Context(), storage.Clean(req.Path)); err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) fsRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.v.Remove(r.Context(), storage.Clean(req.Path), req.Recursive); err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) fsMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.v.Move(r.Context(), storage.Clean(req.From), storage.Clean(req.To)); err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) fsCopy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.v.Copy(r.Context(), storage.Clean(req.From), storage.Clean(req.To)); err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// fsUpload 用流式 multipart，避免大文件占内存。
func (s *Server) fsUpload(w http.ResponseWriter, r *http.Request) {
	dir := storage.Clean(r.URL.Query().Get("path"))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "需要 multipart/form-data 上传")
		return
	}
	uploaded := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "读取上传流失败: "+err.Error())
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		dst := storage.Join(dir, name)
		if err := s.v.Upload(r.Context(), dst, part, -1); err != nil {
			part.Close()
			mapError(w, err)
			return
		}
		part.Close()
		uploaded++
	}
	if uploaded == 0 {
		writeError(w, http.StatusBadRequest, "没有收到文件")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": uploaded})
}

// driverLogin 处理「账号密码/ Cookie / 粘贴授权」这类登录请求。
// 返回的 session 由前端合并进挂载配置，再由前端保存。
func (s *Server) driverLogin(w http.ResponseWriter, r *http.Request) {
	typ := chi.URLParam(r, "type")
	raw, ok := storage.NewRaw(typ)
	if !ok {
		writeError(w, http.StatusNotFound, "未知的驱动类型: "+typ)
		return
	}
	a, ok := raw.(storage.Authenticator)
	if !ok {
		writeError(w, http.StatusBadRequest, "该驱动不支持登录")
		return
	}
	var req storage.LoginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	res, err := a.Login(ctx, req)
	if err != nil {
		if errors.Is(err, storage.ErrLoginRequired) {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// fsDownload 下载文件（带 Content-Disposition）。

func (s *Server) fsDownload(w http.ResponseWriter, r *http.Request) {
	p := storage.Clean(r.URL.Query().Get("path"))
	s.serveFile(w, r, p, true)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("key") != s.st.StreamKey() {
		writeError(w, http.StatusForbidden, "invalid stream key")
		return
	}
	s.serveFile(w, r, storage.Clean(q.Get("path")), false)
}

// serveFile 支持 Range 请求，Emby/Jellyfin 拖拽进度依赖它。
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, p string, download bool) {
	e, err := s.v.Stat(r.Context(), p)
	if err != nil {
		mapError(w, err)
		return
	}
	if e.IsDir {
		writeError(w, http.StatusBadRequest, "这是一个目录")
		return
	}
	ctype := mime.TypeByExtension(path.Ext(e.Name))
	if ctype == "" {
		// Windows 的注册表 mime 表常常缺这些后缀，显式兜底。
		ctype = extraMime[strings.ToLower(path.Ext(e.Name))]
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", ctype)
	if download {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEncode(e.Name)))
	}
	if e.Size <= 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	start, end, partial, rerr := parseRange(r.Header.Get("Range"), e.Size)
	if rerr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", e.Size))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, rerr.Error())
		return
	}
	length := end - start + 1
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, e.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	rc, err := s.v.Open(r.Context(), p, start)
	if err != nil {
		return // 响应头已发出，只能中断
	}
	defer rc.Close()
	_, _ = io.CopyN(w, rc, length)
}

func parseRange(h string, size int64) (start, end int64, partial bool, err error) {
	if h == "" || !strings.HasPrefix(h, "bytes=") {
		return 0, size - 1, false, nil
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	a, b, _ := strings.Cut(spec, "-")
	if a == "" {
		n, perr := strconv.ParseInt(b, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, false, errors.New("非法的 Range")
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}
	start, err = strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, false, errors.New("非法的 Range")
	}
	if b == "" {
		end = size - 1
	} else if end, err = strconv.ParseInt(b, 10, 64); err != nil {
		return 0, 0, false, errors.New("非法的 Range")
	}
	if start < 0 || start >= size || end < start {
		return 0, 0, false, errors.New("Range 超出文件大小")
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true, nil
}

func urlEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---------- 任务 ----------

func (s *Server) listTasks(w http.ResponseWriter, _ *http.Request) {
	ts := s.st.Tasks()
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":      ts,
		"stream_key": s.st.StreamKey(),
		"media_ext":  mediaExts(),
	})
}

func mediaExts() []string {
	// 与 strm 包保持一致的展示用列表
	return []string{"mp4", "mkv", "avi", "mov", "wmv", "flv", "ts", "m2ts", "rmvb", "webm", "mp3", "flac", "wav", "aac", "m4a"}
}

type taskReq struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Mount    string `json:"mount"`
	Dir      string `json:"dir"`
	Target   string `json:"target"`
	Mode     string `json:"mode"`
	Interval string `json:"interval"`
	Enabled  bool   `json:"enabled"`
}

func (s *Server) validateTask(t store.Task) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("任务名不能为空")
	}
	if t.Type != "strm" {
		return errors.New("当前只支持 strm 类型任务")
	}
	if _, ok := s.v.Get(t.Mount); !ok {
		return errors.New("挂载点不存在: " + t.Mount)
	}
	if t.Target == "" {
		return errors.New("必须填写 STRM 输出目录（宿主机路径）")
	}
	if _, err := task.ParseInterval(t.Interval); err != nil {
		return err
	}
	return nil
}

func fromReq(req taskReq) store.Task {
	return store.Task{
		ID:       req.ID,
		Name:     strings.TrimSpace(req.Name),
		Type:     req.Type,
		Mount:    req.Mount,
		Dir:      storage.Clean(req.Dir),
		Target:   req.Target,
		Mode:     req.Mode,
		Interval: req.Interval,
		Enabled:  req.Enabled,
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req taskReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	t := fromReq(req)
	t.ID = store.NewTaskID()
	if t.Type == "" {
		t.Type = "strm"
	}
	if t.Mode == "" {
		t.Mode = "proxy"
	}
	if err := s.validateTask(t); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.UpsertTask(t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.runner.Reload()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": t.ID})
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	if _, ok := s.st.GetTask(id); !ok {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	var req taskReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	req.ID = id
	t := fromReq(req)
	if err := s.validateTask(t); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.UpsertTask(t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.runner.Reload()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	if err := s.st.DeleteTask(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.runner.Reload()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	// 路径形如 /api/tasks/{id}/run
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, "缺少任务 ID")
		return
	}
	id := parts[len(parts)-2]
	done := make(chan error, 1)
	go func() { done <- s.runner.RunNow(id) }()
	select {
	case err := <-done:
		if err != nil {
			mapError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case <-time.After(30 * time.Second):
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "note": "任务仍在后台执行"})
	}
}

// fsCheckTarget 检查 STRM 输出目录是否可写（任务表单里的「测试目录」按钮）。
// 只做一次探测文件，不保留任何内容。
func (s *Server) fsCheckTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	dir := strings.TrimSpace(req.Path)
	if dir == "" {
		writeError(w, http.StatusBadRequest, "输出目录不能为空")
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "路径无效: "+err.Error())
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeError(w, http.StatusBadRequest, "无法创建目录: "+err.Error())
		return
	}
	probe := filepath.Join(abs, ".panhub-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		writeError(w, http.StatusBadRequest, "目录不可写: "+err.Error())
		return
	}
	_ = os.Remove(probe)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": abs})
}