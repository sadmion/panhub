package cloud139

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 139 的登录链路：邮箱 SSO（mail.10086.cn）→ 换 artifact → thirdlogin（双层 AES）。
// 这些地址与密钥都来自公开的逆向实现（AList / OpenList 的 139 驱动），
// 官方随时可能调整；失效时只改本文件的常量即可。
const (
	keyHex1       = "73634235495062495331515373756c734e7253306c673d3d" // 第一层 AES key（24 字节 ASCII）
	keyHex2       = "7150714477323633586746674c337538"                 // 第二层 AES-128 key
	mailPublicKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAnsOHTFtwW5rq/8gGhPlM5Z3RPdeN/d+FYIHb5JmcfBOCozXuT8c+0anvxtkjzghixwNlnmBuhN8OYfS789YuH/qReQbHC7OdlisLildNWPHRNUYcPa0W3lXSG3+81CXK7FDXPvXo5ubw2GqVbIsccMarI1dyfXdi4ITiCXvmM9wYBdUs9yXtoorhlpyYUI2GV8HNsQjWK9P5QZHT3ox5Qy+mjRmvv6RUFJLPOMkOS/pGZ0DwC1ypFZBxstW0/ftVupdOmGWvW7J2/e3dq3A/UvIkC4YUY/diL1wighJx1G9MiRROISjNMvNyUDSTqPJy516+l3sgHEbc067QIJx2NQIDAQAB"
)

// 端点改成变量：便于测试里替换成 httptest 服务器。
var (
	mailPasswordURL = "https://mail.10086.cn/Login/Login.ashx"
	mailSMSURL      = "https://mail.10086.cn/s"
	artifactURL     = "https://smsrebuild1.mail.10086.cn/setting/s"
	ssoLoginURL     = "https://user-njs.yun.139.com/user/thirdlogin"
	refreshURL      = "https://aas.caiyun.feixin.10086.cn:443/tellin/authTokenRefresh.do"
)

// Credentials 是登录所需的全部输入。
type Credentials struct {
	Account       string
	Password      string
	MailCookies   string
	SmsCode       string
	Authorization string
}

// AuthInfo 是解析后的 Authorization。
// 明文形如：pc:<account>:<token>|<a>|<b>|<过期毫秒时间戳>
type AuthInfo struct {
	Raw       string
	Scheme    string
	Account   string
	Token     string
	ExpiresAt time.Time
}

// ParseAuthorization 解析 Authorization（允许带 "Basic " 前缀）。
func ParseAuthorization(raw string) (*AuthInfo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("Authorization 为空")
	}
	if len(raw) > 6 && strings.EqualFold(raw[:6], "basic ") {
		raw = strings.TrimSpace(raw[6:])
	}
	// 也兼容直接粘贴整段 "Basic xxx"
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if d2, err2 := base64.RawStdEncoding.DecodeString(raw); err2 == nil {
			decoded = d2
		} else {
			return nil, fmt.Errorf("Authorization 不是合法 Base64: %w", err)
		}
	}
	text := string(decoded)
	parts := strings.SplitN(text, ":", 3)
	if len(parts) < 3 {
		return nil, errors.New("Authorization 结构异常（应为 前缀:账号:令牌）")
	}
	info := &AuthInfo{Raw: raw, Scheme: parts[0], Account: parts[1], Token: parts[2]}
	fields := strings.Split(parts[2], "|")
	if len(fields) >= 4 {
		if ms, err := strconv.ParseInt(fields[3], 10, 64); err == nil && ms > 0 {
			info.ExpiresAt = time.UnixMilli(ms)
		}
	}
	return info, nil
}

// BuildAuthorization 按 139 的格式拼回 Authorization。
func BuildAuthorization(scheme, account, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(scheme + ":" + account + ":" + token))
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlField(name, value string) string {
	return `<string name="` + escapeXML(name) + `">` + escapeXML(value) + `</string>`
}

// ---------- Cookie 小工具 ----------

func parseCookies(header string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func joinCookies(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 稳定输出，方便日志比对
	sortStrings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}

func mergeSetCookies(header string, resp *http.Response) string {
	m := parseCookies(header)
	for _, c := range resp.Cookies() {
		if c.Name != "" {
			m[c.Name] = c.Value
		}
	}
	return joinCookies(m)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------- 登录三步 ----------

// Step1Login 用账号密码登录邮箱 SSO，拿到 sid。
func step1Login(ctx context.Context, c *Credentials) (sid, cookies string, err error) {
	hashed := sha1Hex("fetion.com.cn:" + c.Password)
	cguid := strconv.FormatInt(time.Now().UnixMilli(), 10)

	form := url.Values{}
	form.Set("UserName", c.Account)
	form.Set("passOld", "")
	form.Set("auto", "on")
	form.Set("Password", hashed)
	form.Set("webIndexPagePwdLogin", "1")
	form.Set("pwdType", "1")
	form.Set("clientId", "1003")
	form.Set("authType", "2")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mailPasswordURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", c.MailCookies, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Origin", "https://mail.10086.cn")
	req.Header.Set("Referer", fmt.Sprintf(
		"https://mail.10086.cn/default.html?&s=1&v=0&u=%s&m=1&ec=S001&resource=indexLogin&clientid=1003&auto=on&cguid=%s&mtime=45",
		base64.StdEncoding.EncodeToString([]byte(c.Account)), cguid))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0")
	if c.MailCookies != "" {
		req.Header.Set("Cookie", c.MailCookies)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 需要读 Location 里的 sid
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", c.MailCookies, fmt.Errorf("第 1 步登录请求失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	cookies = mergeSetCookies(c.MailCookies, resp)

	// 风控：Location 里带 ec=XXXX
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, perr := url.Parse(loc); perr == nil {
			if code := u.Query().Get("ec"); code != "" {
				return "", cookies, fmt.Errorf(
					"第 1 步未通过（%s）：账号或密码不正确，或触发了邮箱风控。若确认密码无误，请改用「粘贴 Authorization」方式登录", code)
			}
			if s := u.Query().Get("sid"); s != "" {
				return s, cookies, nil
			}
		}
	}
	for _, name := range []string{"Os_Sso_Sid", "sid"} {
		if v := parseCookies(cookies)[name]; v != "" {
			return v, cookies, nil
		}
	}
	return "", cookies, errors.New("第 1 步成功但没拿到 sid（可能需要验证码或账号密码错误）")
}

// step2GetArtifact 用 sid 换 artifact（dycpwd）。
func step2GetArtifact(ctx context.Context, cookies string) (string, error) {
	rmkey := ""
	for k, v := range parseCookies(cookies) {
		if k == "RMKEY" {
			rmkey = v
		}
	}
	if rmkey == "" {
		return "", errors.New("Cookie 里缺少 RMKEY，无法换取 artifact")
	}
	cguid := strconv.FormatInt(time.Now().UnixMilli(), 10)
	u := fmt.Sprintf("%s?func=%s&sid=%s&cguid=%s",
		artifactURL,
		url.QueryEscape("umc:getArtifact"),
		url.QueryEscape(parseCookies(cookies)["Os_Sso_Sid"]),
		cguid)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(""))
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", "RMKEY="+rmkey)
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("User-Agent", "okhttp/4.12.0")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("第 2 步换取 artifact 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Var struct {
			Artifact string `json:"artifact"`
		} `json:"var"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Var.Artifact == "" {
		return "", fmt.Errorf("第 2 步响应里没有 artifact（HTTP %d）: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return out.Var.Artifact, nil
}

// step3ThirdLogin 用 artifact 做单点登录，双层 AES 解密后得到 authToken。
func step3ThirdLogin(ctx context.Context, account, dycpwd string) (auth string, userDomainID string, err error) {
	body := map[string]any{
		"clientkey_decrypt": "l3TryM&Q+X7@dzwk)qP",
		"clienttype":        "886",
		"cpid":              "507",
		"dycpwd":            dycpwd,
		"extInfo":           map[string]any{"ifOpenAccount": "0"},
		"loginMode":         "0",
		"msisdn":            account,
		"pintype":           "13",
		"secinfo":           strings.ToUpper(sha1Hex("fetion.com.cn:" + dycpwd)),
		"version":           "20250901",
	}
	plain, err := sortedJSON(body)
	if err != nil {
		return "", "", err
	}

	key1, err := hex.DecodeString(keyHex1)
	if err != nil {
		return "", "", fmt.Errorf("key1 解码失败: %w", err)
	}
	iv, err := randomIV()
	if err != nil {
		return "", "", err
	}
	ct, err := aesCBCEncrypt([]byte(plain), key1, iv)
	if err != nil {
		return "", "", fmt.Errorf("第 3 步请求加密失败: %w", err)
	}
	payload := base64.StdEncoding.EncodeToString(append(iv, ct...))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ssoLoginURL, strings.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	for k, v := range map[string]string{
		"hcy-cool-flag":       "1",
		"x-huawei-channelSrc": "10246600",
		"x-MM-Source":         "0",
		"x-UserAgent":         "android|23116PN5BC|android15|1.2.6|||1440x3200|10246600",
		"x-DeviceInfo":        "4|127.0.0.1|5|1.2.6|Xiaomi|23116PN5BC||02-00-00-00-00-00|android 15|1440x3200|android|||",
		"Content-Type":        "text/plain;charset=UTF-8",
		"User-Agent":          "okhttp/3.12.2",
	} {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("第 3 步单点登录失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("第 3 步返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	// 响应：Base64(IV + AES-CBC(外层 JSON))，外层 JSON 的 data 是 hex(AES-ECB(内层 JSON))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) < 16 {
		return "", "", fmt.Errorf("第 3 步响应无法解码: %s", truncate(string(raw), 200))
	}
	layer1, err := aesCBCDecrypt(decoded[16:], key1, decoded[:16])
	if err != nil {
		return "", "", fmt.Errorf("第 3 步外层解密失败: %w", err)
	}
	var outer struct {
		Data string `json:"data"`
		Code string `json:"code"`
		Msg  string `json:"message"`
	}
	if err := json.Unmarshal(layer1, &outer); err != nil || outer.Data == "" {
		return "", "", fmt.Errorf("第 3 步外层 JSON 异常: %s", truncate(string(layer1), 200))
	}
	key2, err := hex.DecodeString(keyHex2)
	if err != nil {
		return "", "", err
	}
	inner, err := hex.DecodeString(outer.Data)
	if err != nil {
		return "", "", fmt.Errorf("第 3 步内层不是合法 hex: %w", err)
	}
	plainInner, err := aesECBDecrypt(inner, key2)
	if err != nil {
		return "", "", fmt.Errorf("第 3 步内层解密失败: %w", err)
	}
	var final struct {
		AuthToken    string `json:"authToken"`
		Account      string `json:"account"`
		UserDomainID string `json:"userDomainId"`
	}
	if err := json.Unmarshal(plainInner, &final); err != nil {
		return "", "", fmt.Errorf("第 3 步内层 JSON 异常: %s", truncate(string(plainInner), 200))
	}
	if final.AuthToken == "" || final.Account == "" {
		return "", "", errors.New("第 3 步没有返回 authToken，可能需要短信验证")
	}
	return BuildAuthorization("pc", final.Account, final.AuthToken), final.UserDomainID, nil
}

// LoginWithPassword 完整走一遍账号密码登录。
func LoginWithPassword(ctx context.Context, c *Credentials) (auth, cookies, userDomainID string, err error) {
	if c.Account == "" || c.Password == "" {
		return "", "", "", errors.New("账号和密码都不能为空")
	}
	sid, ck, err := step1Login(ctx, c)
	if err != nil {
		return "", ck, "", err
	}
	_ = sid
	cookies = ck
	dycpwd, err := step2GetArtifact(ctx, cookies)
	if err != nil {
		return "", cookies, "", err
	}
	auth, userDomainID, err = step3ThirdLogin(ctx, c.Account, dycpwd)
	if err != nil {
		return "", cookies, "", err
	}
	return auth, cookies, userDomainID, nil
}

// LoginWithCookies 用浏览器 Cookie（含 Os_Sso_Sid + RMKEY）快速换授权。
func LoginWithCookies(ctx context.Context, c *Credentials) (auth, userDomainID string, err error) {
	if c.MailCookies == "" {
		return "", "", errors.New("Cookie 为空")
	}
	ck := parseCookies(c.MailCookies)
	if ck["Os_Sso_Sid"] == "" || ck["RMKEY"] == "" {
		return "", "", errors.New("Cookie 里需要同时包含 Os_Sso_Sid 和 RMKEY（登录 mail.10086.cn 后复制）")
	}
	dycpwd, err := step2GetArtifact(ctx, c.MailCookies)
	if err != nil {
		return "", "", err
	}
	account := c.Account
	if account == "" {
		account = ck["ud_id"]
	}
	return step3ThirdLogin(ctx, account, dycpwd)
}

// RefreshAuthorization 在过期前续期；失败时返回 ErrLoginRequired 语义的错误。
func RefreshAuthorization(ctx context.Context, info *AuthInfo) (string, error) {
	if info.ExpiresAt.IsZero() {
		return info.Raw, nil
	}
	if time.Until(info.ExpiresAt) > 15*24*time.Hour {
		return info.Raw, nil
	}
	if time.Now().After(info.ExpiresAt) {
		return "", errors.New("Authorization 已过期，请重新登录")
	}
	reqBody := "<root><token>" + escapeXML(info.Token) + "</token><account>" + escapeXML(info.Account) +
		"</account><clienttype>656</clienttype></root>"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("续期请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed struct {
		Return string `xml:"return"`
		Token  string `xml:"token"`
		Desc   string `xml:"desc"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil || parsed.Token == "" {
		var fallback struct {
			Return string `xml:"return"`
			Token  string `xml:"Token"`
		}
		_ = xml.Unmarshal(body, &fallback)
		if fallback.Token != "" {
			parsed.Token = fallback.Token
		}
	}
	if parsed.Token == "" {
		// 有些版本用大写标签，兜底再试一次正则
		if m := regexp.MustCompile(`(?i)<token>([^<]+)</token>`).FindStringSubmatch(string(body)); len(m) == 2 {
			parsed.Token = m[1]
		}
	}
	if parsed.Token == "" {
		return "", fmt.Errorf("续期失败（返回 %q）: %s", parsed.Desc, truncate(string(body), 200))
	}
	return BuildAuthorization(info.Scheme, info.Account, parsed.Token), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
