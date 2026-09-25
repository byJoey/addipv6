package cfapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCF 是一个够用的 Cloudflare 假服务端，用来验证批量逻辑和重试。
type fakeCF struct {
	mu        sync.Mutex
	records   map[string]Record
	nextID    int
	creates   int32
	deletes   int32
	failOnce  map[string]bool // content -> 第一次创建先失败一次
	rateLimit int32           // 前 N 次请求返回 429
	sawToken  bool
	sawKey    bool
}

func newFakeCF() *fakeCF {
	return &fakeCF{records: map[string]Record{}, failOnce: map[string]bool{}}
}

func (f *fakeCF) ok(w http.ResponseWriter, result any) {
	json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "result": result,
		"result_info": map[string]int{"page": 1, "per_page": 100, "total_pages": 1},
	})
}

func (f *fakeCF) bad(w http.ResponseWriter, status, code int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"errors":  []map[string]any{{"code": code, "message": msg}},
	})
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&f.rateLimit) > 0 {
		atomic.AddInt32(&f.rateLimit, -1)
		w.Header().Set("Retry-After", "0")
		f.bad(w, http.StatusTooManyRequests, 971, "rate limited")
		return
	}
	byToken := r.Header.Get("Authorization") == "Bearer good-token"
	byKey := r.Header.Get("X-Auth-Email") == "me@example.com" && r.Header.Get("X-Auth-Key") == "good-key"
	if !byToken && !byKey {
		f.bad(w, http.StatusUnauthorized, 1000, "invalid token")
		return
	}
	f.mu.Lock()
	if byKey {
		f.sawKey = true
	} else {
		f.sawToken = true
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/user/tokens/verify":
		// 真实的 Cloudflare 不让全局 Key 打这个端点。
		if byKey {
			f.bad(w, http.StatusForbidden, 9109, "Invalid access token")
			return
		}
		f.ok(w, map[string]string{"id": "tok1", "status": "active"})
	case r.URL.Path == "/user":
		// 反过来 Token 也打不了 /user。
		if byToken {
			f.bad(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		f.ok(w, map[string]string{"id": "usr1", "email": "me@example.com"})
	case r.URL.Path == "/zones":
		f.ok(w, []Zone{{ID: "z1", Name: "example.com", Status: "active"}})
	case r.URL.Path == "/zones/z1/dns_records" && r.Method == "GET":
		f.mu.Lock()
		out := make([]Record, 0, len(f.records))
		for _, rec := range f.records {
			out = append(out, rec)
		}
		f.mu.Unlock()
		f.ok(w, out)
	case r.URL.Path == "/zones/z1/dns_records" && r.Method == "POST":
		atomic.AddInt32(&f.creates, 1)
		var req CreateRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failOnce[req.Content] {
			delete(f.failOnce, req.Content)
			f.bad(w, http.StatusBadRequest, 1004, "DNS Validation Error")
			return
		}
		f.nextID++
		id := "r" + strconv.Itoa(f.nextID)
		rec := Record{ID: id, Type: req.Type, Name: req.Name, Content: req.Content,
			TTL: req.TTL, Proxied: req.Proxied, Comment: req.Comment}
		f.records[id] = rec
		f.ok(w, rec)
	case strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records/") && r.Method == "DELETE":
		atomic.AddInt32(&f.deletes, 1)
		id := strings.TrimPrefix(r.URL.Path, "/zones/z1/dns_records/")
		f.mu.Lock()
		_, ok := f.records[id]
		delete(f.records, id)
		f.mu.Unlock()
		if !ok {
			f.bad(w, http.StatusNotFound, 81044, "Record does not exist.")
			return
		}
		f.ok(w, map[string]string{"id": id})
	default:
		f.bad(w, http.StatusNotFound, 7003, "no route")
	}
}

func newTestClient(t *testing.T, f *fakeCF) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("good-token")
	c.BaseURL = srv.URL
	// 测试里不需要真限速，不然一条一条太慢。
	c.limiter = newLimiter(1000, time.Second)
	return c
}

func addrs(t *testing.T, list ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(list))
	for _, s := range list {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestVerifyAndZones(t *testing.T) {
	c := newTestClient(t, newFakeCF())
	if _, err := c.Verify(context.Background()); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	zones, err := c.ListZones(context.Background())
	if err != nil || len(zones) != 1 || zones[0].Name != "example.com" {
		t.Fatalf("站点列表不对: %v %v", zones, err)
	}
}

func TestBadTokenRejected(t *testing.T) {
	f := newFakeCF()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New("wrong")
	c.BaseURL = srv.URL
	_, err := c.Verify(context.Background())
	if err == nil {
		t.Fatal("坏 Token 应该报错")
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("错误信息该带上 Cloudflare 的原话，拿到: %v", err)
	}
}

func TestBatchCreateSkipsExisting(t *testing.T) {
	f := newFakeCF()
	c := newTestClient(t, f)
	ctx := context.Background()
	list := addrs(t, "2001:db8::1", "2001:db8::2", "2001:db8::3")
	names := []string{"a.example.com", "a.example.com", "a.example.com"}

	res, err := c.BatchCreateAAAA(ctx, "z1", names, list, 60, false)
	if err != nil {
		t.Fatalf("第一次批量失败: %v", err)
	}
	if res.Created != 3 || res.Failed != 0 {
		t.Fatalf("想要新建 3 条，拿到 %+v", res)
	}

	// 再跑一遍必须全部跳过，不能建出重复记录。
	res2, err := c.BatchCreateAAAA(ctx, "z1", names, list, 60, false)
	if err != nil {
		t.Fatalf("第二次批量失败: %v", err)
	}
	if res2.Created != 0 || res2.Skipped != 3 {
		t.Fatalf("重复执行该全跳过，拿到 %+v", res2)
	}
	if len(f.records) != 3 {
		t.Fatalf("服务端该只有 3 条，实际 %d", len(f.records))
	}
}

func TestBatchCreateDedupsWithinOneBatch(t *testing.T) {
	f := newFakeCF()
	c := newTestClient(t, f)
	// 同一批里塞两个一模一样的，并发下也只能建一条。
	list := addrs(t, "2001:db8::9", "2001:db8::9")
	names := []string{"dup.example.com", "dup.example.com"}
	res, err := c.BatchCreateAAAA(context.Background(), "z1", names, list, 60, false)
	if err != nil {
		t.Fatalf("批量失败: %v", err)
	}
	if res.Created != 1 || res.Skipped != 1 {
		t.Fatalf("同批重复该只建一条，拿到 %+v", res)
	}
}

func TestBatchCreatePartialFailure(t *testing.T) {
	f := newFakeCF()
	f.failOnce["2001:db8::2"] = true
	c := newTestClient(t, f)
	list := addrs(t, "2001:db8::1", "2001:db8::2", "2001:db8::3")
	names := []string{"n1.example.com", "n2.example.com", "n3.example.com"}

	res, err := c.BatchCreateAAAA(context.Background(), "z1", names, list, 60, false)
	if err != nil {
		t.Fatalf("批量不该整体失败: %v", err)
	}
	if res.Created != 2 || res.Failed != 1 {
		t.Fatalf("想要 2 成 1 败，拿到 %+v", res)
	}
	var failed BatchItem
	for _, it := range res.Items {
		if !it.OK {
			failed = it
		}
	}
	if !strings.Contains(failed.Error, "DNS Validation Error") {
		t.Fatalf("失败项该带原因，拿到 %q", failed.Error)
	}
	// 失败的那条要能重试成功，说明占位被正确撤销了。
	res2, _ := c.BatchCreateAAAA(context.Background(), "z1", names, list, 60, false)
	if res2.Created != 1 || res2.Skipped != 2 {
		t.Fatalf("重试该补上那一条，拿到 %+v", res2)
	}
}

func TestBatchCreateNamesPerAddress(t *testing.T) {
	f := newFakeCF()
	c := newTestClient(t, f)
	list := addrs(t, "2001:db8::1", "2001:db8::2")
	names, err := ExpandNames("node-{n}", "example.com", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BatchCreateAAAA(context.Background(), "z1", names, list, 120, true); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range f.records {
		got[r.Name] = r.Content
		if r.TTL != 120 || !r.Proxied {
			t.Fatalf("TTL / proxied 没透传: %+v", r)
		}
		if r.Comment != RecordComment {
			t.Fatalf("该打上标记，拿到 %q", r.Comment)
		}
	}
	if got["node-1.example.com"] != "2001:db8::1" || got["node-2.example.com"] != "2001:db8::2" {
		t.Fatalf("名字和地址没对上: %v", got)
	}
}

func TestBatchDelete(t *testing.T) {
	f := newFakeCF()
	c := newTestClient(t, f)
	ctx := context.Background()
	list := addrs(t, "2001:db8::1", "2001:db8::2")
	names := []string{"x.example.com", "x.example.com"}
	if _, err := c.BatchCreateAAAA(ctx, "z1", names, list, 60, false); err != nil {
		t.Fatal(err)
	}
	recs, err := c.ListRecords(ctx, "z1", "AAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if !r.Managed() {
			t.Fatalf("该被认成本工具建的: %+v", r)
		}
	}
	res, err := c.BatchDelete(ctx, "z1", recs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || res.Failed != 0 {
		t.Fatalf("想要删 2 条，拿到 %+v", res)
	}
	// 再删一次：记录已经没了，按跳过处理而不是失败。
	res2, err := c.BatchDelete(ctx, "z1", recs)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Skipped != 2 || res2.Failed != 0 {
		t.Fatalf("重复删除该算跳过，拿到 %+v", res2)
	}
}

func TestRetriesOn429(t *testing.T) {
	f := newFakeCF()
	f.rateLimit = 3 // 头三次请求全 429
	c := newTestClient(t, f)
	if _, err := c.Verify(context.Background()); err != nil {
		t.Fatalf("限速后该自己重试成功，却报错: %v", err)
	}
}

func TestContextCancelStopsBatch(t *testing.T) {
	f := newFakeCF()
	c := newTestClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	list := make([]netip.Addr, 0, 20)
	names := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		list = append(list, netip.MustParseAddr(fmt.Sprintf("2001:db8::%d", i+1)))
		names = append(names, "c.example.com")
	}
	if _, err := c.BatchCreateAAAA(ctx, "z1", names, list, 60, false); err == nil {
		t.Fatal("已取消的 context 应该让批量直接退出")
	}
}

func newTestClientCred(t *testing.T, f *fakeCF, cred Credential) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := NewWithCredential(cred)
	c.BaseURL = srv.URL
	c.limiter = newLimiter(1000, time.Second)
	return c
}

func TestGlobalKeyAuth(t *testing.T) {
	f := newFakeCF()
	c := newTestClientCred(t, f, Credential{Mode: ModeGlobalKey, Email: "me@example.com", Key: "good-key"})
	id, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("全局 Key 校验该通过: %v", err)
	}
	if id.Email != "me@example.com" {
		t.Fatalf("该拿到邮箱，得到 %+v", id)
	}
	if !f.sawKey || f.sawToken {
		t.Fatalf("该走 X-Auth-Email/X-Auth-Key 这条路，sawKey=%v sawToken=%v", f.sawKey, f.sawToken)
	}
	// 建删记录也得能用同一把 Key。
	list := addrs(t, "2001:db8::a1")
	res, err := c.BatchCreateAAAA(context.Background(), "z1", []string{"k.example.com"}, list, 60, false)
	if err != nil || res.Created != 1 {
		t.Fatalf("全局 Key 该能建记录: %+v %v", res, err)
	}
}

func TestGlobalKeyWrongEmailRejected(t *testing.T) {
	f := newFakeCF()
	c := newTestClientCred(t, f, Credential{Mode: ModeGlobalKey, Email: "me@example.com", Key: "wrong-key"})
	if _, err := c.Verify(context.Background()); err == nil {
		t.Fatal("Key 不对该报错")
	}
}

func TestGlobalKeyEmailCaseInsensitive(t *testing.T) {
	f := newFakeCF()
	// 用户手打大写邮箱也得能连上，Cloudflare 那边存的是小写。
	c := newTestClientCred(t, f, Credential{Mode: ModeGlobalKey, Email: "  ME@Example.COM ", Key: "good-key"})
	if c.Cred.Email != "me@example.com" {
		t.Fatalf("邮箱该被归一成小写，拿到 %q", c.Cred.Email)
	}
	if _, err := c.Verify(context.Background()); err != nil {
		t.Fatalf("大小写不同不该连不上: %v", err)
	}
}

func TestTokenCannotUseUserEndpoint(t *testing.T) {
	// 这条固化住两种认证的端点差异：Token 打 /user 会被挡。
	f := newFakeCF()
	c := newTestClient(t, f)
	var out Identity
	if _, err := c.do(context.Background(), "GET", "/user", nil, nil, &out); err == nil {
		t.Fatal("Token 打 /user 该被拒")
	}
}

func TestCredentialNormalizeAndHint(t *testing.T) {
	// 老配置只有 token 字段、没有 mode，要能自动认成 Token 模式。
	legacy := Credential{Token: "  abcdefghijklmnopqrst  "}.Normalize()
	if legacy.Mode != ModeToken || legacy.Token != "abcdefghijklmnopqrst" {
		t.Fatalf("老配置该按 Token 处理: %+v", legacy)
	}
	if !legacy.Valid() {
		t.Fatal("填了 token 就该有效")
	}

	onlyKey := Credential{Email: "a@b.c", Key: "k"}.Normalize()
	if onlyKey.Mode != ModeGlobalKey {
		t.Fatalf("只有 key 的配置该认成全局 Key: %+v", onlyKey)
	}

	if (Credential{Mode: ModeGlobalKey, Email: "a@b.c"}).Valid() {
		t.Fatal("全局 Key 缺 key 该判无效")
	}
	if (Credential{Mode: ModeGlobalKey, Key: "k"}).Valid() {
		t.Fatal("全局 Key 缺邮箱该判无效")
	}

	h := Credential{Mode: ModeGlobalKey, Email: "me@example.com", Key: "abcdefghijklmnopqrst"}.Hint()
	if !strings.Contains(h, "me@example.com") || strings.Contains(h, "efghijklmnop") {
		t.Fatalf("提示该留邮箱、打码 Key: %s", h)
	}
	if (Credential{}).Hint() != "" {
		t.Fatal("空凭据该返回空提示")
	}
}
