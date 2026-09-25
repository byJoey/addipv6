// Package netif 负责枚举网卡、增删 IPv6 地址、读写默认路由。
// 具体实现按平台分文件：Linux 走 netlink，其它平台是只读的演示桩。
package netif

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Address 是网卡上的一条 IPv6 地址。
type Address struct {
	Addr      netip.Addr `json:"addr"`
	PrefixLen int        `json:"prefixLen"`
	Scope     string     `json:"scope"`
	Flags     []string   `json:"flags"`
	// Permanent 为 false 通常意味着这是 SLAAC 临时地址或租期地址，重启会变。
	Permanent bool `json:"permanent"`
}

// Prefix 返回 addr/prefixLen 形式的网段。
func (a Address) Prefix() netip.Prefix {
	return netip.PrefixFrom(a.Addr, a.PrefixLen)
}

func (a Address) String() string {
	return fmt.Sprintf("%s/%d", a.Addr, a.PrefixLen)
}

// Interface 是一块有全局 IPv6 的网卡。
type Interface struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
	Up    bool   `json:"up"`
	MAC   string `json:"mac"`
	// PointToPoint 为真表示这是隧道这类点对点链路（比如 6in4 的 sit 接口）。
	// 这种链路的默认路由本来就没有网关，形如 `default dev ipv6net`。
	PointToPoint bool           `json:"pointToPoint"`
	Addresses    []Address      `json:"addresses"`
	Prefixes     []netip.Prefix `json:"prefixes"`
	Route        *Route         `json:"route,omitempty"`
}

// Route 是这块网卡上的 IPv6 默认路由。
type Route struct {
	Gateway netip.Addr `json:"gateway"`
	Source  netip.Addr `json:"source"`
	Dev     string     `json:"dev"`
	OnLink  bool       `json:"onLink"`
	Metric  int        `json:"metric"`
}

// Manager 抽象掉平台差异，方便在非 Linux 上跑界面。
type Manager interface {
	// Platform 返回 "linux" 或 "demo"，界面据此决定要不要挂演示模式横幅。
	Platform() string
	List() ([]Interface, error)
	AddAddr(iface string, p netip.Prefix) error
	DelAddr(iface string, p netip.Prefix) error
	SetDefaultRoute(iface string, gw, src netip.Addr, onLink bool) error
}

var (
	// ErrNotSupported 表示当前平台做不了这个写操作。
	ErrNotSupported = errors.New("当前平台不支持该操作，只有 Linux 能改网卡地址")
	// ErrHostBitsTooSmall 表示前缀太长，主机位不够随机。
	ErrHostBitsTooSmall = errors.New("前缀主机位不足，无法随机生成新地址")
	// ErrAddrExists 表示地址已经在网卡上了。恢复流程靠它判断"这条不用再加"，
	// 所以必须是个可以 errors.Is 的哨兵，不能让调用方去匹配错误文案。
	ErrAddrExists = errors.New("地址已经在网卡上")
	// ErrAddrNotFound 表示要删的地址本来就不在网卡上。
	ErrAddrNotFound = errors.New("地址不在网卡上")
)

// Find 按名字挑一块网卡。
func Find(list []Interface, name string) (Interface, bool) {
	for _, it := range list {
		if it.Name == name {
			return it, true
		}
	}
	return Interface{}, false
}

// CollectPrefixes 把网卡上的全局地址归并成互不重复的可用网段。
func CollectPrefixes(addrs []Address) []netip.Prefix {
	seen := map[netip.Prefix]struct{}{}
	var out []netip.Prefix
	for _, a := range addrs {
		if a.Scope != "global" || a.PrefixLen >= 127 {
			continue
		}
		p := netip.PrefixFrom(a.Addr, a.PrefixLen).Masked()
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bits() != out[j].Bits() {
			return out[i].Bits() < out[j].Bits()
		}
		return out[i].Addr().Less(out[j].Addr())
	})
	return out
}

// ParsePrefix 兼容 "2001:db8::1/64" 和裸地址两种写法，裸地址按 defaultBits 补。
func ParsePrefix(s string, defaultBits int) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Prefix{}, errors.New("地址为空")
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if !p.Addr().Is6() {
			return netip.Prefix{}, errors.New("不是 IPv6 地址: " + s)
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !a.Is6() || a.Is4In6() {
		return netip.Prefix{}, errors.New("不是 IPv6 地址: " + s)
	}
	return netip.PrefixFrom(a, defaultBits), nil
}
