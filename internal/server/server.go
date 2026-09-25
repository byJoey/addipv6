// Package server 提供 Web 界面和 JSON API。
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/byJoey/addipv6/internal/cfapi"
	"github.com/byJoey/addipv6/internal/netif"
	"github.com/byJoey/addipv6/internal/store"
)

//go:embed web
var webFS embed.FS

// maxBody 限制请求体大小，批量操作的地址列表再长也用不到 4MB。
const maxBody = 4 << 20

// Options 是建 Server 需要的东西。
type Options struct {
	Manager  netif.Manager
	Store    *store.Store
	Password string
	Version  string
	Secure   bool
	Logger   *slog.Logger
}

// Server 把网卡管理、Cloudflare 客户端和 Web 界面接在一起。
type Server struct {
	mgr     netif.Manager
	store   *store.Store
	auth    *auth
	log     *slog.Logger
	version string
	mux     *http.ServeMux

	cfMu     sync.Mutex
	cfClient *cfapi.Client
	cfToken  string

	// opMu 让所有改网卡的操作串行，避免并发下发 netlink 打架。
	opMu sync.Mutex

	paths Paths
}

// New 组装一个 Server。
func New(opt Options) (*Server, error) {
	if opt.Manager == nil {
		return nil, errors.New("缺少网卡管理器")
	}
	if opt.Store == nil {
		return nil, errors.New("缺少配置存储")
	}
	if opt.Password == "" {
		return nil, errors.New("密码不能为空")
	}
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		mgr:     opt.Manager,
		store:   opt.Store,
		auth:    newAuth(opt.Password, opt.Secure),
		log:     log,
		version: opt.Version,
	}
	s.routes()
	return s, nil
}

// Handler 返回可以直接挂到 http.Server 上的处理器。
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.mux)
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("GET /", cacheStatic(fileServer))

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("GET /api/state", s.requireAuth(s.handleState))
	mux.HandleFunc("POST /api/addresses/preview", s.requireAuth(s.handlePreview))
	mux.HandleFunc("POST /api/addresses/add", s.requireAuth(s.handleAddAddrs))
	mux.HandleFunc("POST /api/addresses/delete", s.requireAuth(s.handleDeleteAddrs))
	mux.HandleFunc("POST /api/route/source", s.requireAuth(s.handleSetSource))
	mux.HandleFunc("POST /api/persist", s.requireAuth(s.handlePersist))
	mux.HandleFunc("POST /api/restore", s.requireAuth(s.handleRestore))

	mux.HandleFunc("POST /api/cf/token", s.requireAuth(s.handleCFToken))
	mux.HandleFunc("DELETE /api/cf/token", s.requireAuth(s.handleCFTokenDelete))
	mux.HandleFunc("GET /api/cf/zones", s.requireAuth(s.handleCFZones))
	mux.HandleFunc("POST /api/cf/zone", s.requireAuth(s.handleCFSelectZone))
	mux.HandleFunc("GET /api/cf/records", s.requireAuth(s.handleCFRecords))
	mux.HandleFunc("POST /api/cf/records/create", s.requireAuth(s.handleCFCreate))
	mux.HandleFunc("POST /api/cf/records/delete", s.requireAuth(s.handleCFDelete))

	s.mux = mux
}

// securityHeaders 挂上一组保守的安全响应头。CSP 不放行任何外部来源，
// 界面用到的脚本和样式全部内联在自带的静态文件里。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".css") || strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("写响应失败", "err", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorBody{Error: msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("请求格式不对: " + err.Error())
	}
	return nil
}

type loginRequest struct {
	Password string `json:"password"`
}

type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	CSRF          string `json:"csrf,omitempty"`
	Version       string `json:"version"`
	Platform      string `json:"platform"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait := s.auth.throttle(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "试得太频繁了，等 "+wait.Round(time.Second).String()+" 再来")
		return
	}
	var req loginRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.auth.checkPassword(req.Password) {
		s.auth.noteFailure(ip)
		s.log.Warn("登录失败", "ip", ip)
		writeError(w, http.StatusUnauthorized, "密码不对")
		return
	}
	s.auth.clearFailures(ip)
	token, csrf := s.auth.issue()
	s.auth.setCookie(w, token)
	s.log.Info("登录成功", "ip", ip)
	writeJSON(w, http.StatusOK, sessionResponse{
		Authenticated: true, CSRF: csrf, Version: s.version, Platform: s.mgr.Platform(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, _, ok := s.auth.sessionFrom(r); ok {
		s.auth.revoke(token)
	}
	s.auth.clearCookie(w)
	writeJSON(w, http.StatusOK, sessionResponse{Version: s.version, Platform: s.mgr.Platform()})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.auth.sessionFrom(r)
	resp := sessionResponse{Version: s.version, Platform: s.mgr.Platform()}
	if ok {
		resp.Authenticated = true
		resp.CSRF = sess.csrf
	}
	writeJSON(w, http.StatusOK, resp)
}
