package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/byJoey/addipv6/internal/cfapi"
	"github.com/byJoey/addipv6/internal/netif"
	"github.com/byJoey/addipv6/internal/store"
)

// fakeManager 是一块假网卡，让 handler 测试不依赖真实内核。
type fakeManager struct {
	mu           sync.Mutex
	addrs        []netif.Address
	route        *netif.Route
	fail         error
	p2p          bool
	hijackSource bool
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		addrs: []netif.Address{
			{Addr: netip.MustParseAddr("2001:db8::2"), PrefixLen: 64, Scope: "global", Permanent: true},
		},
		route: &netif.Route{
			Gateway: netip.MustParseAddr("2001:db8::1"),
			Source:  netip.MustParseAddr("2001:db8::2"),
			Dev:     "eth0", OnLink: true,
		},
	}
}

func (f *fakeManager) Platform() string { return "linux" }

func (f *fakeManager) List() ([]netif.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addrs := append([]netif.Address(nil), f.addrs...)
	return []netif.Interface{{
		Name: "eth0", Index: 2, Up: true, Addresses: addrs, PointToPoint: f.p2p,
		Prefixes: netif.CollectPrefixes(addrs), Route: f.route,
	}}, nil
}

func (f *fakeManager) AddAddr(_ string, p netip.Prefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	// 模拟内核 RFC 6724 的源地址选择：新地址进来可能把出口顶掉。
	if f.hijackSource && f.route != nil {
		f.route.Source = p.Addr()
	}
	for _, a := range f.addrs {
		if a.Addr == p.Addr() {
			return fmt.Errorf("%s 已经在了: %w", p, netif.ErrAddrExists)
		}
	}
	f.addrs = append(f.addrs, netif.Address{
		Addr: p.Addr(), PrefixLen: p.Bits(), Scope: "global", Permanent: true,
	})
	return nil
}

func (f *fakeManager) DelAddr(_ string, p netip.Prefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.addrs[:0]
	for _, a := range f.addrs {
		if a.Addr == p.Addr() {
			continue
		}
		out = append(out, a)
	}
	f.addrs = out
	return nil
}

func (f *fakeManager) SetDefaultRoute(_ string, gw, src netip.Addr, onLink bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.route = &netif.Route{Gateway: gw, Source: src, Dev: "eth0", OnLink: onLink}
	return nil
}

func newTestServer(t *testing.T) (*Server, *fakeManager) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir+"/conf", dir+"/state")
	if err != nil {
		t.Fatal(err)
	}
	mgr := newFakeManager()
	srv, err := New(Options{Manager: mgr, Store: st, Password: "hunter2", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, mgr
}

type client struct {
	t      *testing.T
	srv    http.Handler
	cookie string
	csrf   string
}

func (c *client) do(method, path, body string) (*http.Response, string) {
	c.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	if c.csrf != "" {
		req.Header.Set(csrfHeader, c.csrf)
	}
	w := httptest.NewRecorder()
	c.srv.ServeHTTP(w, req)
	res := w.Result()
	out, _ := io.ReadAll(res.Body)
	return res, string(out)
}

func (c *client) login(pw string) *http.Response {
	c.t.Helper()
	res, body := c.do("POST", "/api/login", `{"password":"`+pw+`"}`)
	if res.StatusCode == 200 {
		if ck := res.Header.Get("Set-Cookie"); ck != "" {
			c.cookie = strings.Split(ck, ";")[0]
		}
		var sr sessionResponse
		json.Unmarshal([]byte(body), &sr)
		c.csrf = sr.CSRF
	}
	return res
}

func TestUnauthenticatedBlocked(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	for _, p := range []string{"/api/state", "/api/cf/zones"} {
		res, _ := c.do("GET", p, "")
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s 该返回 401，拿到 %d", p, res.StatusCode)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}

	if res := c.login("wrong"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错密码该 401，拿到 %d", res.StatusCode)
	}
	if res := c.login("hunter2"); res.StatusCode != http.StatusOK {
		t.Fatalf("对密码该 200，拿到 %d", res.StatusCode)
	}
	if c.csrf == "" || c.cookie == "" {
		t.Fatal("该发回 cookie 和 csrf")
	}
	res, body := c.do("GET", "/api/state", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("登录后读状态该 200，拿到 %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"name":"eth0"`) {
		t.Fatalf("状态里该有 eth0: %s", body)
	}

	c.do("POST", "/api/logout", "")
	if res, _ := c.do("GET", "/api/state", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后该 401，拿到 %d", res.StatusCode)
	}
}

func TestCSRFRequiredOnWrites(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	good := c.csrf
	c.csrf = ""
	if res, _ := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":1}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("没带 CSRF 该 403，拿到 %d", res.StatusCode)
	}
	c.csrf = "deadbeef"
	if res, _ := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":1}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF 不对该 403，拿到 %d", res.StatusCode)
	}
	c.csrf = good
	if res, _ := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":1}`); res.StatusCode != http.StatusOK {
		t.Fatalf("CSRF 正确该放行，拿到 %d", res.StatusCode)
	}
}

func TestLoginThrottle(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	var got429 bool
	for i := 0; i < 8; i++ {
		res := c.login("nope")
		if res.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("连续猜密码应该被限速")
	}
}

func TestPreviewAndAdd(t *testing.T) {
	srv, mgr := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	res, body := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":5}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("预览失败 %d: %s", res.StatusCode, body)
	}
	var pv previewResponse
	json.Unmarshal([]byte(body), &pv)
	if len(pv.Addrs) != 5 {
		t.Fatalf("该生成 5 个，拿到 %d", len(pv.Addrs))
	}
	for _, a := range pv.Addrs {
		if !strings.HasSuffix(a, "/64") {
			t.Fatalf("前缀不对: %s", a)
		}
	}

	payload, _ := json.Marshal(map[string]any{"iface": "eth0", "addrs": pv.Addrs})
	res, body = c.do("POST", "/api/addresses/add", string(payload))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("添加失败 %d: %s", res.StatusCode, body)
	}
	var op opResult
	json.Unmarshal([]byte(body), &op)
	if op.Succeed != 5 || op.Failed != 0 {
		t.Fatalf("该全部成功，拿到 %+v", op)
	}
	if len(mgr.addrs) != 6 {
		t.Fatalf("假网卡上该有 6 个，实际 %d", len(mgr.addrs))
	}

	// 状态里必须把这 5 个标成本工具添加的。
	_, body = c.do("GET", "/api/state", "")
	if n := strings.Count(body, `"managed":true`); n != 5 {
		t.Fatalf("该标记 5 个，实际 %d: %s", n, body)
	}
}

func TestDeleteGuards(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	_, body := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":2}`)
	var pv previewResponse
	json.Unmarshal([]byte(body), &pv)
	payload, _ := json.Marshal(map[string]any{"iface": "eth0", "addrs": pv.Addrs})
	c.do("POST", "/api/addresses/add", string(payload))

	// 一口气把所有全局地址删光要被拦住。
	all := append([]string{"2001:db8::2/64"}, pv.Addrs...)
	payload, _ = json.Marshal(map[string]any{"iface": "eth0", "addrs": all})
	res, _ := c.do("POST", "/api/addresses/delete", string(payload))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("全删该 409，拿到 %d", res.StatusCode)
	}

	// 当前出口地址单独删也要拦。
	payload, _ = json.Marshal(map[string]any{"iface": "eth0", "addrs": []string{"2001:db8::2/64"}})
	_, body = c.do("POST", "/api/addresses/delete", string(payload))
	if !strings.Contains(body, "默认出口地址") {
		t.Fatalf("该拦住删出口地址: %s", body)
	}

	// 勾了强制就放行。
	payload, _ = json.Marshal(map[string]any{"iface": "eth0", "addrs": []string{"2001:db8::2/64"}, "force": true})
	_, body = c.do("POST", "/api/addresses/delete", string(payload))
	var op opResult
	json.Unmarshal([]byte(body), &op)
	if op.Succeed != 1 {
		t.Fatalf("强制该删掉，拿到 %+v", op)
	}
}

func TestSetSourceRejectsForeignAddr(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")
	_, body := c.do("POST", "/api/route/source", `{"iface":"eth0","source":"2001:db8::ffff"}`)
	if !strings.Contains(body, "不在 eth0 上") {
		t.Fatalf("该拒绝不在网卡上的地址: %s", body)
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	srv, mgr := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	_, body := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":3}`)
	var pv previewResponse
	json.Unmarshal([]byte(body), &pv)
	payload, _ := json.Marshal(map[string]any{"iface": "eth0", "addrs": pv.Addrs})
	c.do("POST", "/api/addresses/add", string(payload))

	before := len(mgr.addrs)
	for i := 0; i < 3; i++ {
		_, body = c.do("POST", "/api/restore", "{}")
		var op opResult
		json.Unmarshal([]byte(body), &op)
		if op.Failed != 0 {
			t.Fatalf("第 %d 次恢复有失败项: %s", i+1, body)
		}
	}
	if len(mgr.addrs) != before {
		t.Fatalf("反复恢复不该把地址越加越多: %d -> %d", before, len(mgr.addrs))
	}
}

func TestTokenHintMasks(t *testing.T) {
	got := cfapi.Credential{Mode: cfapi.ModeToken, Token: "abcdefghijklmnopqrst"}.Hint()
	if strings.Contains(got, "efghijklmnop") {
		t.Fatalf("中间该打码: %s", got)
	}
	if !strings.HasPrefix(got, "abcd") || !strings.HasSuffix(got, "qrst") {
		t.Fatalf("头尾该留着: %s", got)
	}
	if (cfapi.Credential{}).Hint() != "" {
		t.Fatal("空凭据该返回空")
	}
}

func TestCFCredentialRejectsIncomplete(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	cases := map[string]string{
		`{"mode":"globalkey","email":"a@b.c"}`:          "都要填",
		`{"mode":"globalkey","key":"k"}`:                "都要填",
		`{"mode":"globalkey","email":"nope","key":"k"}`: "邮箱看着不对",
		`{"mode":"token","token":""}`:                   "Token 是空的",
	}
	for body, want := range cases {
		res, out := c.do("POST", "/api/cf/token", body)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s 该 400，拿到 %d", body, res.StatusCode)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("%s 的错误该提到 %q，拿到 %s", body, want, out)
		}
	}
}

func TestCFNotConfigured(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")
	res, body := c.do("GET", "/api/cf/zones", "")
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "还没连 Cloudflare") {
		t.Fatalf("没配凭据该给出明确提示，拿到 %d %s", res.StatusCode, body)
	}
	_, body = c.do("GET", "/api/state", "")
	if !strings.Contains(body, `"configured":false`) {
		t.Fatalf("状态里该是未配置: %s", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	res, _ := c.do("GET", "/", "")
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if res.Header.Get(k) != want {
			t.Fatalf("%s 该是 %s，拿到 %q", k, want, res.Header.Get(k))
		}
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("CSP 不对: %s", res.Header.Get("Content-Security-Policy"))
	}
}

func TestIndexServed(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	res, body := c.do("GET", "/", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("首页该 200，拿到 %d", res.StatusCode)
	}
	if !strings.Contains(body, "addipv6") {
		t.Fatal("首页内容不对")
	}
	// CSP 只放行 'self'，HTML 里不能再出现内联 style。
	if strings.Contains(body, "style=\"") {
		t.Fatal("页面里不该有内联 style，会被 CSP 拦掉")
	}
}

func TestSetSourceOnPointToPointLink(t *testing.T) {
	// 6in4 这类隧道的默认路由长这样：default dev ipv6net，没有 via。
	// 以前这里会被"没找到默认网关"挡住，现在要放行。
	srv, mgr := newTestServer(t)
	mgr.p2p = true
	mgr.route = &netif.Route{Dev: "ipv6net", Source: netip.MustParseAddr("2001:db8::2")}

	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	_, body := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":1}`)
	var pv previewResponse
	json.Unmarshal([]byte(body), &pv)
	payload, _ := json.Marshal(map[string]any{"iface": "eth0", "addrs": pv.Addrs})
	c.do("POST", "/api/addresses/add", string(payload))

	target := strings.Split(pv.Addrs[0], "/")[0]
	res, body := c.do("POST", "/api/route/source", `{"iface":"eth0","source":"`+target+`"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("点对点链路该能设出口，拿到 %d: %s", res.StatusCode, body)
	}
	if mgr.route.Source.String() != target {
		t.Fatalf("出口该换成 %s，实际 %s", target, mgr.route.Source)
	}
	if mgr.route.Gateway.IsValid() {
		t.Fatalf("点对点链路不该硬塞网关，拿到 %s", mgr.route.Gateway)
	}
}

func TestSetSourceStillNeedsGatewayOnEthernet(t *testing.T) {
	// 普通以太网卡没有网关时仍然要拦住，别让用户把默认路由改坏。
	srv, mgr := newTestServer(t)
	mgr.route = nil
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")
	_, body := c.do("POST", "/api/route/source", `{"iface":"eth0","source":"2001:db8::2"}`)
	if !strings.Contains(body, "没找到 IPv6 默认网关") {
		t.Fatalf("以太网卡缺网关该被拦: %s", body)
	}
}

func TestAddWarnsWhenSourceChanges(t *testing.T) {
	// 只加地址、不碰路由，内核也可能换出站源地址，这事必须报出来。
	srv, mgr := newTestServer(t)
	mgr.hijackSource = true
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")

	_, body := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":1}`)
	var pv previewResponse
	json.Unmarshal([]byte(body), &pv)
	payload, _ := json.Marshal(map[string]any{"iface": "eth0", "addrs": pv.Addrs})
	_, body = c.do("POST", "/api/addresses/add", string(payload))

	var op opResult
	json.Unmarshal([]byte(body), &op)
	if op.Warning == "" {
		t.Fatalf("出口变了该给警告: %s", body)
	}
	if !strings.Contains(op.Warning, "2001:db8::2") {
		t.Fatalf("警告里该说清从哪换到哪: %s", op.Warning)
	}
}

func TestAddQuietWhenSourceStable(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, srv: srv.Handler()}
	c.login("hunter2")
	_, body := c.do("POST", "/api/addresses/preview", `{"iface":"eth0","count":2}`)
	var pv previewResponse
	json.Unmarshal([]byte(body), &pv)
	payload, _ := json.Marshal(map[string]any{"iface": "eth0", "addrs": pv.Addrs})
	_, body = c.do("POST", "/api/addresses/add", string(payload))
	var op opResult
	json.Unmarshal([]byte(body), &op)
	if op.Warning != "" {
		t.Fatalf("出口没变就别瞎报警: %s", op.Warning)
	}
}
