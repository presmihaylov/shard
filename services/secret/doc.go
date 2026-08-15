// Package secret stores host-level secrets and their destination grants.
//
// A secret is granted to a DESTINATION, never to a sandbox alone. Otherwise a
// sandbox allowed to reach two hosts sends the placeholder to the wrong one and
// the proxy hands over the real value.
//
// A sandbox references a secret by name and never holds a value. Never log a
// value, and never write one into a state file.
//
// SHARD-72 fills this package in.
package secret
