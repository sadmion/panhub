package cloud139

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"panhub/internal/storage"
)

// 新个人盘接口（/file/list）。相比旧版 getDisk：
//   - 先调 /user/route/qryRoutePolicy 拿到账号专属 host（分地域）
//   - 用 parentFileId 定位目录，pageCursor 翻页
//   - 直链走 /file/getDownloadUrl
//
// 已迁移到新盘的账号调旧 getDisk 会返回「无接口权限」，所以 auto 模式下优先走这里。

type personalItem struct {
	FileID    string `json:"fileId"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Type      string `json:"type"` // folder / file
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type personalListResp struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Items          []personalItem `json:"items"`
		NextPageCursor string         `json:"nextPageCursor"`
	} `json:"data"`
}

// queryRoutePolicy 查出该账号应使用的个人云 host（分地域）。
func (d *Driver) queryRoutePolicy(ctx context.Context) (string, error) {
	payload := map[string]any{
		"userInfo": map[string]any{
			"userType":    1,
			"accountType": 1,
			"accountName": d.auth.Account,
		},
		"modAddrType": 1,
	}
	raw, err := d.signedPost(ctx, "https://user-njs.yun.139.com/user/route/qryRoutePolicy", payload, profileLegacy)
	if err != nil {
		return "", err
	}
	var r struct {
		Success bool `json:"success"`
		Data    struct {
			RoutePolicyList []struct {
				ModName  string `json:"modName"`
				HttpsURL string `json:"httpsUrl"`
			} `json:"routePolicyList"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("cloud139: 路由策略解析失败: %w", err)
	}
	for _, p := range r.Data.RoutePolicyList {
		if p.ModName == "personal" && p.HttpsURL != "" {
			return strings.TrimSuffix(p.HttpsURL, "/"), nil
		}
	}
	return "", fmt.Errorf("cloud139: 路由策略里没有 personal 节点")
}

// ensureHost 惰性获取并缓存个人云 host。
func (d *Driver) ensureHost(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.host != "" {
		return d.host, nil
	}
	host, err := d.queryRoutePolicy(ctx)
	if err != nil {
		return "", err
	}
	d.host = host
	return host, nil
}

func (d *Driver) personalPost(ctx context.Context, pathname string, payload any) ([]byte, error) {
	return d.personalPostWith(ctx, pathname, payload, profilePersonal)
}

// personalList 列出新个人盘某个 fileId 下的内容（自动翻页）。
func (d *Driver) personalList(ctx context.Context, parentID string) ([]personalItem, error) {
	var out []personalItem
	cursor := ""
	for i := 0; i < 1000; i++ {
		payload := map[string]any{
			"imageThumbnailStyleList": []string{"Small", "Large"},
			"orderBy":                 "updated_at",
			"orderDirection":          "DESC",
			"pageInfo": map[string]any{
				"pageCursor": cursor,
				"pageSize":   100,
			},
			"parentFileId": parentID,
		}
		raw, err := d.personalPost(ctx, "/file/list", payload)
		if err != nil {
			return nil, err
		}
		var r personalListResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("cloud139: 新个人盘目录解析失败: %w", err)
		}
		if r.Code != "" && !r.Success {
			return nil, fmt.Errorf("cloud139: 新个人盘返回错误: %s", r.Message)
		}
		out = append(out, r.Data.Items...)
		if r.Data.NextPageCursor == "" {
			break
		}
		cursor = r.Data.NextPageCursor
	}
	return out, nil
}

// personalRoot 返回新个人盘的根 ID（默认 "/"）。
func (d *Driver) personalRoot() string {
	if d.rootID != "" && d.rootID != "root" {
		return d.rootID
	}
	return "/"
}

func (d *Driver) personalResolveDir(ctx context.Context, p string) (string, error) {
	p = storage.Clean(p)
	if p == "/" {
		return d.personalRoot(), nil
	}
	cur := d.personalRoot()
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" {
			continue
		}
		items, err := d.personalList(ctx, cur)
		if err != nil {
			return "", err
		}
		found := false
		for _, it := range items {
			if it.Type == "folder" && it.Name == seg {
				cur = it.FileID
				found = true
				break
			}
		}
		if !found {
			return "", storage.ErrNotFound
		}
	}
	return cur, nil
}

func (d *Driver) personalListEntries(ctx context.Context, dir string) ([]storage.Entry, error) {
	id, err := d.personalResolveDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	items, err := d.personalList(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]storage.Entry, 0, len(items))
	for _, it := range items {
		out = append(out, storage.Entry{
			Name:    it.Name,
			Path:    storage.Join(dir, it.Name),
			IsDir:   it.Type == "folder",
			Size:    it.Size,
			ModTime: parseTime(it.UpdatedAt),
			ID:      it.FileID,
		})
	}
	storage.SortEntries(out)
	return out, nil
}

// personalLink 取新个人盘的下载直链。
func (d *Driver) personalLink(ctx context.Context, fileID string) (string, error) {
	raw, err := d.personalPost(ctx, "/file/getDownloadUrl", map[string]any{"fileId": fileID})
	if err != nil {
		return "", err
	}
	var r struct {
		Data struct {
			CDNURL    string `json:"cdnUrl"`
			CDNSwitch bool   `json:"cdnSwitch"`
			URL       string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if r.Data.CDNSwitch && r.Data.CDNURL != "" {
		return r.Data.CDNURL, nil
	}
	if r.Data.URL != "" {
		return r.Data.URL, nil
	}
	return "", fmt.Errorf("cloud139: 新个人盘没有返回下载地址")
}
