package netif

import (
	"net/netip"
	"testing"
)

func TestRandomAddrStaysInPrefix(t *testing.T) {
	p := netip.MustParsePrefix("2001:db8:1:2::/64")
	for i := 0; i < 500; i++ {
		a, err := RandomAddr(p)
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if !p.Contains(a) {
			t.Fatalf("%s 跑到前缀 %s 外面去了", a, p)
		}
		if a == p.Addr() {
			t.Fatalf("不该生成网段地址本身")
		}
	}
}

func TestReservedRecognisesAnycast(t *testing.T) {
	p := netip.MustParsePrefix("2001:db8::/64")
	reserved := []string{
		"2001:db8::",                    // Subnet-Router anycast
		"2001:db8::ffff:ffff:ffff:ff80", // 通用形式的最高 128 个
		"2001:db8::ffff:ffff:ffff:ffff",
		"2001:db8::fdff:ffff:ffff:ff80", // EUI-64 形式
		"2001:db8::fdff:ffff:ffff:ffff",
	}
	for _, s := range reserved {
		if !Reserved(p, netip.MustParseAddr(s)) {
			t.Fatalf("%s 应该被判为保留地址", s)
		}
	}
	ok := []string{"2001:db8::1", "2001:db8::fdff:ffff:ffff:ff7f", "2001:db8::abcd"}
	for _, s := range ok {
		if Reserved(p, netip.MustParseAddr(s)) {
			t.Fatalf("%s 不该被判为保留地址", s)
		}
	}
}

func TestRandomAddrSkipsReserved(t *testing.T) {
	// /120 只有 256 个地址，最高 128 个是保留段，随机结果必须全落在低半区。
	p := netip.MustParsePrefix("2001:db8::ff00/120")
	for i := 0; i < 3000; i++ {
		a, err := RandomAddr(p)
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		last := a.As16()[15]
		if last == 0 || last >= 0x80 {
			t.Fatalf("%s 落进了保留段", a)
		}
	}
}

func TestRandomAddrRejectsTinyPrefix(t *testing.T) {
	if _, err := RandomAddr(netip.MustParsePrefix("2001:db8::1/128")); err == nil {
		t.Fatal("/128 应该被拒绝")
	}
	if _, err := RandomAddr(netip.MustParsePrefix("2001:db8::/127")); err == nil {
		t.Fatal("/127 主机位不够，应该被拒绝")
	}
}

func TestGenerateBatchUniqueAndExcludes(t *testing.T) {
	p := netip.MustParsePrefix("2001:db8:aa::/64")
	blocked := netip.MustParseAddr("2001:db8:aa::1234")
	exclude := map[netip.Addr]struct{}{blocked: {}}
	got, err := GenerateBatch(p, 200, exclude)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(got) != 200 {
		t.Fatalf("想要 200 个，拿到 %d", len(got))
	}
	seen := map[netip.Addr]bool{}
	for _, a := range got {
		if seen[a] {
			t.Fatalf("出现重复地址 %s", a)
		}
		if a == blocked {
			t.Fatalf("排除列表里的地址还是被生成了")
		}
		seen[a] = true
	}
}

func TestGenerateBatchSmallPrefixExhausts(t *testing.T) {
	// /124 只有 16 个地址，扣掉网段地址最多 15 个，要 40 个必然不够。
	p := netip.MustParsePrefix("2001:db8::/124")
	if _, err := GenerateBatch(p, 40, nil); err == nil {
		t.Fatal("地址不够时应该报错")
	}
}

func TestCollectPrefixesDedups(t *testing.T) {
	addrs := []Address{
		{Addr: netip.MustParseAddr("2001:db8::1"), PrefixLen: 64, Scope: "global"},
		{Addr: netip.MustParseAddr("2001:db8::2"), PrefixLen: 64, Scope: "global"},
		{Addr: netip.MustParseAddr("fe80::1"), PrefixLen: 64, Scope: "link"},
		{Addr: netip.MustParseAddr("2001:db8:1::1"), PrefixLen: 48, Scope: "global"},
		{Addr: netip.MustParseAddr("2001:db8:2::9"), PrefixLen: 128, Scope: "global"},
	}
	got := CollectPrefixes(addrs)
	if len(got) != 2 {
		t.Fatalf("想要 2 个网段，拿到 %d: %v", len(got), got)
	}
	if got[0].Bits() != 48 {
		t.Fatalf("短前缀该排前面，拿到 %v", got)
	}
}

func TestParsePrefix(t *testing.T) {
	p, err := ParsePrefix("2001:db8::5", 64)
	if err != nil || p.Bits() != 64 {
		t.Fatalf("裸地址该补默认前缀长度: %v %v", p, err)
	}
	if _, err := ParsePrefix("192.0.2.1", 64); err == nil {
		t.Fatal("IPv4 该被拒绝")
	}
	if _, err := ParsePrefix("::ffff:192.0.2.1", 64); err == nil {
		t.Fatal("v4-mapped 该被拒绝")
	}
}
