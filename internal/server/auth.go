package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "addipv6_session"
	csrfHeader    = "X-CSRF-Token"
	sessionTTL    = 12 * time.Hour
)

type session struct {
	csrf    string
	expires time.Time
}

// auth 管登录态：会话表 + 登录失败限速。
type auth struct {
	mu         sync.Mutex
	passHash   [32]byte
	sessions   map[string]session
	failures   map[string]*failureRecord
	lastSweep  time.Time
	cookieSafe bool // 走 HTTPS 时给 cookie 加 Secure
}

type failureRecord struct {
	count  int
	until  time.Time
	lastAt time.Time
}

func newAuth(password string, secure bool) *auth {
	return &auth{
		passHash:   sha256.Sum256([]byte(password)),
		sessions:   map[string]session{},
		failures:   map[string]*failureRecord{},
		cookieSafe: secure,
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 读不到系统随机源属于致命问题，宁可崩也不能发个可预测的令牌出去。
		panic("读取随机数失败: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// clientIP 取对端地址，只用于限速，不做信任判断。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkPassword 用恒定时间比较，避免按字符逐位试出来。
func (a *auth) checkPassword(pw string) bool {
	got := sha256.Sum256([]byte(pw))
	return subtle.ConstantTimeCompare(got[:], a.passHash[:]) == 1
}

// throttle 返回这个 IP 还要等多久才能再试。
func (a *auth) throttle(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.failures[ip]
	if !ok {
		return 0
	}
	if d := time.Until(rec.until); d > 0 {
		return d
	}
	return 0
}

// noteFailure 记一次失败，连续失败会指数级拉长锁定时间。
func (a *auth) noteFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.failures[ip]
	if !ok || time.Since(rec.lastAt) > 15*time.Minute {
		rec = &failureRecord{}
		a.failures[ip] = rec
	}
	rec.count++
	rec.lastAt = time.Now()
	if rec.count >= 5 {
		// 第 5 次起开始锁，1s、2s、4s…… 最多 5 分钟。
		backoff := time.Duration(1<<min(rec.count-5, 8)) * time.Second
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		rec.until = time.Now().Add(backoff)
	}
}

func (a *auth) clearFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, ip)
}

// issue 建一个新会话，返回会话 token 和配套的 CSRF token。
func (a *auth) issue() (string, string) {
	token := randomToken(32)
	csrf := randomToken(32)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	a.sessions[token] = session{csrf: csrf, expires: time.Now().Add(sessionTTL)}
	return token, csrf
}

func (a *auth) revoke(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *auth) lookup(token string) (session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok {
		return session{}, false
	}
	if time.Now().After(s.expires) {
		delete(a.sessions, token)
		return session{}, false
	}
	return s, true
}

// sweepLocked 顺手清掉过期会话，调用方必须已经持锁。
func (a *auth) sweepLocked() {
	if time.Since(a.lastSweep) < time.Minute {
		return
	}
	a.lastSweep = time.Now()
	now := time.Now()
	for k, v := range a.sessions {
		if now.After(v.expires) {
			delete(a.sessions, k)
		}
	}
	for k, v := range a.failures {
		if now.Sub(v.lastAt) > 30*time.Minute {
			delete(a.failures, k)
		}
	}
}

func (a *auth) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSafe,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (a *auth) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSafe,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// sessionFrom 从请求里取出有效会话。
func (a *auth) sessionFrom(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", session{}, false
	}
	s, ok := a.lookup(c.Value)
	return c.Value, s, ok
}

// requireAuth 包一层登录校验；写请求还要带上匹配的 CSRF 头。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, sess, ok := s.auth.sessionFrom(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "没登录或登录已过期")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get(csrfHeader))), []byte(sess.csrf)) != 1 {
				writeError(w, http.StatusForbidden, "CSRF 校验没过，刷新页面重试")
				return
			}
		}
		next(w, r)
	}
}
