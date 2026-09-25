package server

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/byJoey/addipv6/internal/cfapi"
	"github.com/byJoey/addipv6/internal/store"
)

// cfTimeout 给批量操作留足时间，1000 条记录按 4 次/秒也要四分多钟。
const cfTimeout = 10 * time.Minute

// credential 把落盘的配置翻成一份凭据。
func credentialFrom(c store.CloudflareConfig) cfapi.Credential {
	return cfapi.Credential{
		Mode:  c.AuthMode,
		Token: c.Token,
		Email: c.Email,
		Key:   c.GlobalKey,
	}.Normalize()
}

// client 拿一个跟当前凭据对应的客户端，凭据没变就复用。
func (s *Server) client() (*cfapi.Client, error) {
	cred := credentialFrom(s.store.Config().Cloudflare)
	if !cred.Valid() {
		return nil, fmt.Errorf("还没连 Cloudflare，先填 API Token 或者全局 Key")
	}
	key := cred.Mode + "|" + cred.Token + "|" + cred.Email + "|" + cred.Key
	s.cfMu.Lock()
	defer s.cfMu.Unlock()
	if s.cfClient == nil || s.cfToken != key {
		s.cfClient = cfapi.NewWithCredential(cred)
		s.cfToken = key
	}
	return s.cfClient, nil
}

func (s *Server) cfContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), cfTimeout)
}

type cfTokenRequest struct {
	Mode  string `json:"mode,omitempty"`
	Token string `json:"token,omitempty"`
	Email string `json:"email,omitempty"`
	Key   string `json:"key,omitempty"`
}

func (s *Server) handleCFToken(w http.ResponseWriter, r *http.Request) {
	var req cfTokenRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cred := cfapi.Credential{
		Mode: req.Mode, Token: req.Token, Email: req.Email, Key: req.Key,
	}.Normalize()
	if !cred.Valid() {
		if cred.Mode == cfapi.ModeGlobalKey {
			writeError(w, http.StatusBadRequest, "邮箱和全局 Key 都要填")
		} else {
			writeError(w, http.StatusBadRequest, "Token 是空的")
		}
		return
	}
	if cred.Mode == cfapi.ModeGlobalKey && !strings.Contains(cred.Email, "@") {
		writeError(w, http.StatusBadRequest, "邮箱看着不对，要填 Cloudflare 账号的登录邮箱")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	probe := cfapi.NewWithCredential(cred)
	what := "Token"
	if cred.Mode == cfapi.ModeGlobalKey {
		what = "全局 Key"
	}
	if _, err := probe.Verify(ctx); err != nil {
		writeError(w, http.StatusBadRequest, what+"校验没过: "+err.Error())
		return
	}
	zones, err := probe.ListZones(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, what+"能用，但列站点失败，八成是权限不够: "+err.Error())
		return
	}
	if err := s.store.UpdateConfig(func(c *store.Config) {
		c.Cloudflare.AuthMode = cred.Mode
		if cred.Mode == cfapi.ModeGlobalKey {
			c.Cloudflare.Email, c.Cloudflare.GlobalKey, c.Cloudflare.Token = cred.Email, cred.Key, ""
		} else {
			c.Cloudflare.Token, c.Cloudflare.Email, c.Cloudflare.GlobalKey = cred.Token, "", ""
		}
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	s.cfMu.Lock()
	s.cfClient, s.cfToken = probe, cred.Mode+"|"+cred.Token+"|"+cred.Email+"|"+cred.Key
	s.cfMu.Unlock()
	s.log.Info("已连上 Cloudflare", "方式", what, "站点数", len(zones))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "zones": zones})
}

func (s *Server) handleCFTokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.UpdateConfig(func(c *store.Config) {
		c.Cloudflare.Token = ""
		c.Cloudflare.Email = ""
		c.Cloudflare.GlobalKey = ""
		c.Cloudflare.AuthMode = ""
		c.Cloudflare.ZoneID = ""
		c.Cloudflare.ZoneName = ""
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.cfMu.Lock()
	s.cfClient, s.cfToken = nil, ""
	s.cfMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCFZones(w http.ResponseWriter, r *http.Request) {
	c, err := s.client()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	zones, err := c.ListZones(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zones})
}

type cfZoneRequest struct {
	ZoneID     string `json:"zoneId"`
	ZoneName   string `json:"zoneName"`
	RecordName string `json:"recordName,omitempty"`
	TTL        int    `json:"ttl,omitempty"`
	Proxied    bool   `json:"proxied,omitempty"`
}

func (s *Server) handleCFSelectZone(w http.ResponseWriter, r *http.Request) {
	var req cfZoneRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateConfig(func(c *store.Config) {
		c.Cloudflare.ZoneID = strings.TrimSpace(req.ZoneID)
		c.Cloudflare.ZoneName = strings.TrimSpace(req.ZoneName)
		c.Cloudflare.RecordName = strings.TrimSpace(req.RecordName)
		if req.TTL > 0 {
			c.Cloudflare.TTL = req.TTL
		}
		c.Cloudflare.Proxied = req.Proxied
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCFRecords(w http.ResponseWriter, r *http.Request) {
	c, err := s.client()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	zoneID := strings.TrimSpace(r.URL.Query().Get("zoneId"))
	if zoneID == "" {
		zoneID = s.store.Config().Cloudflare.ZoneID
	}
	if zoneID == "" {
		writeError(w, http.StatusBadRequest, "没选站点")
		return
	}
	recType := strings.TrimSpace(r.URL.Query().Get("type"))
	if recType == "" {
		recType = "AAAA"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	records, err := c.ListRecords(ctx, zoneID, recType)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	type recView struct {
		cfapi.Record
		Managed bool `json:"managed"`
	}
	out := make([]recView, 0, len(records))
	managed := 0
	for _, rec := range records {
		if rec.Managed() {
			managed++
		}
		out = append(out, recView{Record: rec, Managed: rec.Managed()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out, "managed": managed, "total": len(out)})
}

type cfCreateRequest struct {
	ZoneID   string   `json:"zoneId"`
	ZoneName string   `json:"zoneName"`
	Name     string   `json:"name"`
	Addrs    []string `json:"addrs"`
	TTL      int      `json:"ttl"`
	Proxied  bool     `json:"proxied"`
}

func (s *Server) handleCFCreate(w http.ResponseWriter, r *http.Request) {
	var req cfCreateRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.client()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.store.Config()
	zoneID := strings.TrimSpace(req.ZoneID)
	zoneName := strings.TrimSpace(req.ZoneName)
	if zoneID == "" {
		zoneID, zoneName = cfg.Cloudflare.ZoneID, cfg.Cloudflare.ZoneName
	}
	if zoneID == "" || zoneName == "" {
		writeError(w, http.StatusBadRequest, "没选站点")
		return
	}
	if len(req.Addrs) == 0 {
		writeError(w, http.StatusBadRequest, "没勾地址")
		return
	}
	if len(req.Addrs) > maxBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("一次最多 %d 条", maxBatch))
		return
	}

	addrs := make([]netip.Addr, 0, len(req.Addrs))
	for _, raw := range req.Addrs {
		raw = strings.TrimSpace(raw)
		if i := strings.IndexByte(raw, '/'); i >= 0 {
			raw = raw[:i]
		}
		a, err := netip.ParseAddr(raw)
		if err != nil || !a.Is6() || a.Is4In6() {
			writeError(w, http.StatusBadRequest, raw+" 不是合法的 IPv6")
			return
		}
		if a.IsLinkLocalUnicast() || a.IsLoopback() || a.IsUnspecified() {
			writeError(w, http.StatusBadRequest, raw+" 是本地地址，解析到公网没有意义")
			return
		}
		addrs = append(addrs, a)
	}

	names, err := cfapi.ExpandNames(req.Name, zoneName, len(addrs))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = cfg.Cloudflare.TTL
	}

	ctx, cancel := s.cfContext(r)
	defer cancel()
	res, err := c.BatchCreateAAAA(ctx, zoneID, names, addrs, ttl, req.Proxied)
	if err != nil && res == nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 顺手把这次用的参数记下来，下次打开界面不用重填。
	if err := s.store.UpdateConfig(func(cc *store.Config) {
		cc.Cloudflare.ZoneID = zoneID
		cc.Cloudflare.ZoneName = zoneName
		cc.Cloudflare.RecordName = strings.TrimSpace(req.Name)
		cc.Cloudflare.TTL = ttl
		cc.Cloudflare.Proxied = req.Proxied
	}); err != nil {
		s.log.Error("保存 Cloudflare 参数失败", "err", err)
	}
	s.log.Info("批量解析到 Cloudflare", "zone", zoneName,
		"新建", res.Created, "跳过", res.Skipped, "失败", res.Failed)
	writeJSON(w, http.StatusOK, res)
}

type cfDeleteRequest struct {
	ZoneID string   `json:"zoneId"`
	IDs    []string `json:"ids"`
}

func (s *Server) handleCFDelete(w http.ResponseWriter, r *http.Request) {
	var req cfDeleteRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.client()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	zoneID := strings.TrimSpace(req.ZoneID)
	if zoneID == "" {
		zoneID = s.store.Config().Cloudflare.ZoneID
	}
	if zoneID == "" {
		writeError(w, http.StatusBadRequest, "没选站点")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "没勾要删的记录")
		return
	}

	records := make([]cfapi.Record, 0, len(req.IDs))
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		records = append(records, cfapi.Record{ID: id})
	}

	ctx, cancel := s.cfContext(r)
	defer cancel()
	res, err := c.BatchDelete(ctx, zoneID, records)
	if err != nil && res == nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("批量删除 Cloudflare 记录", "删除", res.Deleted, "跳过", res.Skipped, "失败", res.Failed)
	writeJSON(w, http.StatusOK, res)
}
