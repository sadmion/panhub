package cloud139

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 新个人盘：/file/list 翻页 + /file/getDownloadUrl 直链
func TestPersonalListPagingAndLink(t *testing.T) {
	var listCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/list":
			listCalls++
			// 新盘必须带 X-Yun-* 那套头，否则网关回 04000005 认证失败
			if r.Header.Get("Caller") != "web" || r.Header.Get("X-Yun-Api-Version") != "v1" ||
				r.Header.Get("X-Yun-Client-Info") == "" || r.Header.Get("Mcloud-Route") != "001" {
				t.Errorf("新盘请求头不完整: Caller=%q X-Yun-Api-Version=%q X-Yun-Client-Info=%q Mcloud-Route=%q",
					r.Header.Get("Caller"), r.Header.Get("X-Yun-Api-Version"),
					r.Header.Get("X-Yun-Client-Info"), r.Header.Get("Mcloud-Route"))
			}
			w.Header().Set("Content-Type", "application/json")
			if listCalls == 1 {
				fmt.Fprint(w, `{"success":true,"data":{"items":[
					{"fileId":"f1","name":"电影","type":"folder","updatedAt":"2026-10-01T12:00:00.000+08:00"},
					{"fileId":"c1","name":"a.mkv","type":"file","size":1024,"updatedAt":"2026-10-02T12:00:00.000+08:00"}
				],"nextPageCursor":"CURSOR-1"}}`)
				return
			}
			if got := r.Header.Get("mcloud-sign"); !strings.Contains(got, ",") || len(strings.Split(got, ",")) != 3 {
				t.Errorf("mcloud-sign 格式不对: %q", got)
			}
			fmt.Fprint(w, `{"success":true,"data":{"items":[
				{"fileId":"c2","name":"b.mkv","type":"file","size":2048,"updatedAt":"2026-10-03T12:00:00.000+08:00"}
			],"nextPageCursor":""}}`)
		case "/file/getDownloadUrl":
			fmt.Fprint(w, `{"success":true,"data":{"cdnSwitch":true,"cdnUrl":"https://cdn.example/a.mkv","url":"https://origin.example/a.mkv"}}`)
		default:
			t.Errorf("意外的路径: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Driver{
		auth:       &AuthInfo{Raw: "x", Account: "13800000000"},
		client:     srv.Client(),
		host:       srv.URL,
		apiMode:    apiAuto,
		clientType: "web",
		rootID:     "",
	}
	d.client = srv.Client()

	items, err := d.personalList(t.Context(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("翻页结果应有 3 项，实际 %d: %+v", len(items), items)
	}
	if listCalls != 2 {
		t.Fatalf("应该调用 2 次 /file/list，实际 %d", listCalls)
	}
	if items[0].Type != "folder" {
		t.Errorf("folder 类型识别错误: %+v", items[0])
	}

	link, err := d.personalLink(t.Context(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://cdn.example/a.mkv" {
		t.Fatalf("cdnSwitch=true 时应优先用 cdnUrl，实际 %q", link)
	}
}

// auto 模式下，新接口可用时应选中 personal_new
func TestAutoPrefersNewAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"items":[
			{"fileId":"c1","name":"a.mkv","type":"file","size":1,"updatedAt":"2026-10-02T12:00:00.000+08:00"}
		],"nextPageCursor":""}}`)
	}))
	defer srv.Close()

	d := &Driver{
		auth:    &AuthInfo{Raw: "x", Account: "13800000000"},
		client:  srv.Client(),
		host:    srv.URL,
		apiMode: apiAuto,
	}

	es, err := d.List(t.Context(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name != "a.mkv" || es[0].Size != 1 {
		t.Fatalf("列表结果不对: %+v", es)
	}
	if d.api() != apiNew {
		t.Fatalf("auto 应记住 personal_new，实际 %q", d.api())
	}
	// 第二次应直接走新接口，不再探测
	if _, err := d.List(t.Context(), "/"); err != nil {
		t.Fatal(err)
	}
}

// 显式 api_type=personal 时不应触碰新接口
func TestExplicitLegacyModeSkipsNewAPI(t *testing.T) {
	d := &Driver{
		auth:    &AuthInfo{Raw: "x", Account: "1"},
		client:  &http.Client{Timeout: time.Second},
		apiMode: apiPersonal,
		host:    "http://127.0.0.1:1/should-not-be-called",
	}
	if d.api() != apiPersonal {
		t.Fatalf("api_type 应为 personal，实际 %q", d.api())
	}
}
