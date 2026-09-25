package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/byJoey/addipv6/internal/netif"
)

// maxBatch 是单次批量操作的地址上限，防手滑输了个天文数字。
const maxBatch = 1000

type addrView struct {
	Addr      string   `json:"addr"`
	Prefix    string   `json:"prefix"`
	PrefixLen int      `json:"prefixLen"`
	Scope     string   `json:"scope"`
	Flags     []string `json:"flags"`
	Managed   bool     `json:"managed"`
	IsSource  bool     `json:"isSource"`
}

type ifaceView struct {
	Name         string     `json:"name"`
	Index        int        `json:"index"`
	Up           bool       `json:"up"`
	MAC          string     `json:"mac"`
	PointToPoint bool       `json:"pointToPoint"`
	Prefixes     []string   `json:"prefixes"`
	Addresses    []addrView `json:"addresses"`
	Gateway      string     `json:"gateway,omitempty"`
	Source       string     `json:"source,omitempty"`
	OnLink       bool       `json:"onLink"`
}

type cfStatus struct {
	Configured bool   `json:"configured"`
	AuthMode   string `json:"authMode"`
	TokenHint  string `json:"tokenHint,omitempty"`
	Email      string `json:"email,omitempty"`
	ZoneID     string `json:"zoneId,omitempty"`
	ZoneName   string `json:"zoneName,omitempty"`
	RecordName string `json:"recordName,omitempty"`
	TTL        int    `json:"ttl"`
	Proxied    bool   `json:"proxied"`
}

type stateResponse struct {
	Platform   string      `json:"platform"`
	Version    string      `json:"version"`
	Interfaces []ifaceView `json:"interfaces"`
	Cloudflare cfStatus    `json:"cloudflare"`
	ConfigPath string      `json:"configPath"`
	StatePath  string      `json:"statePath"`
	Iface      string      `json:"iface,omitempty"`
	ManagedAll int         `json:"managedAll"`
}

func (s *Server) buildState() (stateResponse, error) {
	list, err := s.mgr.List()
	if err != nil {
		return stateResponse{}, err
	}
	cfg := s.store.Config()
	st := s.store.State()
	cred := credentialFrom(cfg.Cloudflare)

	managed := map[string]struct{}{}
	for _, m := range st.Managed {
		managed[m.Iface+"|"+m.Prefix] = struct{}{}
	}

	views := make([]ifaceView, 0, len(list))
	for _, it := range list {
		v := ifaceView{Name: it.Name, Index: it.Index, Up: it.Up, MAC: it.MAC, PointToPoint: it.PointToPoint}
		for _, p := range it.Prefixes {
			v.Prefixes = append(v.Prefixes, p.String())
		}
		var source string
		if it.Route != nil {
			if it.Route.Gateway.IsValid() {
				v.Gateway = it.Route.Gateway.String()
			}
			if it.Route.Source.IsValid() {
				source = it.Route.Source.String()
				v.Source = source
			}
			v.OnLink = it.Route.OnLink
		}
		for _, a := range it.Addresses {
			key := it.Name + "|" + a.String()
			v.Addresses = append(v.Addresses, addrView{
				Addr:      a.Addr.String(),
				Prefix:    a.String(),
				PrefixLen: a.PrefixLen,
				Scope:     a.Scope,
				Flags:     a.Flags,
				Managed:   func() bool { _, ok := managed[key]; return ok }(),
				IsSource:  source != "" && a.Addr.String() == source,
			})
		}
		views = append(views, v)
	}

	return stateResponse{
		Platform:   s.mgr.Platform(),
		Version:    s.version,
		Interfaces: views,
		ConfigPath: s.store.ConfigPath(),
		StatePath:  s.store.StatePath(),
		Iface:      cfg.Iface,
		ManagedAll: len(st.Managed),
		Cloudflare: cfStatus{
			Configured: cred.Valid(),
			AuthMode:   cred.Mode,
			TokenHint:  cred.Hint(),
			Email:      cred.Email,
			ZoneID:     cfg.Cloudflare.ZoneID,
			ZoneName:   cfg.Cloudflare.ZoneName,
			RecordName: cfg.Cloudflare.RecordName,
			TTL:        cfg.Cloudflare.TTL,
			Proxied:    cfg.Cloudflare.Proxied,
		},
	}, nil
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st, err := s.buildState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type previewRequest struct {
	Iface  string `json:"iface"`
	Prefix string `json:"prefix"`
	Count  int    `json:"count"`
}

type previewResponse struct {
	Prefix  string   `json:"prefix"`
	Addrs   []string `json:"addrs"`
	Warning string   `json:"warning,omitempty"`
}

// resolvePrefix 定位要用哪个网段：显式指定优先，否则取网卡上第一个可用网段。
func (s *Server) resolvePrefix(ifaceName, prefix string) (netif.Interface, netip.Prefix, error) {
	list, err := s.mgr.List()
	if err != nil {
		return netif.Interface{}, netip.Prefix{}, err
	}
	it, ok := netif.Find(list, ifaceName)
	if !ok {
		return netif.Interface{}, netip.Prefix{}, fmt.Errorf("找不到网卡 %s，或者它没有全局 IPv6", ifaceName)
	}
	if strings.TrimSpace(prefix) != "" {
		p, err := netip.ParsePrefix(strings.TrimSpace(prefix))
		if err != nil {
			return it, netip.Prefix{}, fmt.Errorf("网段写法不对: %w", err)
		}
		return it, p.Masked(), nil
	}
	if len(it.Prefixes) == 0 {
		return it, netip.Prefix{}, fmt.Errorf("%s 上没有能用来随机的网段（/127 和 /128 不行）", ifaceName)
	}
	return it, it.Prefixes[0], nil
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Count <= 0 || req.Count > maxBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("数量要在 1 到 %d 之间", maxBatch))
		return
	}
	it, p, err := s.resolvePrefix(req.Iface, req.Prefix)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	exclude := map[netip.Addr]struct{}{}
	for _, a := range it.Addresses {
		exclude[a.Addr] = struct{}{}
	}
	if it.Route != nil && it.Route.Gateway.IsValid() {
		exclude[it.Route.Gateway] = struct{}{}
	}

	addrs, err := netif.GenerateBatch(p, req.Count, exclude)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := previewResponse{Prefix: p.String()}
	for _, a := range addrs {
		out.Addrs = append(out.Addrs, fmt.Sprintf("%s/%d", a, p.Bits()))
	}
	if p.Bits() != 64 {
		out.Warning = fmt.Sprintf("这个网段是 /%d，不是常见的 /64，确认一下是不是你要的", p.Bits())
	}
	writeJSON(w, http.StatusOK, out)
}

type addrBatchRequest struct {
	Iface string   `json:"iface"`
	Addrs []string `json:"addrs"`
	Force bool     `json:"force,omitempty"`
}

type opItem struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type opResult struct {
	Items   []opItem `json:"items"`
	Succeed int      `json:"succeed"`
	Failed  int      `json:"failed"`
	Warning string   `json:"warning,omitempty"`
}

// currentSource 读一下这块网卡当前的出站源地址。
func (s *Server) currentSource(iface string) string {
	list, err := s.mgr.List()
	if err != nil {
		return ""
	}
	it, ok := netif.Find(list, iface)
	if !ok || it.Route == nil || !it.Route.Source.IsValid() {
		return ""
	}
	return it.Route.Source.String()
}

func (s *Server) handleAddAddrs(w http.ResponseWriter, r *http.Request) {
	var req addrBatchRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Addrs) == 0 {
		writeError(w, http.StatusBadRequest, "没给要添加的地址")
		return
	}
	if len(req.Addrs) > maxBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("一次最多 %d 个", maxBatch))
		return
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()

	// 内核按 RFC 6724 挑出站源地址，多加几个同段地址就可能把出口换掉，
	// 哪怕一行路由都没动。已有服务绑着旧地址的话这会很难查，所以加完对一下。
	sourceBefore := s.currentSource(req.Iface)

	res := opResult{}
	for _, raw := range req.Addrs {
		p, err := netif.ParsePrefix(raw, 64)
		item := opItem{Target: raw}
		if err != nil {
			item.Error = err.Error()
			res.Items = append(res.Items, item)
			res.Failed++
			continue
		}
		item.Target = p.String()
		if err := s.mgr.AddAddr(req.Iface, p); err != nil {
			item.Error = err.Error()
			res.Failed++
		} else {
			item.OK = true
			res.Succeed++
			if err := s.store.TrackAddr(req.Iface, p.String()); err != nil {
				s.log.Error("记录地址失败", "err", err)
			}
		}
		res.Items = append(res.Items, item)
	}
	if sourceAfter := s.currentSource(req.Iface); sourceBefore != "" && sourceAfter != "" && sourceBefore != sourceAfter {
		res.Warning = "注意：出站源地址被内核从 " + sourceBefore + " 换成了 " + sourceAfter +
			"。想钉死用哪个就勾中它点「设为出口」。"
		s.log.Warn("添加地址后出口变了", "iface", req.Iface, "从", sourceBefore, "到", sourceAfter)
	}
	s.log.Info("批量添加地址", "iface", req.Iface, "成功", res.Succeed, "失败", res.Failed)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeleteAddrs(w http.ResponseWriter, r *http.Request) {
	var req addrBatchRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Addrs) == 0 {
		writeError(w, http.StatusBadRequest, "没给要删除的地址")
		return
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()

	list, err := s.mgr.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	it, ok := netif.Find(list, req.Iface)
	if !ok {
		writeError(w, http.StatusBadRequest, "找不到网卡 "+req.Iface)
		return
	}

	// 先算清楚这块网卡还剩多少全局地址，别把自己从网络上摘下来。
	globals := map[string]bool{}
	for _, a := range it.Addresses {
		if a.Scope == "global" {
			globals[a.String()] = true
		}
	}
	var sourceAddr string
	if it.Route != nil && it.Route.Source.IsValid() {
		sourceAddr = it.Route.Source.String()
	}

	targets := make([]netip.Prefix, 0, len(req.Addrs))
	res := opResult{}
	for _, raw := range req.Addrs {
		p, err := netif.ParsePrefix(raw, 64)
		if err != nil {
			res.Items = append(res.Items, opItem{Target: raw, Error: err.Error()})
			res.Failed++
			continue
		}
		targets = append(targets, p)
	}

	if !req.Force {
		remaining := len(globals)
		for _, p := range targets {
			if globals[p.String()] {
				remaining--
			}
		}
		if len(globals) > 0 && remaining <= 0 {
			writeError(w, http.StatusConflict,
				"这批删完 "+req.Iface+" 上就一个全局 IPv6 都不剩了，会直接断网。想清楚再勾 强制执行")
			return
		}
	}

	for _, p := range targets {
		item := opItem{Target: p.String()}
		if !req.Force && sourceAddr != "" && p.Addr().String() == sourceAddr {
			item.Error = "这是当前默认出口地址，删了出站会断。要删先换个出口，或者勾强制执行"
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		if err := s.mgr.DelAddr(req.Iface, p); err != nil {
			item.Error = err.Error()
			res.Failed++
		} else {
			item.OK = true
			res.Succeed++
		}
		// 不管内核那边删没删掉，本地登记都要清，不然状态会对不上。
		if err := s.store.UntrackAddr(req.Iface, p.String()); err != nil {
			s.log.Error("清理地址记录失败", "err", err)
		}
		res.Items = append(res.Items, item)
	}
	s.log.Info("批量删除地址", "iface", req.Iface, "成功", res.Succeed, "失败", res.Failed)
	writeJSON(w, http.StatusOK, res)
}

type sourceRequest struct {
	Iface   string `json:"iface"`
	Source  string `json:"source"`
	Gateway string `json:"gateway,omitempty"`
	OnLink  bool   `json:"onLink,omitempty"`
}

func (s *Server) handleSetSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	src, err := netip.ParseAddr(strings.TrimSpace(req.Source))
	if err != nil || !src.Is6() {
		writeError(w, http.StatusBadRequest, "源地址不是合法的 IPv6")
		return
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()

	list, err := s.mgr.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	it, ok := netif.Find(list, req.Iface)
	if !ok {
		writeError(w, http.StatusBadRequest, "找不到网卡 "+req.Iface)
		return
	}
	found := false
	for _, a := range it.Addresses {
		if a.Addr == src {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusBadRequest, src.String()+" 不在 "+req.Iface+" 上，先把它加上再设出口")
		return
	}

	gw := netip.Addr{}
	onLink := req.OnLink
	if strings.TrimSpace(req.Gateway) != "" {
		gw, err = netip.ParseAddr(strings.TrimSpace(req.Gateway))
		if err != nil || !gw.Is6() {
			writeError(w, http.StatusBadRequest, "网关不是合法的 IPv6")
			return
		}
	} else if it.Route != nil && it.Route.Gateway.IsValid() {
		gw = it.Route.Gateway
		onLink = it.Route.OnLink
	}
	// 点对点链路（6in4 隧道之类）的默认路由本来就没网关，这种情况不拦。
	if !gw.IsValid() && !it.PointToPoint {
		writeError(w, http.StatusBadRequest, "没找到 IPv6 默认网关，手动填一个再试")
		return
	}

	if err := s.mgr.SetDefaultRoute(req.Iface, gw, src, onLink); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SetDefaultSource(req.Iface, src.String()); err != nil {
		s.log.Error("记录默认出口失败", "err", err)
	}
	resp := map[string]any{"ok": true, "source": src.String(), "onLink": onLink}
	gwText := "(点对点链路，无网关)"
	if gw.IsValid() {
		gwText = gw.String()
		resp["gateway"] = gwText
	}
	s.log.Info("切换默认出口", "iface", req.Iface, "src", src.String(), "gw", gwText)
	writeJSON(w, http.StatusOK, resp)
}

// handleRestore 把状态文件里登记过的地址和出口重新下发一遍。
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	res := s.restore()
	writeJSON(w, http.StatusOK, res)
}

// restore 是开机恢复和手动恢复共用的逻辑。
func (s *Server) restore() opResult {
	st := s.store.State()
	res := opResult{}

	sort.SliceStable(st.Managed, func(i, j int) bool { return st.Managed[i].Prefix < st.Managed[j].Prefix })
	for _, m := range st.Managed {
		item := opItem{Target: m.Iface + " " + m.Prefix}
		p, err := netip.ParsePrefix(m.Prefix)
		if err != nil {
			item.Error = err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		err = s.mgr.AddAddr(m.Iface, p)
		// 已经在网卡上就算恢复成功，这个流程必须可以反复跑。
		if errors.Is(err, netif.ErrAddrExists) {
			err = nil
		}
		if err != nil {
			item.Error = err.Error()
			res.Failed++
		} else {
			item.OK = true
			res.Succeed++
		}
		res.Items = append(res.Items, item)
	}

	for iface, src := range st.DefaultSource {
		item := opItem{Target: iface + " 出口 " + src}
		addr, err := netip.ParseAddr(src)
		if err != nil {
			item.Error = err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		list, err := s.mgr.List()
		if err != nil {
			item.Error = err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		it, ok := netif.Find(list, iface)
		if !ok {
			item.Error = "网卡不在了"
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		gw := netip.Addr{}
		onLink := false
		if it.Route != nil {
			gw, onLink = it.Route.Gateway, it.Route.OnLink
		}
		if !gw.IsValid() && !it.PointToPoint {
			item.Error = "没有默认网关，跳过"
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		if err := s.mgr.SetDefaultRoute(iface, gw, addr, onLink); err != nil {
			item.Error = err.Error()
			res.Failed++
		} else {
			item.OK = true
			res.Succeed++
		}
		res.Items = append(res.Items, item)
	}
	return res
}

// Restore 供 restore 子命令调用。
func (s *Server) Restore() (int, int) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	res := s.restore()
	return res.Succeed, res.Failed
}
