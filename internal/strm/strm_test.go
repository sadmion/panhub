package strm

import "testing"

func TestIsMedia(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"电影.mkv", true},
		{"a.MP4", true},
		{"readme.txt", false},
		{"subtitle.srt", false},
		{"海报.jpg", false},
		{"nfo.nfo", false},
		{"已生成.strm", false}, // 已有的 strm 不再处理
		// .cas 系列
		{"电影.mkv.cas", true},
		{"电影.MKV.CAS", true},
		{"剧集.mp4.cas", true},
		{"电影.cas", true},     // 没带原始后缀的也算
		{"未知.xyz.cas", true}, // 内容里可能带真实名字，交给驱动解析
		{"x.115.cas", true},
	}
	for _, c := range cases {
		if got := IsMedia(c.name); got != c.want {
			t.Errorf("IsMedia(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"电影.mkv", "电影.strm"},
		{"电影.mkv.cas", "电影.strm"}, // cas 还原后的片名
		{"电影.MKV.CAS", "电影.strm"},
		{"电影.cas", "电影.strm"},
		{"剧集.mp4.cas", "剧集.strm"},
	}
	for _, c := range cases {
		if got := FileName(c.in); got != c.want {
			t.Errorf("FileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
