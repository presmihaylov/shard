package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/presmihaylov/shard/models"
)

// LinkPort is where the link forwarder listens on the gateway, beside the proxy; the host rules dnat a sandbox's flows to its links onto it.
const LinkPort = 30022

// maxLinkConns bounds one sandbox's open link flows, since each holds daemon descriptors the way a proxied one does (SHARD-350).
const maxLinkConns = 1024

// LinkSource says which link addresses each sandbox reaches; nil is no link anywhere.
type LinkSource interface {
	Linked(ctx context.Context) ([]Linked, error)
}

// Linked is one sandbox, by its address, and the link addresses it reaches through the forwarder.
type Linked struct {
	Source  netip.Addr
	Targets []netip.Addr
}

// readLinks reads every sandbox's links and refuses one the rules could not scope: a source off the subnet, or a target outside the link range.
func readLinks(ctx context.Context, source LinkSource, subnet netip.Prefix) ([]Linked, error) {
	if source == nil {
		return nil, nil
	}

	linked, err := source.Linked(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the sandbox links: %w", err)
	}

	for _, l := range linked {
		if !subnet.Contains(l.Source) {
			return nil, fmt.Errorf("the links of %s name a source outside the sandbox subnet %s", l.Source, subnet)
		}
		for _, target := range l.Targets {
			if !models.LinkSubnet.Contains(target) {
				return nil, fmt.Errorf("the links of %s name %s, outside the link range %s", l.Source, target, models.LinkSubnet)
			}
		}
	}

	return linked, nil
}

func joinAddrs(addrs []netip.Addr) string {
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		parts = append(parts, addr.String())
	}

	return strings.Join(parts, ", ")
}
