//go:build linux

package netns

// supported gates every verb: this driver runs iproute2 and nft, which only Linux has.
const supported = true
