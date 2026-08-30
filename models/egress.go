package models

import "time"

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
	// DestinationDomain is one host name, resolved when the policy is applied.
	DestinationDomain DestinationKind = "domain"
	// DestinationDomainSuffix is a host name and everything under it. Only the proxy can match it.
	DestinationDomainSuffix DestinationKind = "domain-suffix"
	// DestinationGroup is a named set of prefixes: private or any.
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

// Where an egress decision was made.
const (
	EgressSourceProxy = "proxy"
	EgressSourceHost  = "host"
)

// EgressEvent is one decision on one request or flow. Rule is the index into the sandbox's effective
// rules, as shard inspect prints them, or what stood in for one: none, private, missing, default, resolve.
type EgressEvent struct {
	Time        time.Time `json:"time"`
	Sandbox     string    `json:"sandbox"`
	Source      string    `json:"source"`
	Verdict     Action    `json:"verdict"`
	Protocol    string    `json:"protocol,omitempty"`
	Destination string    `json:"destination"`
	Address     string    `json:"address,omitempty"`
	Rule        string    `json:"rule"`
	RuleText    string    `json:"rule_text,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}
