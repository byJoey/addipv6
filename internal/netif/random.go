package netif

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
)

// hostMask 算出第 byteIndex 个字节里哪几位归主机部分。
func hostMask(byteIndex, prefixBits int) byte {
	start := byteIndex * 8
	switch {
	case start >= prefixBits:
		return 0xff
	case start+8 <= prefixBits:
		return 0x00
	default:
		return byte(0xff) >> (prefixBits - start)
	}
}

// hostAllZero 判断主机位是不是全 0，也就是 Subnet-Router anycast 地址。
func hostAllZero(a [16]byte, prefixBits int) bool {
	for i := 0; i < 16; i++ {
		if m := hostMask(i, prefixBits); m != 0 && a[i]&m != 0 {
			return false
		}
	}
	return true
}

// reservedAnycastGeneric 命中 RFC 2526 里「子网内最高 128 个地址」这条通用规则，
// 也就是主机位除最低 7 位之外全是 1。主机位不足 8 位时这条规则不适用。
func reservedAnycastGeneric(a [16]byte, prefixBits int) bool {
	if 128-prefixBits < 8 {
		return false
	}
	for i := 0; i < 16; i++ {
		m := hostMask(i, prefixBits)
		if m == 0 {
			continue
		}
		want := m
		if i == 15 {
			want &^= 0x7f // 最低 7 位是 anycast ID，取值随意
		}
		if a[i]&want != want {
			return false
		}
	}
	return true
}

// reservedAnycastEUI64 命中 RFC 2526 给 EUI-64 接口标识符留的那段
// fdff:ffff:ffff:ff80/121，只在 /64 子网里成立。
func reservedAnycastEUI64(a [16]byte, prefixBits int) bool {
	if prefixBits != 64 {
		return false
	}
	if a[8] != 0xfd {
		return false
	}
	for i := 9; i <= 14; i++ {
		if a[i] != 0xff {
			return false
		}
	}
	return a[15]&0x80 == 0x80
}

// Reserved 判断一个地址在给定子网里是否属于不该分配的保留地址。
func Reserved(p netip.Prefix, a netip.Addr) bool {
	b := a.As16()
	bits := p.Bits()
	return hostAllZero(b, bits) || reservedAnycastGeneric(b, bits) || reservedAnycastEUI64(b, bits)
}

// RandomAddr 在 p 里随机挑一个可用地址，跳过网段地址和 anycast 保留段。
func RandomAddr(p netip.Prefix) (netip.Addr, error) {
	p = p.Masked()
	if !p.Addr().Is6() || p.Addr().Is4In6() {
		return netip.Addr{}, errors.New("只支持 IPv6 前缀")
	}
	if 128-p.Bits() < 2 {
		return netip.Addr{}, ErrHostBitsTooSmall
	}

	base := p.Addr().As16()
	var buf [16]byte
	for attempt := 0; attempt < 64; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return netip.Addr{}, fmt.Errorf("读取随机数失败: %w", err)
		}
		out := base
		for i := 0; i < 16; i++ {
			out[i] |= buf[i] & hostMask(i, p.Bits())
		}
		a := netip.AddrFrom16(out)
		if Reserved(p, a) {
			continue
		}
		return a, nil
	}
	return netip.Addr{}, errors.New("连续 64 次都撞上保留地址，请换个前缀")
}

// GenerateBatch 一次生成 count 个互不重复、且避开 exclude 的地址。
func GenerateBatch(p netip.Prefix, count int, exclude map[netip.Addr]struct{}) ([]netip.Addr, error) {
	if count <= 0 {
		return nil, errors.New("数量必须大于 0")
	}
	if exclude == nil {
		exclude = map[netip.Addr]struct{}{}
	}
	seen := make(map[netip.Addr]struct{}, count)
	out := make([]netip.Addr, 0, count)
	// 主机位少的时候碰撞频繁，给足重试预算；位数大时几乎一次就中。
	budget := count*16 + 1024
	for len(out) < count && budget > 0 {
		budget--
		a, err := RandomAddr(p)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[a]; dup {
			continue
		}
		if _, dup := exclude[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) < count {
		return out, fmt.Errorf("前缀 %s 里可用地址不够，只生成了 %d 个", p, len(out))
	}
	return out, nil
}
