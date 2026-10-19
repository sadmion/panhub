// Package cloud189 适配天翼云盘（cloud.189.cn）。
//
// ⚠️ 实验性：天翼云盘没有官方开放的第三方 API，这里用的是网页版
// 会话 Cookie 调用的内部接口。官方随时可能改动，字段/路径如失效
// 请直接改本文件（endpoints 集中在下方常量里）。
//
// 配置：
//
//	cookie:    浏览器里 cloud.189.cn 的完整 Cookie 字符串（必填）
//	root_id:   起始目录 ID，个人云根目录是 -11（默认）
//	user_agent: 可选，默认伪装桌面浏览器
package cloud189

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"panhub/internal/storage"
)

const (
	apiBase       = "https://cloud.189.cn"
	pathListFiles = "/api/open/file/listFiles.action"
	defaultRootID = "-11"
	defaultUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	pageSize      = 1000
)

func init() {
	storage.Register("cloud189", func() storage.Driver { return &Driver{} })
	storage.RegisterMeta("cloud189", "天翼云盘（实验性）", []storage.Field{
		{Key: "cookie", Label: "Cookie", Type: "password", Required: true, Help: "浏览器登录 cloud.189.cn 后，F12 → Network → 复制任意请求的完整 Cookie"},
		{Key: "root_id", Label: "起始目录 ID", Type: "text", Placeholder: "-11", Help: "个人云根目录是 -11，家庭云请填家庭云 ID"},
		{Key: "user_agent", Label: "User-Agent", Type: "text", Help: "可选，一般留空"},
	})
}

// Driver 实现天翼云盘驱动。
type Driver struct {
	cookie string
	ua     string
	rootID string
	client *http.Client
}

func (d *Driver) Type() string { return "cloud189" }

func (d *Driver) Caps() storage.Caps {
	// 只读：写入接口需要额外的签名流程，待联调后再打开。
	return storage.Caps{Read: true}
}

func (d *Driver) Init(_ context.Context, cfg map[string]any) error {
	d.cookie = storage.String(cfg, "cookie", "")
	if d.cookie == "" {
		return errors.New("cloud189: 需要配置 cookie（浏览器登录后复制完整 Cookie）")
	}
	d.rootID = storage.String(cfg, "root_id", defaultRootID)
	d.ua = storage.String(cfg, "user_agent", defaultUA)
	d.client = &http.Client{Timeout: 60 * time.Second}
	return nil
}

type fileItem struct {
	ID         json.Number `json:"id"`
	Name       string      `json:"name"`
	Size       int64       `json:"size"`
	IsFolder   bool        `json:"isFolder"`
	LastOpTime string      `json:"lastOpTime"`
	CreateTime string      `json:"createDate"`
}

type fileListAO struct {
	Count      int        `json:"count"`
	FileList   []fileItem `json:"fileList"`
	FolderList []fileItem `json:"folderList"`
}

type listResp struct {
	ResCode    json.Number `json:"res_code"`
	ResMessage string      `json:"res_message"`
	FileListAO *fileListAO `json:"fileListAO"`
}

func (d *Driver) get(ctx context.Context, p string, q url.Values) ([]byte, error) {
	u := apiBase + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", d.cookie)
	req.Header.Set("User-Agent", d.ua)
	req.Header.Set("Accept", "application/json;charset=UTF-8")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("cloud189: 认证失败 (HTTP %d)，Cookie 可能已过期", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cloud189: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// listDir 列出某个 folderId 下的内容。
func (d *Driver) listDir(ctx context.Context, folderID string) ([]fileItem, error) {
	q := url.Values{}
	q.Set("folderId", folderID)
	q.Set("pageNum", "1")
	q.Set("pageSize", strconv.Itoa(pageSize))
	q.Set("mediaType", "0")
	q.Set("orderBy", "lastOpTime")
	q.Set("descending", "true")

	body, err := d.get(ctx, pathListFiles, q)
	if err != nil {
		return nil, err
	}
	var lr listResp
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&lr); err != nil {
		return nil, fmt.Errorf("cloud189: 响应解析失败（接口可能已变更）: %w", err)
	}
	if lr.ResCode.String() != "0" && lr.ResCode.String() != "" {
		return nil, fmt.Errorf("cloud189: 接口返回错误 res_code=%s %s", lr.ResCode, lr.ResMessage)
	}
	if lr.FileListAO == nil {
		return nil, nil
	}
	out := make([]fileItem, 0, len(lr.FileListAO.FolderList)+len(lr.FileListAO.FileList))
	out = append(out, lr.FileListAO.FolderList...)
	out = append(out, lr.FileListAO.FileList...)
	return out, nil
}

// resolveDir 把虚拟路径逐段解析成 folderId。
// 天翼云盘用 ID 定位目录，所以这里按名字逐级查找。
func (d *Driver) resolveDir(ctx context.Context, p string) (string, error) {
	p = storage.Clean(p)
	if p == "/" {
		return d.rootID, nil
	}
	cur := d.rootID
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" {
			continue
		}
		items, err := d.listDir(ctx, cur)
		if err != nil {
			return "", err
		}
		var found *fileItem
		for i := range items {
			if items[i].Name == seg && items[i].IsFolder {
				found = &items[i]
				break
			}
		}
		if found == nil {
			return "", storage.ErrNotFound
		}
		cur = found.ID.String()
	}
	return cur, nil
}

func parseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (d *Driver) List(ctx context.Context, dir string) ([]storage.Entry, error) {
	id, err := d.resolveDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	items, err := d.listDir(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]storage.Entry, 0, len(items))
	for _, it := range items {
		mt := parseTime(it.LastOpTime)
		if mt.IsZero() {
			mt = parseTime(it.CreateTime)
		}
		out = append(out, storage.Entry{
			Name:    it.Name,
			Path:    storage.Join(dir, it.Name),
			IsDir:   it.IsFolder,
			Size:    it.Size,
			ModTime: mt,
			ID:      it.ID.String(),
		})
	}
	storage.SortEntries(out)
	return out, nil
}

func (d *Driver) Stat(ctx context.Context, p string) (storage.Entry, error) {
	p = storage.Clean(p)
	if p == "/" {
		return storage.Entry{Name: "/", Path: "/", IsDir: true}, nil
	}
	parent := storage.Parent(p)
	name := storage.Base(p)
	es, err := d.List(ctx, parent)
	if err != nil {
		return storage.Entry{}, err
	}
	for _, e := range es {
		if e.Name == name {
			return e, nil
		}
	}
	return storage.Entry{}, storage.ErrNotFound
}

// Link 天翼云盘的直链需要 appId + 签名换取，暂未实现；
// 上层会自动回退到代理播放（/api/stream）。
func (d *Driver) Link(_ context.Context, _ string) (string, error) {
	return "", storage.ErrNotSupported
}
