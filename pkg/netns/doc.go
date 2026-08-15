// Package netns manipulates Linux network namespaces, veth pairs, bridges and
// NAT rules.
//
// Host netfilter is the policy of record. gVisor netstack iptables do NOT
// survive checkpoint/restore, so they are defence in depth only, and the caller
// must re-apply them after every restore.
//
// SHARD-13 fills this package in.
package netns
