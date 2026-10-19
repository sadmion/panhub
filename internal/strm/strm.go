// Package strm 负责生成 .strm 文件（Emby / Jellyfin 的直链播放约定）。
package strm

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// mediaExt 是需要生成 STRM 的媒体扩展名。
var mediaExt = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".wmv": true,
	".flv": true, ".ts": true, ".m2ts": true, ".rmvb": true, ".webm": true,
	".mpg": true, ".mpeg": true, ".iso": true, ".m4v": true,
	".mp3": true, ".flac": true, ".wav": true, ".aac": true, ".m4a": true, ".ape": true,
}

// IsMedia 判断文件名是否是需要生成 STRM 的媒体。
// 会自动忽略已经生成过的 .strm 和下载字幕等附属文件。
// `.cas`（移动云盘秒传凭证）也算媒体——播放时会自动还原成真文件，
// 生成的 STRM 名字会去掉 .cas（电影.mkv.cas → 电影.strm）。
func IsMedia(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	if ext == ".cas" {
		// "电影.mkv.cas" → 去掉 .cas 后看真实后缀；"电影.cas"/"未知.xyz.cas"
		// 这类看不出后缀的也算媒体——真实片名在 .cas 内容里，驱动还原时解析。
		base := strings.TrimSuffix(name[:len(name)-4], ".")
		if base == "" {
			return false
		}
		return true
	}
	return mediaExt[ext]
}

// FileName 生成 STRM 的文件名。"电影.mkv" → "电影.strm"；
// ".cas" 会把两层后缀都去掉（"电影.mkv.cas" → "电影.strm"）。
func FileName(name string) string {
	if strings.EqualFold(path.Ext(name), ".cas") {
		base := strings.TrimSuffix(name[:len(name)-4], ".")
		if base == "" {
			base = "cas"
		}
		name = base
	}
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + ".strm"
}

// WriteIfChanged 在内容变化时才写盘，避免每次任务都刷新 mtime 让 Emby 重扫。
func WriteIfChanged(fullPath, content string) (changed bool, err error) {
	if old, err := os.ReadFile(fullPath); err == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return false, err
	}
	// 用临时文件 + rename，避免 Emby 读到半截链接。
	tmp := fullPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, fullPath); err != nil {
		return false, err
	}
	return true, nil
}

// LocalPath 把虚拟目录映射到本地目标目录：/movies/a/b.mkv → target/movies/a/b.strm
func LocalPath(target, virtualDir, name string) string {
	rel := strings.TrimPrefix(path.Join(virtualDir), "/")
	return filepath.Join(target, filepath.FromSlash(rel), FileName(name))
}

var _ = time.Now
