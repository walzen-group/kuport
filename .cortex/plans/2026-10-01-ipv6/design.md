# IPv6 delivery

Status: agreed with the admin on 2026-10-01 and built the same day in commits
cb48171 to d842615. The unit, kernel-namespace and envtest tiers pass; no IPv6
packet has crossed a cluster yet. Decisions beyond this design are in
docs/decisions.md and docs/datapath.md.

## Goal

Deliver a mapping over IPv6 as well as IPv4, keeping the client's IPv6 source
address, on a dual-stack cluster. The first user is the infra repo's prod
cluster, dual-stack since 2026-10-01 (pods in `fd9f:6e92:f8f3::/56`, one /64 per
node), where a public worker answers on its own IPv6 address.

## What turns IPv6 on for a mapping

The Service the PortMap names. kuport reads the mapping's endpoint from the
Service's EndpointSlices, and today it skips every slice that is not IPv4
(internal/reconcile/endpoints.go). A Service with
`ipFamilyPolicy: PreferDualStack` has an IPv6 EndpointSlice as well; kuport
then programs both families, to the same pod, matched by the endpoint's
targetRef. A SingleStack Service keeps exactly today's rules. The PortMap API
gains no field.

## Changes

| Area | Today | With IPv6 |
| --- | --- | --- |
| nftables table | `table ip kuport` (nft.go, TableFamilyIPv4) | `table inet kuport`, the same three chains, hooks and priorities. On start the agent deletes a leftover `table ip kuport` |
| accepting DNAT | `iifname X <proto> dport P dnat to <v4>` | one rule per family the endpoint has: `meta nfproto ipv4 … dnat ip to <v4>`, `meta nfproto ipv6 … dnat ip6 to <v6>` |
| rules matching an address | `ip saddr` / `ip daddr` | `ip` or `ip6` by the address's family, preceded by the `meta nfproto` match an inet table needs before a payload load |
| identity SNAT (exempt) | `snat to ip saddr` | per family: `snat ip to ip saddr`, `snat ip6 to ip6 saddr` |
| return link device | one IPv4 /31 from `returnPath.vxlan.subnet` | also one IPv6 /127 from the new `returnPath.vxlan.subnet6`, at the same slot |
| return path routing | `ip rule` fwmark → table 200+slot, route via the peer's IPv4 end | the same mark and table in `ip -6 rule`, a route via the peer's IPv6 end |
| loop guard | `ip rule` to the peer's underlay /32 | unchanged: the underlay stays IPv4, so only IPv4 packets can loop |
| endpoint | first ready IPv4 address | the IPv4 endpoint as today, plus the same pod's IPv6 address when an IPv6 slice exists |
| status.published | the interface's IPv4 address | also the interface's first global IPv6 address, when it has one |

The VXLAN underlay stays IPv4, between the nodes' InternalIPv4 addresses.
VXLAN carries Ethernet frames, so an inner IPv6 packet rides it unchanged.
The link MTU is the underlay less 50, which at a 1350 underlay is 1300, above
IPv6's minimum of 1280.

## API

`PortMapClass.spec.returnPath.vxlan.subnet6`, string, default
`fd64:f5ac:e961::/112`. The 40 bits after `fd` were generated with
`od -An -N5 -tx1 /dev/urandom` on 2026-10-01, per RFC 4193. Validation rejects a
prefix that is not IPv6, and a prefix holding fewer /127s than `subnet` holds
/31s, since both are indexed by the same slot.

## Host requirements

IPv6 forwarding on every node. Cilium with IPv6 on sets
`net.ipv6.conf.all.forwarding` to 1; the agent's host check reads it, and
reports IPv6 delivery as unavailable on a node where it is 0, while IPv4 keeps
working.

## Not in scope

- An IPv6 underlay for the return links.
- IPv6 on an interface that holds no IPv6 address. The per-family DNAT rules
  match the interface and port, so they are harmless there; no IPv6 packet
  arrives, and status publishes no IPv6 address for it.

## Tests

- Goldens: every rule kind in both families, the inet table header, the
  family-specific DNAT and SNAT targets.
- Reconcile: a SingleStack Service yields today's State byte for byte; a
  PreferDualStack Service yields both families to the same pod; an IPv6 slice
  whose pod has no IPv4 endpoint is ignored.
- Links: slot n gets the nth /127 of subnet6; both ends agree.
- Netlink: IPv6 address on the link, `ip -6` rule and route, reconcile removes
  stale IPv6 objects.
- Migration: an agent starting next to a `table ip kuport` deletes it.
- On the cluster: the udp-echo probe over IPv6 to a node's netbird IPv6 address
  on wt0, from an admin peer with netbird IPv6, Single and Multi, pod on the
  same node and on another node.
