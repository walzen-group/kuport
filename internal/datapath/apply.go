// Package datapath renders one node's desired kuport state into an nftables
// ruleset and a set of netlink links, rules and routes, and applies it. It is
// the only package in kuport that touches the host, and within it only nft.go,
// netlink.go and apply.go do; render.go, text.go and state.go stay pure so the
// reconcile above can be tested without a cluster or root.
package datapath

import (
	"context"
	"net"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
)

// NFTConn is the slice of *nftables.Conn that Apply and Teardown use. Taking an
// interface keeps the pure rendering testable against a fake handle, with no
// root and no kernel.
type NFTConn interface {
	AddTable(t *nftables.Table) *nftables.Table
	FlushTable(t *nftables.Table)
	AddChain(c *nftables.Chain) *nftables.Chain
	AddRule(r *nftables.Rule) *nftables.Rule
	DelTable(t *nftables.Table)
	Flush() error
}

// NetlinkConn is the slice of the netlink package that Apply and Teardown use,
// again an interface so the reconcile can run against a fake.
type NetlinkConn interface {
	LinkByName(name string) (netlink.Link, error)
	LinkList() ([]netlink.Link, error)
	LinkAdd(link netlink.Link) error
	LinkDel(link netlink.Link) error
	LinkSetUp(link netlink.Link) error
	AddrAdd(link netlink.Link, addr *netlink.Addr) error
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	RuleAdd(rule *netlink.Rule) error
	RuleDel(rule *netlink.Rule) error
	RuleList(family int) ([]netlink.Rule, error)
	RouteAdd(route *netlink.Route) error
	RouteDel(route *netlink.Route) error
	RouteListFiltered(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error)
}

// underlaySetter is an optional capability a NetlinkConn may provide:
// re-pointing an existing VXLAN's endpoints in place. realNetlink does;
// implementations that do not (a fake, or a future handle the kernel would
// refuse) simply take the rebuild path, which converges on the same state.
// It is deliberately not part of NetlinkConn: widening that seam would
// force the change onto every consumer's fake across the packages this one
// feeds.
type underlaySetter interface {
	LinkSetUnderlay(idx int, name string, local, remote net.IP) error
}

// Handles bundle the two host connections Apply and Teardown need.
type Handles struct {
	NFT NFTConn
	NL  NetlinkConn
}

// NewHandles opens real connections to nftables and netlink. It is the one path
// that needs NET_ADMIN; tests supply their own fakes instead.
func NewHandles() (Handles, error) {
	conn, err := nftables.New()
	if err != nil {
		return Handles{}, err
	}
	return Handles{NFT: conn, NL: realNetlink{}}, nil
}

// Apply makes the host match the Plan. nftables is rewritten in one transaction,
// so a deleted mapping disappears by absence. netlink has no transaction, so
// links, rules and routes are reconciled by comparison: what is missing is
// added, what is stale is updated in place, and what kuport finds under its
// own names and no longer wants is removed. Applying the same Plan twice
// changes nothing on the second pass.
//
// F10: links are brought current before the nftables commit. A failure in the
// link stage then leaves the old ruleset and no new path: the node refuses
// rather than DNATs to a remote pod whose return link is not yet live. After
// the commit, links exist for the rules to use, so the only half-programmed
// window left is rules-over-a-live-link, which the level-driven requeue
// closes in seconds.
func Apply(ctx context.Context, plan Plan, h Handles) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := applyLinks(h.NL, plan.Links); err != nil {
		return err
	}
	if err := applyNFT(h.NFT, plan); err != nil {
		return err
	}
	if err := applyRules(h.NL, plan.IPRules); err != nil {
		return err
	}
	return applyRoutes(h.NL, plan.Routes)
}

// Teardown removes everything kuport owns on this node: the nftables table, the
// kup-* links, and kuport's routes and routing rules. A node taken out of a
// class stops holding state nobody wants; a crashed agent leaves these and the
// next Apply reconciles them away instead.
func Teardown(ctx context.Context, h Handles) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := teardownNetlink(h.NL); err != nil {
		return err
	}
	return teardownNFT(h.NFT)
}
