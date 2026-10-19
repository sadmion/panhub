package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenStore 是内存态会话表：重启即失效，够用且没有额外的存储依赖。
type tokenStore struct {
	mu       sync.Mutex
	password string
	ttl      time.Duration
	tokens   map[string]time.Time
}

func newTokenStore(password string) *tokenStore {
	return &tokenStore{
		password: password,
		ttl:      7 * 24 * time.Hour,
		tokens:   map[string]time.Time{},
	}
}

func (t *tokenStore) issue() (string, time.Time) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	exp := time.Now().Add(t.ttl)
	t.mu.Lock()
	t.tokens[tok] = exp
	t.mu.Unlock()
	return tok, exp
}

func (t *tokenStore) valid(tok string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(t.tokens, tok)
		return false
	}
	return true
}

func (t *tokenStore) revoke(tok string) {
	t.mu.Lock()
	delete(t.tokens, tok)
	t.mu.Unlock()
}

// checkPassword 用常量时间比较，避免时序侧信道。
func (t *tokenStore) checkPassword(p string) bool {
	return subtle.ConstantTimeCompare([]byte(p), []byte(t.password)) == 1
}

// bearer 从 Authorization 头取 token。
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// authRequired 保护 /api 下的接口。
func (s *Server) authRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if !s.tokens.valid(tok) {
			writeError(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// davAuth 让 WebDAV 同时支持 Basic（播放器/文件管理器）和 Bearer（浏览器）。
func (s *Server) davAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := bearer(r); tok != "" && s.tokens.valid(tok) {
			next.ServeHTTP(w, r)
			return
		}
		if u, p, ok := r.BasicAuth(); ok {
			if u == "admin" && s.tokens.checkPassword(p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="panhub"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

var errInvalidCredential = errors.New("密码错误")
