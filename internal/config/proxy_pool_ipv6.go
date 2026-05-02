package config

import (
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"strings"
)

const maxIPv6BindRangeEntries = 4096

// IPv6BindLease is an on-demand proxy entry allocated from a lease CIDR.
type IPv6BindLease struct {
	Pool      string
	EntryName string
	IP        string
	URL       string
}

func expandIPv6BindRangeEntries(ranges []IPv6BindRange, existing []ProxyPoolEntry) []ProxyPoolEntry {
	if len(ranges) == 0 {
		return nil
	}
	seenNames := make(map[string]struct{}, len(existing))
	seenURLs := make(map[string]struct{}, len(existing))
	for _, entry := range existing {
		name := strings.ToLower(strings.TrimSpace(entry.Name))
		if name != "" {
			seenNames[name] = struct{}{}
		}
		url := strings.TrimSpace(entry.URL)
		if url != "" {
			seenURLs[url] = struct{}{}
		}
	}

	out := make([]ProxyPoolEntry, 0)
	for _, rng := range ranges {
		start, end, ok := normalizeIPv6BindRange(rng)
		if !ok {
			continue
		}
		forward := ipv6AddrToBigInt(start)
		limit := ipv6AddrToBigInt(end)
		if forward.Cmp(limit) > 0 {
			continue
		}
		rangeSize := new(big.Int).Sub(limit, forward)
		rangeSize.Add(rangeSize, big.NewInt(1))
		if rangeSize.Cmp(big.NewInt(maxIPv6BindRangeEntries)) > 0 {
			continue
		}
		count := 0
		for current := new(big.Int).Set(forward); current.Cmp(limit) <= 0; current = new(big.Int).Add(current, big.NewInt(1)) {
			addr, ok := bigIntToIPv6Addr(current)
			if !ok {
				break
			}
			url := fmt.Sprintf("bind://[%s]", addr.String())
			if _, ok := seenURLs[url]; ok {
				count++
				continue
			}
			name := uniqueProxyPoolEntryName(resolveIPv6BindRangeName(rng.NamePrefix, count+1), seenNames)
			entry := ProxyPoolEntry{
				Name:             name,
				URL:              url,
				Weight:           rng.Weight,
				runtimeGenerated: true,
			}
			if entry.Weight <= 0 {
				entry.Weight = 1
			}
			out = append(out, entry)
			seenNames[strings.ToLower(name)] = struct{}{}
			seenURLs[url] = struct{}{}
			count++
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeIPv6BindRange(rng IPv6BindRange) (netip.Addr, netip.Addr, bool) {
	if rng.Disabled {
		return netip.Addr{}, netip.Addr{}, false
	}
	startText := strings.TrimSpace(rng.Start)
	if startText == "" {
		return netip.Addr{}, netip.Addr{}, false
	}
	start, err := netip.ParseAddr(startText)
	if err != nil || !start.Is6() {
		return netip.Addr{}, netip.Addr{}, false
	}
	endText := strings.TrimSpace(rng.End)
	if endText == "" {
		return start, start, true
	}
	end, err := netip.ParseAddr(endText)
	if err != nil || !end.Is6() {
		return netip.Addr{}, netip.Addr{}, false
	}
	return start, end, true
}

// AllocateIPv6BindLease returns the lowest free IPv6 address from the pool's
// lease CIDRs without expanding the full CIDR. occupied keys must be canonical
// IPv6 strings.
func AllocateIPv6BindLease(pool *ProxyPool, authSeed string, occupied map[string]struct{}) (IPv6BindLease, bool) {
	if pool == nil || len(pool.IPv6BindLeaseRanges) == 0 {
		return IPv6BindLease{}, false
	}
	for rangeIndex, rng := range pool.IPv6BindLeaseRanges {
		prefix, ok := normalizeIPv6BindLeaseRange(rng)
		if !ok {
			continue
		}
		addr, ok := firstFreeIPv6InPrefix(prefix, occupied)
		if !ok {
			continue
		}
		ip := addr.String()
		entryName := ipv6BindLeaseEntryName(rng.NamePrefix, authSeed, rangeIndex+1, ip)
		return IPv6BindLease{
			Pool:      pool.Name,
			EntryName: entryName,
			IP:        ip,
			URL:       fmt.Sprintf("bind://[%s]", ip),
		}, true
	}
	return IPv6BindLease{}, false
}

// IPv6BindLeasePoolContains reports whether ip belongs to any enabled lease
// CIDR on the pool.
func IPv6BindLeasePoolContains(pool *ProxyPool, ip string) bool {
	if pool == nil || strings.TrimSpace(ip) == "" {
		return false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !addr.Is6() {
		return false
	}
	for _, rng := range pool.IPv6BindLeaseRanges {
		prefix, ok := normalizeIPv6BindLeaseRange(rng)
		if !ok {
			continue
		}
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func normalizeIPv6BindLeaseRange(rng IPv6BindLeaseRange) (netip.Prefix, bool) {
	if rng.Disabled {
		return netip.Prefix{}, false
	}
	cidr := strings.TrimSpace(rng.CIDR)
	if cidr == "" {
		return netip.Prefix{}, false
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is6() {
		return netip.Prefix{}, false
	}
	return prefix.Masked(), true
}

func firstFreeIPv6InPrefix(prefix netip.Prefix, occupied map[string]struct{}) (netip.Addr, bool) {
	start := ipv6AddrToBigInt(prefix.Addr())
	bits := prefix.Bits()
	if bits < 0 || bits > 128 {
		return netip.Addr{}, false
	}
	size := new(big.Int).Lsh(big.NewInt(1), uint(128-bits))
	limit := new(big.Int).Add(start, size)
	for current := new(big.Int).Set(start); current.Cmp(limit) < 0; current.Add(current, big.NewInt(1)) {
		addr, ok := bigIntToIPv6Addr(current)
		if !ok {
			return netip.Addr{}, false
		}
		if _, exists := occupied[addr.String()]; !exists {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

func ipv6BindLeaseEntryName(prefix, authSeed string, rangeIndex int, ip string) string {
	cleanPrefix := strings.TrimSpace(prefix)
	if cleanPrefix == "" {
		cleanPrefix = "bind-v6-lease-"
	}
	seed := strings.TrimSpace(authSeed)
	if seed == "" {
		seed = strings.ReplaceAll(ip, ":", "-")
	}
	seed = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, seed)
	seed = strings.Trim(seed, "-_")
	if seed == "" {
		seed = strconv.Itoa(rangeIndex)
	}
	if len(seed) > 48 {
		seed = seed[:48]
	}
	return cleanPrefix + seed
}

func resolveIPv6BindRangeName(prefix string, index int) string {
	cleanPrefix := strings.TrimSpace(prefix)
	if cleanPrefix == "" {
		cleanPrefix = "bind-v6-"
	}
	if index < 1 {
		index = 1
	}
	return cleanPrefix + strconv.Itoa(index)
}

func uniqueProxyPoolEntryName(base string, seen map[string]struct{}) string {
	cleanBase := strings.TrimSpace(base)
	if cleanBase == "" {
		cleanBase = "proxy"
	}
	key := strings.ToLower(cleanBase)
	if _, ok := seen[key]; !ok {
		return cleanBase
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", cleanBase, i)
		key = strings.ToLower(candidate)
		if _, ok := seen[key]; !ok {
			return candidate
		}
	}
}

func ipv6AddrToBigInt(addr netip.Addr) *big.Int {
	var raw [16]byte
	raw = addr.As16()
	return new(big.Int).SetBytes(raw[:])
}

func bigIntToIPv6Addr(v *big.Int) (netip.Addr, bool) {
	if v == nil || v.Sign() < 0 {
		return netip.Addr{}, false
	}
	bytes := v.Bytes()
	if len(bytes) > 16 {
		return netip.Addr{}, false
	}
	var raw [16]byte
	copy(raw[16-len(bytes):], bytes)
	addr := netip.AddrFrom16(raw)
	return addr, true
}
