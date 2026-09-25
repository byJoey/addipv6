//go:build !linux

package netif

import (
	"net/netip"
	"sync"
)

// demoManager 让界面能在 macOS 之类的机器上跑起来看效果，所有写操作都被拒绝。
type demoManager struct {
	mu    sync.Mutex
	addrs []Address
}

// New 返回当前平台的网卡管理器。
func New() Manager {
	m := &demoManager{}
	for _, s := range []string{
		"2001:db8:1234:5678::1/64",
		"2001:db8:1234:5678:a1b2:c3d4:e5f6:0789/64",
		"fe80::1c2d:3e4f:5a6b:7c8d/64",
	} {
		p := netip.MustParsePrefix(s)
		scope := "global"
		if p.Addr().IsLinkLocalUnicast() {
			scope = "link"
		}
		m.addrs = append(m.addrs, Address{
			Addr:      p.Addr(),
			PrefixLen: p.Bits(),
			Scope:     scope,
			Flags:     []string{"permanent"},
			Permanent: true,
		})
	}
	return m
}

func (m *demoManager) Platform() string { return "demo" }

func (m *demoManager) List() ([]Interface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addrs := append([]Address(nil), m.addrs...)
	gw := netip.MustParseAddr("2001:db8:1234:5678::ffff")
	return []Interface{{
		Name: "demo0",

		Index:     2,
		Up:        true,
		MAC:       "00:16:3e:00:00:01",
		Addresses: addrs,
		Prefixes:  CollectPrefixes(addrs),
		Route:     &Route{Gateway: gw, Source: addrs[0].Addr, Dev: "demo0", OnLink: true},
	}}, nil
}

func (m *demoManager) AddAddr(string, netip.Prefix) error { return ErrNotSupported }
func (m *demoManager) DelAddr(string, netip.Prefix) error { return ErrNotSupported }
func (m *demoManager) SetDefaultRoute(string, netip.Addr, netip.Addr, bool) error {
	return ErrNotSupported
}
