//go:build linux

package netif

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// linuxManager 直接走 netlink，不依赖 iproute2。
type linuxManager struct{}

// New 返回当前平台的网卡管理器。
func New() Manager { return linuxManager{} }

func (linuxManager) Platform() string { return "linux" }

func scopeName(s int) string {
	switch s {
	case unix.RT_SCOPE_UNIVERSE:
		return "global"
	case unix.RT_SCOPE_SITE:
		return "site"
	case unix.RT_SCOPE_LINK:
		return "link"
	case unix.RT_SCOPE_HOST:
		return "host"
	default:
		return fmt.Sprintf("scope%d", s)
	}
}

// addrFlagNames 把 IFA_F_* 位翻成人看得懂的短标签。
func addrFlagNames(flags int) []string {
	table := []struct {
		bit  int
		name string
	}{
		{unix.IFA_F_TEMPORARY, "temporary"},
		{unix.IFA_F_TENTATIVE, "tentative"},
		{unix.IFA_F_DEPRECATED, "deprecated"},
		{unix.IFA_F_DADFAILED, "dadfailed"},
		{unix.IFA_F_MANAGETEMPADDR, "mngtmpaddr"},
		{unix.IFA_F_NOPREFIXROUTE, "noprefixroute"},
		{unix.IFA_F_PERMANENT, "permanent"},
	}
	var out []string
	for _, t := range table {
		if flags&t.bit != 0 {
			out = append(out, t.name)
		}
	}
	return out
}

func toAddr(ip net.IP) (netip.Addr, bool) {
	a, ok := netip.AddrFromSlice(ip.To16())
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func (m linuxManager) List() ([]Interface, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("枚举网卡失败: %w", err)
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V6)
	if err != nil {
		// 路由读不到不该拖垮整个列表，后面只是少显示一条默认出口。
		routes = nil
	}

	var out []Interface
	for _, link := range links {
		attrs := link.Attrs()
		if attrs.Flags&net.FlagLoopback != 0 {
			continue
		}
		raw, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			continue
		}
		var addrs []Address
		for _, a := range raw {
			ip, ok := toAddr(a.IP)
			if !ok || !ip.Is6() || ip.Is4In6() {
				continue
			}
			ones, _ := a.Mask.Size()
			addrs = append(addrs, Address{
				Addr:      ip,
				PrefixLen: ones,
				Scope:     scopeName(a.Scope),
				Flags:     addrFlagNames(a.Flags),
				Permanent: a.Flags&unix.IFA_F_PERMANENT != 0,
			})
		}
		hasGlobal := false
		for _, a := range addrs {
			if a.Scope == "global" {
				hasGlobal = true
				break
			}
		}
		if !hasGlobal {
			continue
		}
		sort.Slice(addrs, func(i, j int) bool { return addrs[i].Addr.Less(addrs[j].Addr) })

		it := Interface{
			Name:         attrs.Name,
			Index:        attrs.Index,
			Up:           attrs.Flags&net.FlagUp != 0,
			MAC:          attrs.HardwareAddr.String(),
			PointToPoint: attrs.Flags&net.FlagPointToPoint != 0,
			Addresses:    addrs,
			Prefixes:     CollectPrefixes(addrs),
		}
		if r := pickDefaultRoute(routes, attrs.Index, attrs.Name); r != nil {
			it.Route = r
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// pickDefaultRoute 从全表里挑出这块网卡的 ::/0 路由，metric 小的优先。
func pickDefaultRoute(routes []netlink.Route, index int, name string) *Route {
	var best *Route
	for i := range routes {
		r := routes[i]
		if r.LinkIndex != index {
			continue
		}
		if r.Dst != nil && !r.Dst.IP.Equal(net.IPv6zero) {
			continue
		}
		if r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		cur := &Route{Dev: name, Metric: r.Priority, OnLink: r.Flags&int(unix.RTNH_F_ONLINK) != 0}
		if gw, ok := toAddr(r.Gw); ok && r.Gw != nil {
			cur.Gateway = gw
		}
		if src, ok := toAddr(r.Src); ok && r.Src != nil {
			cur.Source = src
		}
		if best == nil || cur.Metric < best.Metric {
			best = cur
		}
	}
	return best
}

func linkByName(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("找不到网卡 %s: %w", name, err)
	}
	return link, nil
}

func toIPNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{
		IP:   net.IP(p.Addr().AsSlice()),
		Mask: net.CIDRMask(p.Bits(), 128),
	}
}

func (m linuxManager) AddAddr(iface string, p netip.Prefix) error {
	link, err := linkByName(iface)
	if err != nil {
		return err
	}
	// NOPREFIXROUTE 避免每加一个地址就往路由表塞一条重复的 on-link 前缀路由。
	addr := &netlink.Addr{IPNet: toIPNet(p), Flags: unix.IFA_F_NOPREFIXROUTE}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%s 已经在 %s 上了: %w", p, iface, ErrAddrExists)
		}
		return fmt.Errorf("添加 %s 到 %s 失败: %w", p, iface, err)
	}
	return nil
}

func (m linuxManager) DelAddr(iface string, p netip.Prefix) error {
	link, err := linkByName(iface)
	if err != nil {
		return err
	}
	addr := &netlink.Addr{IPNet: toIPNet(p)}
	if err := netlink.AddrDel(link, addr); err != nil {
		if errors.Is(err, unix.EADDRNOTAVAIL) || errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%s 不在 %s 上: %w", p, iface, ErrAddrNotFound)
		}
		return fmt.Errorf("从 %s 删除 %s 失败: %w", iface, p, err)
	}
	return nil
}

// waitSourceUsable 等源地址过完 DAD。
//
// 刚 add 上去的地址会有一两秒处于 tentative，这期间内核不认它当路由源地址，
// RouteReplace 会直接回 EINVAL。这个坑只有在"加完立刻设出口"时才会踩到，
// 地址放一会儿再设就没事，所以必须在这里等一下而不是把错误抛给用户。
func waitSourceUsable(link netlink.Link, src netip.Addr, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			return fmt.Errorf("读取网卡地址失败: %w", err)
		}
		found := false
		for _, a := range addrs {
			ip, ok := toAddr(a.IP)
			if !ok || ip != src {
				continue
			}
			found = true
			if a.Flags&unix.IFA_F_DADFAILED != 0 {
				return fmt.Errorf("%s 的重复地址检测没过，这个地址在同网段被别人占了", src)
			}
			if a.Flags&unix.IFA_F_TENTATIVE == 0 {
				return nil
			}
		}
		if !found {
			return fmt.Errorf("%s 不在 %s 上", src, link.Attrs().Name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s 等了 %s 还没通过重复地址检测", src, timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (m linuxManager) SetDefaultRoute(iface string, gw, src netip.Addr, onLink bool) error {
	link, err := linkByName(iface)
	if err != nil {
		return err
	}
	if !src.IsValid() {
		return errors.New("源地址无效")
	}
	if err := waitSourceUsable(link, src, 8*time.Second); err != nil {
		return err
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		Src:       net.IP(src.AsSlice()),
		Family:    netlink.FAMILY_V6,
	}
	// 隧道这类点对点链路没有网关，下发成 `default dev xxx` 就行；
	// 硬要带 Gw 反而会被内核拒掉。
	if gw.IsValid() {
		route.Gw = net.IP(gw.AsSlice())
		if onLink {
			route.Flags = int(unix.RTNH_F_ONLINK)
		}
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("设置默认出口失败: %w", err)
	}
	return nil
}
