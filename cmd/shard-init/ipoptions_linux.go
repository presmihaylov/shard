package main

import (
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// ipOptionsDrop is `ip ihl > 5 ip protocol tcp drop`, as nft compiles it.
var ipOptionsDrop = []expr.Any{
	&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 0, Len: 1},
	&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x0f}, Xor: []byte{0x00}},
	&expr.Cmp{Op: expr.CmpOpGt, Register: 1, Data: []byte{5}},
	&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 9, Len: 1},
	&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
	&expr.Verdict{Kind: expr.VerdictDrop},
}

// dropIPOptions drops IPv4 TCP with options before the stack parses them, which no real traffic sets (CVE-2026-98432).
func dropIPOptions() error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	table := conn.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "shard"})
	chain := conn.AddChain(&nftables.Chain{
		Name:     "ipoptions",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityRaw,
	})
	conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: ipOptionsDrop})
	if err := conn.Flush(); err != nil {
		return fmt.Errorf("add the IP options drop: %w", err)
	}

	return nil
}
