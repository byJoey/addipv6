// Package cfapi 是一个够用的 Cloudflare DNS 客户端：
// 校验 Token、列 Zone、列 / 批量建 / 批量删 AAAA 记录。
// 只依赖标准库，自带限速和退避重试。
package cfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL 是 Cloudflare API v4 的入口。
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// RecordComment 打在本工具创建的记录上，批量删除时靠它认人。
const RecordComment = "addipv6"

// 认证方式。Cloudflare 有两套，请求头和校验端点都不一样。
const (
	// ModeToken 是 API Token，可以只授权某个 zone 的 DNS 编辑，推荐用这个。
	ModeToken = "token"
	// ModeGlobalKey 是账户级的 Global API Key，权限是全账户，能改账单能删站点。
	ModeGlobalKey = "globalkey"
)

// Credential 是连 Cloudflare 用的凭据。
type Credential struct {
	Mode  string `json:"mode"`
	Token string `json:"token,omitempty"`
	Email string `json:"email,omitempty"`
	Key   string `json:"key,omitempty"`
}

// Normalize 去掉首尾空白并把模式补全。
func (c Credential) Normalize() Credential {
	out := Credential{
		Mode:  strings.TrimSpace(c.Mode),
		Token: strings.TrimSpace(c.Token),
		// 邮箱统一转小写：Cloudflare 存的是小写，用户手打大写会连不上。
		Email: strings.ToLower(strings.TrimSpace(c.Email)),
		Key:   strings.TrimSpace(c.Key),
	}
	if out.Mode == "" {
		// 老配置里只有 token 字段，没记过 mode，按 Token 处理。
		if out.Key != "" && out.Token == "" {
			out.Mode = ModeGlobalKey
		} else {
			out.Mode = ModeToken
		}
	}
	return out
}

// Valid 判断凭据填全了没。
func (c Credential) Valid() bool {
	c = c.Normalize()
	if c.Mode == ModeGlobalKey {
		return c.Email != "" && c.Key != ""
	}
	return c.Token != ""
}

// Hint 给界面看的脱敏串，够认出是哪把，又泄不出去。
func (c Credential) Hint() string {
	c = c.Normalize()
	if c.Mode == ModeGlobalKey {
		return c.Email + " / " + mask(c.Key)
	}
	return mask(c.Token)
}

func mask(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 6) + s[len(s)-4:]
}

// Client 是一个 Cloudflare API 客户端，可并发使用。
type Client struct {
	BaseURL   string
	Cred      Credential
	HTTP      *http.Client
	UserAgent string

	limiter *limiter
}

// New 建一个用 API Token 的客户端。
func New(token string) *Client {
	return NewWithCredential(Credential{Mode: ModeToken, Token: token})
}

// NewGlobalKey 建一个用 Global API Key 的客户端。
func NewGlobalKey(email, key string) *Client {
	return NewWithCredential(Credential{Mode: ModeGlobalKey, Email: email, Key: key})
}

// NewWithCredential 按给定凭据建客户端。凭据不全时所有请求都会直接报错。
//
// 想把请求打到别处（本地 mock、企业代理）就设 ADDIPV6_CF_ENDPOINT，
// 平时不用管，留空就是官方地址。
func NewWithCredential(cred Credential) *Client {
	base := DefaultBaseURL
	if v := strings.TrimSpace(os.Getenv("ADDIPV6_CF_ENDPOINT")); v != "" {
		base = v
	}
	return &Client{
		BaseURL: base,
		Cred:    cred.Normalize(),
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
		UserAgent: "addipv6/1.0 (+https://github.com/byJoey/addipv6)",
		// Cloudflare 全局配额是 1200 次 / 5 分钟，留一半余量走 4 次/秒。
		limiter: newLimiter(4, time.Second),
	}
}

// authHeaders 按凭据类型挂上对应的认证头。
func (c *Client) authHeaders(req *http.Request) {
	if c.Cred.Mode == ModeGlobalKey {
		req.Header.Set("X-Auth-Email", c.Cred.Email)
		req.Header.Set("X-Auth-Key", c.Cred.Key)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.Cred.Token)
}

// APIError 是 Cloudflare 返回的业务错误。
type APIError struct {
	Status int
	Errors []ErrorDetail
	Raw    string
}

// ErrorDetail 是单条错误。
type ErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if len(e.Errors) == 0 {
		msg := strings.TrimSpace(e.Raw)
		if msg == "" {
			msg = http.StatusText(e.Status)
		}
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return fmt.Sprintf("Cloudflare 返回 HTTP %d: %s", e.Status, msg)
	}
	parts := make([]string, 0, len(e.Errors))
	for _, d := range e.Errors {
		parts = append(parts, fmt.Sprintf("%d %s", d.Code, d.Message))
	}
	return "Cloudflare: " + strings.Join(parts, "; ")
}

// Code 判断是否包含某个 Cloudflare 错误码。
func (e *APIError) Code(code int) bool {
	for _, d := range e.Errors {
		if d.Code == code {
			return true
		}
	}
	return false
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []ErrorDetail   `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		Count      int `json:"count"`
		TotalCount int `json:"total_count"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

// limiter 是个最简单的令牌桶，避免为了限速再引一个依赖。
type limiter struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	perSec   float64
	lastFill time.Time
}

func newLimiter(n int, per time.Duration) *limiter {
	rate := float64(n) / per.Seconds()
	return &limiter{tokens: float64(n), max: float64(n), perSec: rate, lastFill: time.Now()}
}

func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = math.Min(l.max, l.tokens+now.Sub(l.lastFill).Seconds()*l.perSec)
		l.lastFill = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - l.tokens) / l.perSec * float64(time.Second))
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(need):
		}
	}
}

// do 发一次请求并解包，遇到 429 / 5xx 会退避重试。
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) (*envelope, error) {
	if !c.Cred.Valid() {
		if c.Cred.Mode == ModeGlobalKey {
			return nil, errors.New("全局 Key 模式要同时填邮箱和 Key")
		}
		return nil, errors.New("还没填 Cloudflare API Token")
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			return nil, err
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, err
		}
		c.authHeaders(req)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.UserAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("请求 Cloudflare 失败: %w", err)
			if sleepErr := backoff(ctx, attempt, 0); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if sleepErr := backoff(ctx, attempt, 0); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = &APIError{Status: resp.StatusCode, Raw: string(raw)}
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			if sleepErr := backoff(ctx, attempt, retryAfter); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, &APIError{Status: resp.StatusCode, Raw: string(raw)}
		}
		if !env.Success {
			return nil, &APIError{Status: resp.StatusCode, Errors: env.Errors, Raw: string(raw)}
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return nil, fmt.Errorf("解析 Cloudflare 响应失败: %w", err)
			}
		}
		return &env, nil
	}
	if lastErr == nil {
		lastErr = errors.New("重试多次仍未成功")
	}
	return nil, lastErr
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func backoff(ctx context.Context, attempt int, hint time.Duration) error {
	d := hint
	if d <= 0 {
		d = time.Duration(1<<attempt) * 400 * time.Millisecond
		d += time.Duration(rand.Int63n(250)) * time.Millisecond
	}
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
