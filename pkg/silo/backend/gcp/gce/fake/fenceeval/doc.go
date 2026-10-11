// Package fenceeval is the fence evaluator (docs/silo/testing.md "Fake
// GCE backend", SV-119). The fake GCE doesn't move packets. Instead, an
// Evaluator takes a network's firewall policy and its VPC firewall
// rules and answers "is a packet from A to B, on this protocol and
// port, allowed?" the way GCE's firewall would. Tests of the fence
// compiler use it in place of real packets, and the conformance suite
// can run it against the real policy (Load) to check that it agrees
// with GCE.
//
// CheckSteps is the other half: given a policy and a rule-by-rule diff,
// it checks that no intermediate state is looser than either end
// (gcp-backend.md "Ordering").
//
// # The GCE behaviour it relies on
//
// Each point below is one the conformance suite should check against
// real GCE. Where GCE's documentation is silent, the choice made is
// said.
//
//   - Enforcement order. A network's
//     networkFirewallPolicyEnforcementOrder decides whether its global
//     network firewall policy is evaluated before its VPC firewall
//     rules (BEFORE_CLASSIC_FIREWALL, which Silo uses) or after them
//     (AFTER_CLASSIC_FIREWALL, GCE's default). Hierarchical and
//     regional policies are not modelled.
//     https://cloud.google.com/firewall/docs/firewall-policies-overview#rule-evaluation
//     https://cloud.google.com/firewall/docs/network-firewall-policies#change-enforcement-order
//   - Policy rules are evaluated in priority order, lowest number
//     first; priorities are unique within a policy, and the first rule
//     that matches decides. allow and deny are final. goto_next hands
//     the packet on to the next stage: the VPC firewall rules (with
//     BEFORE_CLASSIC_FIREWALL) or the implied rules (with
//     AFTER_CLASSIC_FIREWALL). A packet no policy rule matches goes on
//     as with goto_next; GCE's own default rules (priorities
//     2147483644 to 2147483647) are goto_next for exactly that reason.
//     Disabled rules are skipped.
//     https://cloud.google.com/firewall/docs/firewall-policy-rules
//   - A policy rule matches when the packet's direction is the rule's,
//     its source is in srcIpRanges and its destination in
//     destIpRanges, and some layer4Config matches its protocol and
//     port. An empty range list puts no limit on that end; GCE asks
//     for a source on ingress rules and a destination on egress rules,
//     which is the fake's and the compiler's business, not the
//     evaluator's. Policy rules here have no targets, so they apply to
//     every VM in the network: a rule with target service accounts,
//     secure tags or resources, or any matcher other than IP ranges and
//     layer4Configs (address groups, FQDNs, geolocation, threat
//     intelligence, network types, secure tags), is refused by New.
//   - VPC firewall rules are evaluated in priority order, lowest number
//     first (0 to 65535, 1000 when not given). Where an allow and a
//     deny rule of the same priority both match, deny wins. Disabled
//     rules are skipped.
//     https://cloud.google.com/firewall/docs/firewalls#priority_order_for_firewall_rules
//   - A VPC rule with no targetTags applies to every VM in the network,
//     and one with targetTags to the VMs carrying at least one of them.
//     An ingress rule's source is sourceRanges or sourceTags (either
//     matching is enough); sourceTags match only packets sent by a VM
//     in the network carrying one of the tags. An ingress rule with
//     neither has sourceRanges 0.0.0.0/0, and an egress rule with no
//     destinationRanges has 0.0.0.0/0, as GCE fills in. destinationRanges
//     on ingress and sourceRanges on egress, when given, narrow the
//     match further. Service accounts are not modelled (the facade
//     refuses them) and New refuses a rule using them.
//     https://cloud.google.com/firewall/docs/firewalls#firewall_rule_components
//     https://cloud.google.com/firewall/docs/firewalls#sources-for-rules
//   - The implied rules: GCE allows egress and denies ingress that no
//     rule decided. They are the last stage in either order.
//     https://cloud.google.com/firewall/docs/firewalls#default_firewall_rules
//   - Egress rules are enforced at the sending VM and ingress rules at
//     the receiving VM, and a packet between two VMs must be allowed by
//     both. A packet to something that is not a VM NIC in the network
//     (the internet, a Private Service Connect endpoint, a Google API)
//     is decided by the sender's egress alone, and a packet from one by
//     the receiver's ingress alone: Endpoint.External says which.
//     https://cloud.google.com/firewall/docs/firewalls#specifications
//   - Firewalls are stateful: a connection allowed one way lets its
//     replies back. The evaluator answers for the packet that opens a
//     connection, and says nothing about replies.
//   - Protocols are given by name (tcp, udp, icmp, esp, ah, sctp, ipip)
//     or by IP protocol number, "all" in a rule matching every one.
//     Ports, single or a range "lo-hi", apply to tcp, udp and sctp; a
//     protocol entry with no ports matches every port.
//     https://cloud.google.com/firewall/docs/firewalls#protocols_and_ports
//
// The metadata server, spoofing and Google APIs (gcp-backend.md "What
// the fence does not cover") are outside the evaluator, as they are
// outside the firewall.
package fenceeval
