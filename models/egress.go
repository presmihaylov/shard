package models

// Action is what a rule does with what it matches.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
)

// DestinationKind says how a rule names where a packet is going.
type DestinationKind string

const (
	// DestinationCIDR is an address or a prefix, as 1.2.3.4 or 10.0.0.0/8.
	DestinationCIDR DestinationKind = "cidr"
	// DestinationDomain is a host name, resolved when the policy is applied; a wildcard label (*.example.com, www.*.com, or * alone) is matched by the proxy only.
	DestinationDomain DestinationKind = "domain"
	// DestinationDomainSuffix is a host name and everything under it. Only the proxy can match it.
	DestinationDomainSuffix DestinationKind = "domain-suffix"
	// DestinationGroup is a named set of prefixes; any is the only one.
	DestinationGroup DestinationKind = "group"
)

// Destination is where a rule applies.
type Destination struct {
	Kind  DestinationKind `json:"kind"`
	Value string          `json:"value"`
}

// Rule is one line of a policy. Protocol empty means every protocol, and then Ports is empty too.
type Rule struct {
	Action      Action      `json:"action"`
	Destination Destination `json:"destination"`
	Protocol    string      `json:"protocol,omitempty"`
	Ports       []int       `json:"ports,omitempty"`
}

// Policy is an ordered list of rules. The first match wins, and what matches nothing is dropped.
type Policy struct {
	Name  string `json:"name"`
	Rules []Rule `json:"rules"`
}
