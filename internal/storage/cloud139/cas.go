package cloud139

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"panhub/internal/storage"
)

// .cas 是移动云盘的「秒传凭证」：一个 Base64(JSON) 小文件，里面带原始文件的
// sha256 与大小。播放时不需要真的上传，只要调 /file/create 把 sha256 报上去，
// 云端发现源数据还在就直接实例化出一个真文件（秒传），再取它的直链。
//
// 关键点（逆向自 139strm 的实践）：
//   - /file/create 必须伪装成 PC 客户端，否则会被判「拒绝秒传」
//   - 秒传出来的文件是真的占网盘空间的，要放在单独的临时目录并定期清理
//   - 同一个 .cas 不要重复还原，缓存直链；过期后再重建

const (
	casTempDirDefault    = "PANHUB_CAS_TEMP"
	casTTLDefault        = 5 * time.Minute
	casParseTTL          = 6 * time.Hour
	casTempCheckInterval = 30 * time.Minute

	pcAppVersion = "8.7.2.20260519"
	pcAppChannel = "10200153"
	pcDeviceTag  = "QkYtMjAyMDAzMTAxNjQ3"
)

// casInfo 是 .cas 里描述的原文件信息。
type casInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	MD5      string `json:"md5"`
	SHA1     string `json:"sha1"`
	Provider string `json:"provider"`
}

// casSession 记录一次还原结果，TTL 内复用，避免每次播放都在网盘里建文件。
type casSession struct {
	TempID  string
	URL     string
	Expires time.Time
}

// parseCASBlob 解析 .cas 内容：Base64 → JSON。
func parseCASBlob(raw []byte) (*casInfo, error) {
	s := strings.TrimSpace(string(raw))
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// 少数工具直接写明文 JSON，兼容一下
		decoded = []byte(s)
	}
	var info casInfo
	if err := json.Unmarshal(decoded, &info); err != nil {
		return nil, fmt.Errorf("cloud139: .cas 内容不是合法 JSON: %w", err)
	}
	if info.Name == "" || info.Size <= 0 {
		return nil, errors.New("cloud139: .cas 缺少 name / size")
	}
	if info.SHA256 == "" {
		return nil, errors.New("cloud139: .cas 缺少 sha256（139 秒传只认 SHA256）")
	}
	if len(info.SHA256) != 64 {
		return nil, fmt.Errorf("cloud139: .cas 的 sha256 长度不对（%d）", len(info.SHA256))
	}
	return &info, nil
}

func isCASName(name string) bool {
	return strings.EqualFold(path.Ext(name), ".cas")
}

// buildPartInfos 生成秒传用的分片描述：>30GB 用 512MB 一片，否则 100MB。
// 只发前 100 片就够了（够大文件也够判断命中）。
func buildPartInfos(size int64) []map[string]any {
	partSize := int64(100 << 20)
	if size>>30 > 30 {
		partSize = 512 << 20
	}
	n := int64(1)
	if size > partSize {
		n = (size + partSize - 1) / partSize
	}
	limit := n
	if limit > 100 {
		limit = 100
	}
	out := make([]map[string]any, 0, limit)
	for i := int64(0); i < limit; i++ {
		l := partSize
		if size-i*partSize < l {
			l = size - i*partSize
		}
		out = append(out, map[string]any{
			"partNumber":      i + 1,
			"partSize":        l,
			"parallelHashCtx": map[string]any{"partOffset": i * partSize},
		})
	}
	return out
}

// pcDeviceID 生成稳定的 PC 设备标识（同一账号每次一致，避免被判新设备）。
func (d *Driver) pcDeviceID() string {
	sum := md5.Sum([]byte(d.auth.Account))
	return "PANHUB" + strings.ToUpper(hex.EncodeToString(sum[:]))[:16] + "-PC"
}

// pcDeviceInfo 是 PC 客户端的设备串，秒传请求必须带这一份。
func (d *Driver) pcDeviceInfo() string {
	return fmt.Sprintf("||11|%s|PC|%s|%s|| Windows 10 (10.0)|1920X1040|Q2hpbmVzZSAoU2ltcGxpZmllZCk=|||",
		pcAppVersion, pcDeviceTag, d.pcDeviceID())
}

// casEnabledNow 判断当前挂载是否开启 .cas 还原。
func (d *Driver) casEnabledNow() bool { return d.casEnabled }

// ensureCASTempDir 取得（必要时创建）云盘根目录下的临时目录。
// 目录可能被用户删掉，所以按间隔重新确认；创建失败则报错。
func (d *Driver) ensureCASTempDir(ctx context.Context) (string, error) {
	d.casMu.Lock()
	if d.casTempDirID != "" && time.Since(d.casTempCheckedAt) < casTempCheckInterval {
		id := d.casTempDirID
		d.casMu.Unlock()
		return id, nil
	}
	d.casMu.Unlock()

	items, err := d.personalList(ctx, d.personalRoot())
	if err != nil {
		// 只是查不到就沿用旧 ID（目录多半还在）
		d.casMu.Lock()
		id := d.casTempDirID
		d.casMu.Unlock()
		if id != "" {
			return id, nil
		}
		return "", fmt.Errorf("cloud139: 列出根目录失败: %w", err)
	}
	var found string
	for _, it := range items {
		if it.Type == "folder" && it.Name == d.casTempDirName {
			found = it.FileID
			break
		}
	}
	if found == "" {
		payload := map[string]any{
			"parentFileId":   d.personalRoot(),
			"name":           d.casTempDirName,
			"description":    "",
			"type":           "folder",
			"fileRenameMode": "force_rename",
		}
		raw, err := d.personalPostWith(ctx, "/file/create", payload, profilePersonalPC)
		if err != nil {
			return "", fmt.Errorf("cloud139: 创建临时目录失败: %w", err)
		}
		var r struct {
			Data struct {
				FileID string `json:"fileId"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		found = r.Data.FileID
		if found == "" {
			return "", errors.New("cloud139: 创建临时目录后没有拿到 ID")
		}
	}

	d.casMu.Lock()
	d.casTempDirID = found
	d.casTempCheckedAt = time.Now()
	d.casMu.Unlock()
	return found, nil
}

// createCASFile 用 SHA256 秒传在临时目录里实例化出真文件，返回 fileId。
func (d *Driver) createCASFile(ctx context.Context, dirID, name string, info *casInfo) (string, error) {
	payload := map[string]any{
		"contentHash":          info.SHA256,
		"contentHashAlgorithm": "SHA256",
		"contentType":          "application/octet-stream",
		"parallelUpload":       false,
		"partInfos":            buildPartInfos(info.Size),
		"size":                 info.Size,
		"parentFileId":         dirID,
		"name":                 name,
		"type":                 "file",
		"fileRenameMode":       "auto_rename",
	}
	raw, err := d.personalPostWith(ctx, "/file/create", payload, profilePersonalPC)
	if err != nil {
		return "", err
	}
	var r struct {
		Data struct {
			FileID      string `json:"fileId"`
			Exist       bool   `json:"exist"`
			RapidUpload bool   `json:"rapidUpload"`
			PartInfos   []any  `json:"partInfos"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if r.Data.FileID == "" {
		if !r.Data.Exist && !r.Data.RapidUpload && r.Data.PartInfos != nil {
			return "", errors.New("cloud139: 秒传失败，云端已没有该文件的源数据（.cas 已失效）")
		}
		return "", errors.New("cloud139: 秒传没有返回 fileId")
	}
	return r.Data.FileID, nil
}

// restoreCAS 把 .cas 还原成临时目录里的真文件，返回直链。
func (d *Driver) restoreCAS(ctx context.Context, casPath string) (string, error) {
	e, err := d.Stat(ctx, casPath)
	if err != nil {
		return "", err
	}
	if e.ID == "" {
		return "", storage.ErrNotFound
	}

	// 1) 命中缓存
	d.casMu.Lock()
	if s, ok := d.casSessions[casPath]; ok && time.Now().Before(s.Expires) {
		url := s.URL
		d.casMu.Unlock()
		return url, nil
	}
	d.casMu.Unlock()

	// 2) 取 .cas 内容
	blobURL, err := d.linkByID(ctx, e.ID)
	if err != nil {
		return "", fmt.Errorf("cloud139: 取 .cas 直链失败: %w", err)
	}
	blob, err := d.fetchSmall(ctx, blobURL)
	if err != nil {
		return "", fmt.Errorf("cloud139: 下载 .cas 失败: %w", err)
	}
	info, err := parseCASBlob(blob)
	if err != nil {
		return "", err
	}

	// 3) 临时目录 + 秒传
	dirID, err := d.ensureCASTempDir(ctx)
	if err != nil {
		return "", err
	}
	restoreName := casRestoreName(path.Base(casPath), info.Name)
	tempName := fmt.Sprintf("TEMP_%d_%05d_%s", time.Now().UnixMilli(), time.Now().Nanosecond()%100000, restoreName)
	tempID, err := d.createCASFile(ctx, dirID, tempName, info)
	if err != nil {
		// 临时目录可能被删了，重查一次再试
		d.casMu.Lock()
		d.casTempDirID = ""
		d.casTempCheckedAt = time.Time{}
		d.casMu.Unlock()
		if dirID, err2 := d.ensureCASTempDir(ctx); err2 == nil {
			tempID, err = d.createCASFile(ctx, dirID, tempName, info)
		}
		if err != nil {
			return "", err
		}
	}

	// 4) 取临时文件直链
	url, err := d.linkByID(ctx, tempID)
	if err != nil {
		d.deleteTempFile(ctx, tempID)
		return "", fmt.Errorf("cloud139: 还原成功但取不到直链: %w", err)
	}

	d.casMu.Lock()
	if d.casSessions == nil {
		d.casSessions = map[string]*casSession{}
	}
	d.casSessions[casPath] = &casSession{TempID: tempID, URL: url, Expires: time.Now().Add(d.casTTL)}
	expired := make([]string, 0)
	for k, s := range d.casSessions {
		if time.Now().After(s.Expires) {
			expired = append(expired, s.TempID)
			delete(d.casSessions, k)
		}
	}
	d.casMu.Unlock()

	// 5) 顺手清理过期的临时文件
	for _, id := range expired {
		d.deleteTempFile(ctx, id)
	}
	return url, nil
}

// casRestoreName 还原出真实文件名：优先用 .cas 里的 name，其次用 .cas 自己的名字。
func casRestoreName(casName, originalName string) string {
	if originalName != "" {
		return originalName
	}
	base := strings.TrimSuffix(casName, path.Ext(casName))
	for strings.EqualFold(path.Ext(base), ".cas") {
		base = strings.TrimSuffix(base, path.Ext(base))
	}
	if base == "" {
		return "restored" + ".mp4"
	}
	return base
}

// fetchSmall 下载小文件（.cas 只有几百字节）。
func (d *Driver) fetchSmall(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// deleteTempFile 把还原出来的临时文件移进回收站（失败只记日志，不影响播放）。
func (d *Driver) deleteTempFile(ctx context.Context, fileID string) {
	if fileID == "" {
		return
	}
	_, _ = d.personalPost(ctx, "/recyclebin/batchTrash", map[string]any{"fileIds": []string{fileID}})
}

// openCAS 以流方式打开还原后的文件，支持 Range（播放器拖进度）。
func (d *Driver) openCAS(ctx context.Context, casPath string, offset int64) (io.ReadCloser, error) {
	url, err := d.restoreCAS(ctx, casPath)
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
		return nil, fmt.Errorf("cloud139: 取还原文件失败 HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
