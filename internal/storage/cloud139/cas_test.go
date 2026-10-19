package cloud139

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseCASBlob(t *testing.T) {
	sha := strings.Repeat("a", 64)
	payload, _ := json.Marshal(map[string]any{
		"name": "电影.mkv", "size": 1234567, "sha256": sha, "provider": "139",
	})
	blob := base64.StdEncoding.EncodeToString(payload)

	info, err := parseCASBlob([]byte("  " + blob + "\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if info.Name != "电影.mkv" || info.Size != 1234567 || info.SHA256 != sha {
		t.Fatalf("字段不对: %+v", info)
	}

	// 明文 JSON 也兼容
	if _, err := parseCASBlob(payload); err != nil {
		t.Fatalf("明文 JSON 应可解析: %v", err)
	}
	// 缺 sha256 必须报错（139 只认 SHA256）
	bad, _ := json.Marshal(map[string]any{"name": "x.mkv", "size": 1})
	if _, err := parseCASBlob(bad); err == nil {
		t.Fatal("缺少 sha256 应当报错")
	}
	// 长度不对也要报错
	bad2, _ := json.Marshal(map[string]any{"name": "x.mkv", "size": 1, "sha256": "abc"})
	if _, err := parseCASBlob(bad2); err == nil {
		t.Fatal("sha256 长度不对应当报错")
	}
}

func TestBuildPartInfos(t *testing.T) {
	cases := []struct {
		size  int64
		parts int
		first int64
	}{
		{100, 1, 100},
		{200 << 20, 2, 100 << 20},
		{31 << 30, 62, 512 << 20}, // >30GB 改用 512MB 分片：31GB/512MB = 62 片
		{200 << 30, 100, 512 << 20},
	}
	for _, c := range cases {
		got := buildPartInfos(c.size)
		if len(got) != c.parts {
			t.Fatalf("size=%d 分片数应为 %d，实际 %d", c.size, c.parts, len(got))
		}
		if got[0]["partSize"] != c.first {
			t.Fatalf("size=%d 首片大小应为 %d，实际 %v", c.size, c.first, got[0]["partSize"])
		}
	}
}

// TestRestoreCASFlow 覆盖：读 .cas → 建/找临时目录 → PC 头秒传 → 取直链 → 缓存复用
func TestRestoreCASFlow(t *testing.T) {
	sha := strings.Repeat("b", 64)
	blob := base64.StdEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"name":"电影.mkv","size":%d,"sha256":"%s"}`, 4096, sha)))

	var creates int32
	var sawPCHeaders bool
	var capturedCreate map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/file/list":
			fmt.Fprint(w, `{"success":true,"data":{"items":[
				{"fileId":"d-temp","name":"PANHUB_CAS_TEMP","type":"folder"},
				{"fileId":"c1","name":"电影.mkv.cas","type":"file","size":400}
			],"nextPageCursor":""}}`)

		case "/cas.cas":
			fmt.Fprint(w, blob)

		case "/file/getDownloadUrl":
			var req struct {
				FileID string `json:"fileId"`
			}
			_ = json.Unmarshal(body, &req)
			if req.FileID == "c1" {
				fmt.Fprintf(w, `{"success":true,"data":{"url":%q}}`, "http://"+r.Host+"/cas.cas")
				return
			}
			fmt.Fprint(w, `{"success":true,"data":{"cdnSwitch":true,"cdnUrl":"https://cdn.example/temp.mkv"}}`)

		case "/file/create":
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if payload["type"] == "folder" {
				fmt.Fprint(w, `{"success":true,"data":{"fileId":"d-new"}}`)
				return
			}
			atomic.AddInt32(&creates, 1)
			capturedCreate = payload
			// 秒传必须带 PC 客户端标识
			if r.Header.Get("x-MM-Source") == "000" &&
				strings.Contains(r.Header.Get("x-ExpRoute-Code"), "routeCode=13800000000") &&
				r.Header.Get("x-yun-device-id") != "" &&
				r.Header.Get("x-yun-op-type") == "1" {
				sawPCHeaders = true
			}
			fmt.Fprint(w, `{"success":true,"data":{"fileId":"f1","exist":true}}`)

		case "/recyclebin/batchTrash":
			fmt.Fprint(w, `{"success":true}`)

		default:
			t.Errorf("意外请求: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Driver{
		auth:           &AuthInfo{Raw: "x", Account: "13800000000"},
		client:         srv.Client(),
		host:           srv.URL,
		apiMode:        apiNew,
		casEnabled:     true,
		casTTL:         5 * time.Minute,
		casTempDirName: casTempDirDefault,
		casSessions:    map[string]*casSession{},
	}

	url, err := d.Link(t.Context(), "/电影.mkv.cas")
	if err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if url != "https://cdn.example/temp.mkv" {
		t.Fatalf("直链不对: %q", url)
	}
	if !sawPCHeaders {
		t.Fatal("秒传请求没有带 PC 客户端头（会被判拒绝秒传）")
	}
	if capturedCreate["contentHash"] != sha || capturedCreate["contentHashAlgorithm"] != "SHA256" {
		t.Fatalf("秒传请求体不对: %+v", capturedCreate)
	}
	if capturedCreate["size"] != float64(4096) || capturedCreate["type"] != "file" {
		t.Fatalf("size/type 不对: %+v", capturedCreate)
	}
	name, _ := capturedCreate["name"].(string)
	if !strings.HasPrefix(name, "TEMP_") || !strings.HasSuffix(name, "电影.mkv") {
		t.Fatalf("临时文件名不符合约定: %q", name)
	}
	parts, _ := capturedCreate["partInfos"].([]any)
	if len(parts) != 1 {
		t.Fatalf("partInfos 数量不对: %v", parts)
	}

	// TTL 内再次播放应命中缓存，不再产生新的秒传
	if _, err := d.Link(t.Context(), "/电影.mkv.cas"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&creates); n != 1 {
		t.Fatalf("缓存没生效：秒传被调用了 %d 次", n)
	}

	// 过期后应清理临时文件
	d.casMu.Lock()
	for k, s := range d.casSessions {
		s.Expires = time.Now().Add(-time.Minute)
		_ = k
	}
	d.casMu.Unlock()
	if _, err := d.Link(t.Context(), "/电影.mkv.cas"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&creates); n != 2 {
		t.Fatalf("过期后应重新秒传，实际调用 %d 次", n)
	}
}

func TestCASDisabledFallsBackToRawLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/file/list":
			fmt.Fprint(w, `{"success":true,"data":{"items":[
				{"fileId":"c1","name":"a.cas","type":"file","size":10}
			],"nextPageCursor":""}}`)
		case "/file/getDownloadUrl":
			var req struct {
				FileID string `json:"fileId"`
			}
			_ = json.Unmarshal(body, &req)
			fmt.Fprint(w, `{"success":true,"data":{"url":"https://cdn.example/a.cas"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Driver{
		auth:        &AuthInfo{Raw: "x", Account: "1"},
		client:      srv.Client(),
		host:        srv.URL,
		apiMode:     apiNew,
		casEnabled:  false,
		casSessions: map[string]*casSession{},
	}
	u, err := d.Link(t.Context(), "/a.cas")
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://cdn.example/a.cas" {
		t.Fatalf("关闭 CAS 后应直接返回原文件直链，实际 %q", u)
	}
}
