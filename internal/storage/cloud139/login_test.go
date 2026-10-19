package cloud139

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// calSignRef 是逐行照搬公开实现的参考版本，只用于差分测试。
func calSignRef(body, ts, randStr string) string {
	body = strings.ReplaceAll(body, "\n", "")
	body = strings.ReplaceAll(body, " ", "")
	body = strings.ReplaceAll(url.QueryEscape(body), "+", "%20")
	chars := strings.Split(body, "")
	sort.Strings(chars)
	b64 := base64.StdEncoding.EncodeToString([]byte(strings.Join(chars, "")))
	md5hex := func(s string) string {
		sum := md5.Sum([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	seed := md5hex(b64) + md5hex(ts+":"+randStr)
	return strings.ToUpper(md5hex(seed))
}

func TestCalSignMatchesReference(t *testing.T) {
	cases := []struct{ body, ts, rand string }{
		{`{"account":"13800000000"}`, "2026-10-19 15:00:00", "AbCdEfGh12345678"},
		{`{"a":1,"b":[1,2],"c":{"d":"中文"}}`, "2026-01-02 03:04:05", "zzzzzzzzzzzzzzzz"},
		{`{}`, "2026-10-19 15:00:00", "0000000000000000"},
		{`{"path":"/a b/c&d=e"}`, "2026-10-19 15:00:00", "x1y2z3x1y2z3x1y2"},
	}
	for _, c := range cases {
		got := calSign(c.body, c.ts, c.rand)
		want := calSignRef(c.body, c.ts, c.rand)
		if got != want {
			t.Fatalf("calSign 与参考实现不一致\nbody=%s\ngot =%s\nwant=%s", c.body, got, want)
		}
		if len(got) != 32 || got != strings.ToUpper(got) {
			t.Fatalf("签名格式不对: %q", got)
		}
	}
}

func TestSortedJSONKeepsHTMLChars(t *testing.T) {
	s, err := sortedJSON(map[string]any{"clientkey_decrypt": "l3TryM&Q+X7@dzwk)qP", "z": "1", "a": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "&") || strings.Contains(s, `\u0026`) {
		t.Fatalf("& 被转义了: %s", s)
	}
	if !strings.HasPrefix(s, `{"a":"2"`) {
		t.Fatalf("键没有按字典序排列: %s", s)
	}
}

func TestAuthorizationRoundTrip(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	raw := BuildAuthorization("pc", "13800000000", fmt.Sprintf("TOKEN123|1|2|%d", exp))

	info, err := ParseAuthorization(raw)
	if err != nil {
		t.Fatal(err)
	}
	if info.Account != "13800000000" || !strings.HasPrefix(info.Token, "TOKEN123") {
		t.Fatalf("解析结果不对: %+v", info)
	}
	if info.ExpiresAt.UnixMilli() != exp {
		t.Fatalf("过期时间解析错误: %v", info.ExpiresAt)
	}

	// 允许带 Basic 前缀
	if _, err := ParseAuthorization("Basic " + raw); err != nil {
		t.Fatalf("带 Basic 前缀时解析失败: %v", err)
	}
	if _, err := ParseAuthorization("not-base64!!"); err == nil {
		t.Fatal("非法输入应当报错")
	}
}

func TestAESRoundTrip(t *testing.T) {
	key1, _ := hex.DecodeString(keyHex1)
	key2, _ := hex.DecodeString(keyHex2)
	iv, _ := randomIV()

	ct, err := aesCBCEncrypt([]byte(`{"hello":"世界"}`), key1, iv)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := aesCBCDecrypt(ct, key1, iv)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != `{"hello":"世界"}` {
		t.Fatalf("CBC 往返结果不一致: %s", pt)
	}

	// ECB 用 key2 手工加密，再用被测函数解密
	block, _ := aes.NewCipher(key2)
	plain := pkcs7Pad([]byte(`{"authToken":"abc"}`), block.BlockSize())
	ecb := make([]byte, len(plain))
	for i := 0; i < len(plain); i += block.BlockSize() {
		block.Encrypt(ecb[i:i+block.BlockSize()], plain[i:i+block.BlockSize()])
	}
	got, err := aesECBDecrypt(ecb, key2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"authToken":"abc"}` {
		t.Fatalf("ECB 解密结果不一致: %s", got)
	}
}

// TestLoginPipeline 用假服务器把三步登录串起来，覆盖加解密与状态流转。
func TestLoginPipeline(t *testing.T) {
	key1, _ := hex.DecodeString(keyHex1)
	key2, _ := hex.DecodeString(keyHex2)

	var sawStep1Form, sawStep3Body bool

	// 第 1 步：返回 302，Location 带 sid，并下发 Cookie
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("UserName") == "13800000000" && r.Form.Get("Password") == sha1Hex("fetion.com.cn:pwd123") {
			sawStep1Form = true
		}
		http.SetCookie(w, &http.Cookie{Name: "Os_Sso_Sid", Value: "SID-ABC"})
		http.SetCookie(w, &http.Cookie{Name: "RMKEY", Value: "RM-1"})
		w.Header().Set("Location", "https://mail.10086.cn/portal/mailIndex.do?sid=SID-ABC")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv1.Close()

	// 第 2 步：返回 artifact
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "RMKEY=RM-1" {
			t.Errorf("第 2 步 Cookie 不对: %q", r.Header.Get("Cookie"))
		}
		_, _ = io.WriteString(w, `{"var":{"artifact":"DYCPWD-1"}}`)
	}))
	defer srv2.Close()

	// 第 3 步：解开客户端密文，再返回双层加密的 authToken
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		raw, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil || len(raw) < 16 {
			t.Errorf("第 3 步请求体不是合法密文")
			w.WriteHeader(400)
			return
		}
		plain, err := aesCBCDecrypt(raw[16:], key1, raw[:16])
		if err != nil {
			t.Errorf("第 3 步解密失败: %v", err)
			w.WriteHeader(400)
			return
		}
		var req map[string]any
		_ = json.Unmarshal(plain, &req)
		if req["dycpwd"] == "DYCPWD-1" && req["secinfo"] == strings.ToUpper(sha1Hex("fetion.com.cn:DYCPWD-1")) {
			sawStep3Body = true
		}

		inner := `{"authToken":"AUTH-TOKEN","account":"13800000000","userDomainId":"UD-9"}`
		block, _ := aes.NewCipher(key2)
		padded := pkcs7Pad([]byte(inner), block.BlockSize())
		ecb := make([]byte, len(padded))
		for i := 0; i < len(padded); i += block.BlockSize() {
			block.Encrypt(ecb[i:i+block.BlockSize()], padded[i:i+block.BlockSize()])
		}
		outer := fmt.Sprintf(`{"data":%q}`, hex.EncodeToString(ecb))

		iv, _ := randomIV()
		ct, _ := aesCBCEncrypt([]byte(outer), key1, iv)
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(append(iv, ct...))))
	}))
	defer srv3.Close()

	oldPwd, oldArt, oldSSO := mailPasswordURL, artifactURL, ssoLoginURL
	mailPasswordURL, artifactURL, ssoLoginURL = srv1.URL, srv2.URL, srv3.URL
	defer func() { mailPasswordURL, artifactURL, ssoLoginURL = oldPwd, oldArt, oldSSO }()

	auth, cookies, udid, err := LoginWithPassword(t.Context(), &Credentials{
		Account:  "13800000000",
		Password: "pwd123",
	})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !sawStep1Form {
		t.Error("第 1 步没有收到正确的表单（账号或密码哈希不对）")
	}
	if !sawStep3Body {
		t.Error("第 3 步请求体不符合预期（dycpwd/secinfo 不对）")
	}
	if cookies == "" || !strings.Contains(cookies, "RMKEY=RM-1") {
		t.Errorf("Cookie 没有合并回来: %q", cookies)
	}
	if udid != "UD-9" {
		t.Errorf("userDomainId 解析错误: %q", udid)
	}

	info, err := ParseAuthorization(auth)
	if err != nil {
		t.Fatal(err)
	}
	if info.Account != "13800000000" || info.Token != "AUTH-TOKEN" {
		t.Fatalf("最终 Authorization 不对: %+v", info)
	}
}

func TestRefreshKeepsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "<account>13800000000</account>") {
			t.Errorf("续期请求里没有账号: %s", body)
		}
		_, _ = io.WriteString(w, `<root><return>0</return><token>NEW-TOKEN|1|2|9999999999999</token></root>`)
	}))
	defer srv.Close()

	old := refreshURL
	refreshURL = srv.URL
	defer func() { refreshURL = old }()

	// 距过期 1 天 → 触发续期
	info := &AuthInfo{
		Raw:       BuildAuthorization("pc", "13800000000", "OLD|1|2|0"),
		Scheme:    "pc",
		Account:   "13800000000",
		Token:     "OLD|1|2|0",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	newAuth, err := RefreshAuthorization(t.Context(), info)
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	parsed, err := ParseAuthorization(newAuth)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Token != "NEW-TOKEN|1|2|9999999999999" || parsed.Account != "13800000000" {
		t.Fatalf("续期结果不对: %+v", parsed)
	}
}

// TestLiveStep1Shape 是可选的联网探测：只在 PANHUB_LIVE_NET=1 时运行，
// 用一个假账号走一遍第 1 步，用来确认端点/路径还没失效。
// 它只发一次请求，不做任何断言（结果打印到日志）。
func TestLiveStep1Shape(t *testing.T) {
	if os.Getenv("PANHUB_LIVE_NET") == "" {
		t.Skip("设置 PANHUB_LIVE_NET=1 才会联网")
	}
	start := time.Now()
	sid, cookies, err := step1Login(context.Background(), &Credentials{
		Account:  "13900000000",
		Password: "definitely-wrong-password",
	})
	t.Logf("耗时 %s", time.Since(start).Round(time.Millisecond))
	t.Logf("sid=%q", sid)
	t.Logf("cookies=%q", cookies)
	if err != nil {
		t.Logf("err=%v（假账号报错是预期的；关键看是否为网络/404 一类错误）", err)
	}
}

// TestLiveRoutePolicy 可选联网探测：用假授权调路由策略接口，
// 确认端点存在（返回业务错误而不是 404）。
func TestLiveRoutePolicy(t *testing.T) {
	if os.Getenv("PANHUB_LIVE_NET") == "" {
		t.Skip("设置 PANHUB_LIVE_NET=1 才会联网")
	}
	d := &Driver{
		auth:       &AuthInfo{Raw: BuildAuthorization("pc", "13800000000", "FAKE-TOKEN"), Account: "13800000000"},
		client:     &http.Client{Timeout: 20 * time.Second},
		clientType: "web",
	}
	host, err := d.queryRoutePolicy(context.Background())
	t.Logf("host=%q err=%v", host, err)
}

var _ = cipher.Block(nil)
