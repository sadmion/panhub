// Package webdav 把一个远程 WebDAV 服务挂载成网盘。
// 这里手写 WebDAV 协议（PROPFIND/MKCOL/MOVE/COPY/PUT/GET），
// 不依赖第三方客户端库，方便你按需改造或加自定义 Header。
package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"panhub/internal/storage"
)

func init() {
	storage.Register("webdav", func() storage.Driver { return &Driver{} })
	storage.RegisterMeta("webdav", "WebDAV", []storage.Field{
		{Key: "url", Label: "服务地址", Type: "text", Required: true, Placeholder: "https://dav.example.com/dav", Help: "要包含路径前缀，例如 Alist 的 http://ip:5244/dav"},
		{Key: "username", Label: "用户名", Type: "text"},
		{Key: "password", Label: "密码", Type: "password"},
	})
}

// Driver 实现 WebDAV 客户端驱动。
type Driver struct {
	base     *url.URL
	username string
	password string
	client   *http.Client
}

func (d *Driver) Type() string { return "webdav" }

func (d *Driver) Caps() storage.Caps {
	return storage.Caps{Read: true, Write: true, Move: true, Copy: true, Upload: true}
}

func (d *Driver) Init(_ context.Context, cfg map[string]any) error {
	raw := storage.String(cfg, "url", "")
	if raw == "" {
		return errors.New("webdav: 配置项 url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webdav: url 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("webdav: url 必须以 http:// 或 https:// 开头")
	}
	d.base = u
	d.username = storage.String(cfg, "username", "")
	d.password = storage.String(cfg, "password", "")
	d.client = &http.Client{Timeout: 0} // 大文件传输不设总超时
	return nil
}

// urlFor 拼接 base 路径前缀与虚拟路径。
func (d *Driver) urlFor(p string) string {
	u := *d.base
	u.Path = strings.TrimSuffix(d.base.Path, "/") + storage.Clean(p)
	u.RawQuery = ""
	return u.String()
}

// vpath 把服务器返回的 href 还原成虚拟路径。
func (d *Driver) vpath(href string) string {
	if pu, err := url.Parse(href); err == nil && pu.Path != "" {
		href = pu.Path
	}
	if un, err := url.PathUnescape(href); err == nil {
		href = un
	}
	prefix := strings.TrimSuffix(d.base.Path, "/")
	p := strings.TrimPrefix(href, prefix)
	if p == "" {
		p = "/"
	}
	return storage.Clean(p)
}

func (d *Driver) do(ctx context.Context, method, p, depth string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.urlFor(p), rdr)
	if err != nil {
		return nil, err
	}
	if d.username != "" {
		req.SetBasicAuth(d.username, d.password)
	}
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, storage.ErrNotFound
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, fmt.Errorf("webdav: 认证失败 (HTTP %d)，请检查用户名/密码", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("webdav: %s %s 返回 HTTP %d: %s", method, p, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:displayname/>
    <d:getcontentlength/>
    <d:getlastmodified/>
    <d:resourcetype/>
  </d:prop>
</d:propfind>`

type multistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string     `xml:"href"`
	Propstat []propstat `xml:"propstat"`
}

type propstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	DisplayName   string `xml:"displayname"`
	ContentLength string `xml:"getcontentlength"`
	LastModified  string `xml:"getlastmodified"`
	ResourceType  struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

func (d *Driver) propfind(ctx context.Context, p, depth string) ([]davResponse, error) {
	resp, err := d.do(ctx, "PROPFIND", p, depth, []byte(propfindBody), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("webdav: PROPFIND 响应解析失败: %w", err)
	}
	return ms.Responses, nil
}

func (d *Driver) toEntry(r davResponse, self string) (storage.Entry, bool) {
	href := r.Href
	vp := d.vpath(href)
	if vp == self {
		return storage.Entry{}, false
	}
	var prop davProp
	for _, ps := range r.Propstat {
		if strings.Contains(ps.Status, "200") {
			prop = ps.Prop
			break
		}
	}
	e := storage.Entry{
		Name:  storage.Base(vp),
		Path:  vp,
		IsDir: prop.ResourceType.Collection != nil,
	}
	if prop.DisplayName != "" && e.Name == "" {
		e.Name = prop.DisplayName
	}
	if prop.ContentLength != "" {
		if n, err := strconv.ParseInt(prop.ContentLength, 10, 64); err == nil {
			e.Size = n
		}
	}
	if prop.LastModified != "" {
		if t, err := http.ParseTime(prop.LastModified); err == nil {
			e.ModTime = t
		}
	}
	return e, e.Name != ""
}

func (d *Driver) List(ctx context.Context, dir string) ([]storage.Entry, error) {
	dir = storage.Clean(dir)
	rs, err := d.propfind(ctx, dir, "1")
	if err != nil {
		return nil, err
	}
	out := make([]storage.Entry, 0, len(rs))
	for _, r := range rs {
		if e, ok := d.toEntry(r, dir); ok {
			out = append(out, e)
		}
	}
	storage.SortEntries(out)
	return out, nil
}

func (d *Driver) Stat(ctx context.Context, p string) (storage.Entry, error) {
	p = storage.Clean(p)
	if p == "/" {
		return storage.Entry{Name: "/", Path: "/", IsDir: true}, nil
	}
	rs, err := d.propfind(ctx, p, "0")
	if err != nil {
		return storage.Entry{}, err
	}
	for _, r := range rs {
		if e, ok := d.toEntry(r, "\x00"); ok {
			e.Path = p
			e.Name = storage.Base(p)
			return e, nil
		}
	}
	return storage.Entry{}, storage.ErrNotFound
}

func (d *Driver) Mkdir(ctx context.Context, p string) error {
	resp, err := d.do(ctx, "MKCOL", p, "", nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *Driver) Remove(ctx context.Context, p string, _ bool) error {
	// WebDAV 的 DELETE 对集合天然是递归的。
	resp, err := d.do(ctx, "DELETE", p, "", nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *Driver) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", d.urlFor(p), r)
	if err != nil {
		return err
	}
	if d.username != "" {
		req.SetBasicAuth(d.username, d.password)
	}
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webdav: PUT 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (d *Driver) Move(ctx context.Context, from, to string) error {
	resp, err := d.do(ctx, "MOVE", from, "", nil, map[string]string{
		"Destination": d.urlFor(to),
		"Overwrite":   "T",
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *Driver) Copy(ctx context.Context, from, to string) error {
	resp, err := d.do(ctx, "COPY", from, "", nil, map[string]string{
		"Destination": d.urlFor(to),
		"Overwrite":   "T",
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *Driver) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return d.OpenRange(ctx, p, 0)
}

func (d *Driver) OpenRange(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	hdr := map[string]string{}
	if offset > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-", offset)
	}
	resp, err := d.do(ctx, "GET", p, "", nil, hdr)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
