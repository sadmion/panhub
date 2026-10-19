// Package cloud139 适配中国移动云盘（yun.139.com）。
//
// 支持三种登录方式（通过 storage.Authenticator 暴露给前端）：
//   - password      账号 + 密码（邮箱 SSO → artifact → thirdlogin，双层 AES）
//   - authorization 直接粘贴 Basic 后面的授权串
//   - cookies       用 mail.10086.cn 的 Cookie（Os_Sso_Sid + RMKEY）快速换授权
//
// 列目录/直链有两套接口（见 personal.go）：新个人盘（/file/list）与旧个人盘（getDisk）。
// 默认 auto：先新后旧，规避「无接口权限」。
//
// ⚠️ 官方没有开放 API，这些都是社区逆向出来的私有接口（参考 AList / OpenList 的
// 139 驱动）。接口或密钥变更时，改 login.go 顶部的常量即可。
package cloud139

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"panhub/internal/storage"
)

const (
	apiBase   = "https://yun.139.com"
	apiHost   = "yun.139.com"
	svcType   = "1" // 1=个人云 2=家庭云
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	apiAuto     = "auto"
	apiPersonal = "personal"     // 旧个人盘 getDisk
	apiNew      = "personal_new" // 新个人盘 /file/list
)

func init() {
	storage.Register("cloud139", func() storage.Driver { return &Driver{} })
	storage.RegisterMeta("cloud139", "中国移动云盘", []storage.Field{
		{Key: "authorization", Label: "授权（Authorization）", Type: "password", Help: "点上方「登录」自动获取；也可手动粘贴 Basic 后面的值"},
		{Key: "account", Label: "账号", Type: "text", Help: "登录后自动填入，一般不用改"},
		{Key: "api_type", Label: "接口类型", Type: "text", Placeholder: "auto",
			Help: "auto（推荐，先新盘后旧盘）/ personal_new / personal"},
		{Key: "root_id", Label: "根目录 ID", Type: "text", Placeholder: "留空",
			Help: "留空：新盘用 /，旧盘用 root；也可填网页端的 parentFileId / catalogID"},
		{Key: "client_type", Label: "客户端类型", Type: "text", Placeholder: "web",
			Help: "web 或 pc，影响接口上报的客户端标识，默认 web"},
		{Key: "cas_enabled", Label: "还原 .cas 秒传", Type: "bool", Help: "打开后播放 .cas 文件会自动秒传还原（默认开）"},
		{Key: "cas_ttl", Label: "还原缓存时长", Type: "text", Placeholder: "5m",
			Help: "临时文件保留时间；过期后自动移入回收站，再次播放会重新还原"},
		{Key: "cas_temp_dir", Label: "临时目录名", Type: "text", Placeholder: "PANHUB_CAS_TEMP",
			Help: "秒传出来的文件放这里，位于云盘根目录下，会自动创建"},
	})
	storage.RegisterLogin("cloud139", []string{"password", "authorization", "cookies"}, map[string][]storage.LoginField{
		"password": {
			{Key: "account", Label: "账号（手机号/邮箱）", Type: "text", Required: true},
			{Key: "password", Label: "密码", Type: "password", Required: true, Help: "邮箱登录密码，不是短信验证码"},
		},
		"authorization": {
			{Key: "authorization", Label: "Authorization", Type: "password", Required: true, Help: "浏览器登录 yun.139.com 后，从任意请求头复制 Basic 后面的值"},
		},
		"cookies": {
			{Key: "mail_cookies", Label: "mail.10086.cn 的 Cookie", Type: "password", Required: true, Help: "需同时包含 Os_Sso_Sid 与 RMKEY"},
			{Key: "account", Label: "账号", Type: "text", Help: "可选，不填时从 Cookie 推断"},
		},
	})
}

// Driver 实现中国移动云盘驱动。
type Driver struct {
	auth         *AuthInfo
	rootID       string
	userDomainID string
	clientType   string
	client       *http.Client

	mu      sync.Mutex
	host    string // 新个人盘的专属域名（路由策略下发）
	apiMode string // auto / personal_new / personal
	// 新盘接口的路径前缀：正常是 ""，个别地域的 host 需要 "/hcy"
	hcyPrefix string

	// .cas 秒传还原
	casEnabled       bool
	casTTL           time.Duration
	casTempDirName   string
	casMu            sync.Mutex
	casSessions      map[string]*casSession
	casTempDirID     string
	casTempCheckedAt time.Time
}

func (d *Driver) Type() string { return "cloud139" }

func (d *Driver) Caps() storage.Caps {
	return storage.Caps{Read: true, Link: true}
}

func (d *Driver) Init(ctx context.Context, cfg map[string]any) error {
	raw := storage.String(cfg, "authorization", "")
	if raw == "" {
		return fmt.Errorf("%w: 请先在挂载设置里点「登录」获取授权", storage.ErrNotConfigured)
	}
	info, err := ParseAuthorization(raw)
	if err != nil {
		return fmt.Errorf("cloud139: %w", err)
	}
	d.auth = info
	d.rootID = storage.String(cfg, "root_id", "")
	d.userDomainID = storage.String(cfg, "user_domain_id", "")
	d.clientType = strings.ToLower(storage.String(cfg, "client_type", "web"))
	if d.clientType != "pc" {
		d.clientType = "web"
	}

	mode := strings.ToLower(storage.String(cfg, "api_type", apiAuto))
	switch mode {
	case apiAuto, apiPersonal, apiNew:
		d.apiMode = mode
	default:
		return fmt.Errorf("cloud139: api_type 只能是 auto / personal_new / personal，收到 %q", mode)
	}
	d.client = &http.Client{Timeout: 60 * time.Second}

	// .cas 秒传相关
	d.casEnabled = storage.Bool(cfg, "cas_enabled", true)
	d.casTTL = casTTLDefault
	if v := storage.String(cfg, "cas_ttl", ""); v != "" {
		if dur, perr := time.ParseDuration(v); perr == nil && dur >= time.Minute {
			d.casTTL = dur
		}
	}
	d.casTempDirName = storage.String(cfg, "cas_temp_dir", casTempDirDefault)
	d.casSessions = map[string]*casSession{}

	// 快过期就顺手续一次；失败不影响启动，真正调用时会报明确错误。
	if newAuth, rerr := RefreshAuthorization(ctx, info); rerr == nil && newAuth != info.Raw {
		if p, perr := ParseAuthorization(newAuth); perr == nil {
			d.auth = p
		}
	}
	return nil
}

// ---------- 请求 ----------

func randomString(n int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// deviceInfo 是 139 用来识别客户端的一段自定义串，第 2 段是客户端类型（9=web，13=pc）。
func (d *Driver) deviceInfo() string {
	clientType := "9"
	if d.clientType == "pc" {
		clientType = "13"
	}
	return "||" + clientType + "|7.14.0|chrome|120.0.0.0|||windows 10||zh-CN|||dW5kZWZpbmVk||"
}

// headerProfile 决定发哪一套请求头。139 的「旧盘 / 新盘 / 新盘-PC」是三套不同
// 组合，混用会被网关判认证失败；秒传还必须用 PC 那一套。
type headerProfile int

const (
	profileLegacy     headerProfile = iota // 旧个人盘 getDisk、路由策略
	profilePersonal                        // 新个人盘普通接口
	profilePersonalPC                      // 新个人盘 PC 客户端（/file/create 秒传必须）
)

// signedPost 发送带 mcloud-sign 的请求。请求体与签名必须用同一个字符串，
// 不能让 http 层重新序列化。
func (d *Driver) signedPost(ctx context.Context, rawURL string, payload any, profile headerProfile) ([]byte, error) {
	body, err := sortedJSON(payload)
	if err != nil {
		return nil, err
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	randStr := randomString(16)
	sign := calSign(body, ts, randStr)
	signValue := fmt.Sprintf("%s,%s,%s", ts, randStr, sign)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Authorization", "Basic "+d.auth.Raw)
	req.Header.Set("Mcloud-Channel", "1000101")
	req.Header.Set("Mcloud-Client", "10701")
	req.Header.Set("Mcloud-Sign", signValue)
	req.Header.Set("Mcloud-Version", "7.14.0")
	req.Header.Set("x-DeviceInfo", "||9|7.14.0|chrome|120.0.0.0|||windows 10||zh-CN|||")
	req.Header.Set("x-huawei-channelSrc", "10000034")
	req.Header.Set("x-inner-ntwk", "2")
	req.Header.Set("x-m4c-caller", "PC")
	req.Header.Set("x-m4c-src", "10002")
	req.Header.Set("x-SvcType", svcType)
	req.Header.Set("User-Agent", userAgent)

	if profile == profileLegacy {
		req.Header.Set("CMS-DEVICE", "default")
		req.Header.Set("Origin", "https://"+apiHost)
		req.Header.Set("Referer", "https://"+apiHost+"/w/")
		req.Header.Set("Inner-Hcy-Router-Https", "1")
	} else {
		req.Header.Set("Caller", "web")
		req.Header.Set("Cms-Device", "default")
		req.Header.Set("Mcloud-Route", "001")
		req.Header.Set("X-Yun-Api-Version", "v1")
		req.Header.Set("X-Yun-App-Channel", "10000034")
		req.Header.Set("X-Yun-Channel-Source", "10000034")
		req.Header.Set("X-Yun-Client-Info", d.deviceInfo())
		req.Header.Set("X-Yun-Module-Type", "100")
		req.Header.Set("X-Yun-Svc-Type", "1")

		if profile == profilePersonalPC {
			devInfo := d.pcDeviceInfo()
			req.Header.Set("x-DeviceInfo", devInfo)
			req.Header.Set("x-huawei-channelSrc", pcAppChannel)
			req.Header.Set("x-MM-Source", "000")
			req.Header.Set("x-yun-api-version", "v1")
			req.Header.Set("x-yun-app-channel", pcAppChannel)
			req.Header.Set("x-yun-client-info", devInfo)
			req.Header.Set("x-yun-device-id", d.pcDeviceID())
			req.Header.Set("x-yun-device-info", devInfo)
			req.Header.Set("x-yun-market-source", "000")
			req.Header.Set("x-yun-module-type", "100")
			req.Header.Set("x-yun-op-type", "1")
			req.Header.Set("x-yun-svc-type", "1")
			req.Header.Set("x-ExpRoute-Code", "routeCode="+d.auth.Account+",type=2")
		}
	}

	slog.Debug("cloud139 请求", "url", rawURL, "profile", int(profile))

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloud139: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("cloud139: 授权失效（HTTP %d %s）: %w", resp.StatusCode, rawURL, storage.ErrLoginRequired)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cloud139: HTTP %d (%s): %s", resp.StatusCode, rawURL, truncate(string(raw), 200))
	}
	var meta struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("cloud139: 响应解析失败（%s）: %w", rawURL, err)
	}
	if !meta.Success {
		msg := meta.Message
		if msg == "" {
			msg = meta.Code
		}
		if strings.Contains(strings.ToLower(msg), "token") || strings.Contains(msg, "登录") {
			return nil, fmt.Errorf("cloud139: %s: %w", msg, storage.ErrLoginRequired)
		}
		return nil, fmt.Errorf("cloud139: 接口返回失败(%s): %s", meta.Code, msg)
	}
	return raw, nil
}

// personalPostWith 调新个人盘接口；遇到 404 会自动补 /hcy 前缀并记住。
func (d *Driver) personalPostWith(ctx context.Context, pathname string, payload any, profile headerProfile) ([]byte, error) {
	host, err := d.ensureHost(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	prefix := d.hcyPrefix
	d.mu.Unlock()

	raw, err := d.signedPost(ctx, host+prefix+pathname, payload, profile)
	if err != nil && prefix == "" && strings.Contains(err.Error(), "HTTP 404") {
		if raw2, err2 := d.signedPost(ctx, host+"/hcy"+pathname, payload, profile); err2 == nil {
			d.mu.Lock()
			d.hcyPrefix = "/hcy"
			d.mu.Unlock()
			return raw2, nil
		}
	}
	return raw, err
}

func (d *Driver) legacyPost(ctx context.Context, pathname string, payload any) ([]byte, error) {
	return d.signedPost(ctx, apiBase+pathname, payload, profileLegacy)
}

// ---------- 旧个人盘（getDisk） ----------

type diskItem struct {
	ID      string
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

type diskResp struct {
	Success bool `json:"success"`
	Data    struct {
		GetDiskResult struct {
			NodeCount   int `json:"nodeCount"`
			CatalogList []struct {
				CatalogID   string `json:"catalogID"`
				CatalogName string `json:"catalogName"`
				UpdateTime  string `json:"updateTime"`
				CreateTime  string `json:"createTime"`
			} `json:"catalogList"`
			ContentList []struct {
				ContentID   string `json:"contentID"`
				ContentName string `json:"contentName"`
				ContentSize int64  `json:"contentSize"`
				UpdateTime  string `json:"updateTime"`
				Digest      string `json:"digest"`
			} `json:"contentList"`
		} `json:"getDiskResult"`
	} `json:"data"`
}

func parseTime(s string) time.Time {
	for _, layout := range []string{
		"20060102150405",
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999-07:00",
		"2006-01-02T15:04:05-07:00",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (d *Driver) legacyRoot() string {
	if d.rootID == "" || d.rootID == "/" {
		return "root"
	}
	return d.rootID
}

func (d *Driver) legacyListAll(ctx context.Context, catalogID string) ([]diskItem, error) {
	const page = 100
	var out []diskItem
	for start := 0; start < 100000; start += page {
		payload := map[string]any{
			"catalogID":         catalogID,
			"sortDirection":     1,
			"startNumber":       start + 1,
			"endNumber":         start + page,
			"filterType":        0,
			"catalogSortType":   0,
			"contentSortType":   0,
			"commonAccountInfo": map[string]any{"account": d.auth.Account, "accountType": 1},
		}
		raw, err := d.legacyPost(ctx, "/orchestration/personalCloud/catalog/v1.0/getDisk", payload)
		if err != nil {
			return nil, err
		}
		var r diskResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("cloud139: 目录响应解析失败: %w", err)
		}
		res := r.Data.GetDiskResult
		for _, c := range res.CatalogList {
			out = append(out, diskItem{ID: c.CatalogID, Name: c.CatalogName, IsDir: true, ModTime: parseTime(c.UpdateTime)})
		}
		for _, c := range res.ContentList {
			out = append(out, diskItem{ID: c.ContentID, Name: c.ContentName, Size: c.ContentSize, ModTime: parseTime(c.UpdateTime)})
		}
		if len(res.CatalogList)+len(res.ContentList) == 0 || start+page >= res.NodeCount {
			break
		}
	}
	return out, nil
}

func (d *Driver) legacyResolveDir(ctx context.Context, p string) (string, error) {
	p = storage.Clean(p)
	if p == "/" {
		return d.legacyRoot(), nil
	}
	cur := d.legacyRoot()
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" {
			continue
		}
		items, err := d.legacyListAll(ctx, cur)
		if err != nil {
			return "", err
		}
		found := false
		for _, it := range items {
			if it.IsDir && it.Name == seg {
				cur = it.ID
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

func (d *Driver) legacyListEntries(ctx context.Context, dir string) ([]storage.Entry, error) {
	id, err := d.legacyResolveDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	items, err := d.legacyListAll(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]storage.Entry, 0, len(items))
	for _, it := range items {
		out = append(out, storage.Entry{
			Name:    it.Name,
			Path:    storage.Join(dir, it.Name),
			IsDir:   it.IsDir,
			Size:    it.Size,
			ModTime: it.ModTime,
			ID:      it.ID,
		})
	}
	storage.SortEntries(out)
	return out, nil
}

func (d *Driver) legacyLink(ctx context.Context, fileID string) (string, error) {
	payload := map[string]any{
		"appName":           "",
		"contentID":         fileID,
		"commonAccountInfo": map[string]any{"account": d.auth.Account, "accountType": 1},
	}
	raw, err := d.legacyPost(ctx, "/orchestration/personalCloud/uploadAndDownload/v1.0/downloadRequest", payload)
	if err != nil {
		return "", err
	}
	var r struct {
		Data struct {
			DownloadURL string `json:"downloadURL"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if r.Data.DownloadURL == "" {
		return "", errors.New("cloud139: 接口没有返回下载地址")
	}
	return r.Data.DownloadURL, nil
}

// ---------- 接口分发（新盘优先，旧盘兜底） ----------

func (d *Driver) api() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.apiMode
}

func (d *Driver) setAPI(mode string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.apiMode == apiAuto {
		d.apiMode = mode
	}
}

func (d *Driver) List(ctx context.Context, dir string) ([]storage.Entry, error) {
	switch d.api() {
	case apiNew:
		return d.personalListEntries(ctx, dir)
	case apiPersonal:
		return d.legacyListEntries(ctx, dir)
	}

	// auto：先新后旧
	es, errNew := d.personalListEntries(ctx, dir)
	if errNew == nil {
		d.setAPI(apiNew)
		return es, nil
	}
	es, errOld := d.legacyListEntries(ctx, dir)
	if errOld == nil {
		d.setAPI(apiPersonal)
		return es, nil
	}
	return nil, fmt.Errorf("cloud139: 新个人盘接口失败（%v）；旧接口也失败（%v）", errNew, errOld)
}

func (d *Driver) Stat(ctx context.Context, p string) (storage.Entry, error) {
	p = storage.Clean(p)
	if p == "/" {
		return storage.Entry{Name: "/", Path: "/", IsDir: true}, nil
	}
	es, err := d.List(ctx, storage.Parent(p))
	if err != nil {
		return storage.Entry{}, err
	}
	name := storage.Base(p)
	for _, e := range es {
		if e.Name == name {
			return e, nil
		}
	}
	return storage.Entry{}, storage.ErrNotFound
}

// Link 取下载直链（302 播放用）。.cas 文件会先做秒传还原。
func (d *Driver) Link(ctx context.Context, p string) (string, error) {
	if d.casEnabledNow() && isCASName(p) {
		return d.restoreCAS(ctx, p)
	}
	e, err := d.Stat(ctx, p)
	if err != nil {
		return "", err
	}
	if e.IsDir {
		return "", storage.ErrNotSupported
	}
	if e.ID == "" {
		return "", storage.ErrNotFound
	}
	return d.linkByID(ctx, e.ID)
}

// linkByID 按文件 ID 取直链，自动在新旧接口之间选择。
func (d *Driver) linkByID(ctx context.Context, id string) (string, error) {
	switch d.api() {
	case apiNew:
		return d.personalLink(ctx, id)
	case apiPersonal:
		return d.legacyLink(ctx, id)
	}
	if u, errNew := d.personalLink(ctx, id); errNew == nil {
		d.setAPI(apiNew)
		return u, nil
	}
	u, errOld := d.legacyLink(ctx, id)
	if errOld != nil {
		return "", errOld
	}
	d.setAPI(apiPersonal)
	return u, nil
}

// Open 以流方式读取文件（代理播放/下载）。.cas 会先还原再转发。
func (d *Driver) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return d.OpenRange(ctx, p, 0)
}

// OpenRange 支持 Range，播放器拖进度依赖它。
func (d *Driver) OpenRange(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	if d.casEnabledNow() && isCASName(p) {
		return d.openCAS(ctx, p, offset)
	}
	url, err := d.Link(ctx, p)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("cloud139: 下载失败 HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// ---------- 登录 ----------

// Login 实现 storage.Authenticator。
func (d *Driver) Login(ctx context.Context, req storage.LoginRequest) (*storage.LoginResult, error) {
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "password"
	}

	var auth, userDomainID string
	session := map[string]any{}

	switch mode {
	case "password":
		c := &Credentials{
			Account:     req.Get("account"),
			Password:    req.Get("password"),
			MailCookies: req.Get("mail_cookies"),
		}
		newAuth, cookies, udid, err := LoginWithPassword(ctx, c)
		if err != nil {
			return nil, err
		}
		auth, userDomainID = newAuth, udid
		session["mail_cookies"] = cookies
	case "authorization":
		info, err := ParseAuthorization(req.Get("authorization"))
		if err != nil {
			return nil, err
		}
		auth = info.Raw
		if refreshed, rerr := RefreshAuthorization(ctx, info); rerr == nil {
			auth = refreshed
		}
	case "cookies":
		c := &Credentials{Account: req.Get("account"), MailCookies: req.Get("mail_cookies")}
		newAuth, udid, err := LoginWithCookies(ctx, c)
		if err != nil {
			return nil, err
		}
		auth, userDomainID = newAuth, udid
	default:
		return nil, fmt.Errorf("不支持的登录方式: %s", mode)
	}

	info, err := ParseAuthorization(auth)
	if err != nil {
		return nil, err
	}
	if userDomainID == "" {
		userDomainID = req.Get("user_domain_id")
	}

	session["authorization"] = info.Raw
	session["account"] = info.Account
	if userDomainID != "" {
		session["user_domain_id"] = userDomainID
	}
	if !info.ExpiresAt.IsZero() {
		session["authorization_expires_at"] = info.ExpiresAt.Format(time.RFC3339)
	}

	profile := map[string]any{"account": info.Account}
	if !info.ExpiresAt.IsZero() {
		profile["expires_at"] = info.ExpiresAt.Format("2006-01-02 15:04:05")
	}
	if userDomainID != "" {
		profile["user_domain_id"] = userDomainID
	}

	// 用一次根目录列举验证凭证是否真的可用（auto 模式会自动挑能用的接口）。
	probe := &Driver{
		auth:       info,
		clientType: "web",
		client:     &http.Client{Timeout: 30 * time.Second},
		apiMode:    apiAuto,
	}
	items, perr := probe.List(ctx, "/")
	if perr != nil {
		profile["probe_ok"] = false
		return &storage.LoginResult{
			Session: session,
			Profile: profile,
			Message: "已拿到授权，但根目录列举失败（" + perr.Error() + "）。可以先保存，再到「文件浏览」确认。",
		}, nil
	}
	profile["probe_ok"] = true
	profile["api_mode"] = probe.api()
	profile["root_items"] = len(items)
	return &storage.LoginResult{Session: session, Profile: profile}, nil
}

var _ storage.Authenticator = (*Driver)(nil)
