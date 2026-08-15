// Package proxy is the intercepting HTTP and TLS proxy that enforces egress
// rules and substitutes secret placeholders.
//
// Domain rules work for HTTP and TLS only. A raw TCP port carries no hostname,
// so a domain rule on one must be refused at create time, never silently
// ignored.
//
// Chunk 4 fills this package in.
package proxy
