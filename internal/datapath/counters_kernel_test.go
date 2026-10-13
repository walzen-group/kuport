package datapath

import (
	"context"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// TestReadLiveSkipsOtherTablesOnRealKernel applies an empty State beside a
// table another program owns, holding a counter in a forward chain the way a
// node's firewall does. Every counted chain of table inet kuport is empty, so
// ReadLive returns no rule.
func TestReadLiveSkipsOtherTablesOnRealKernel(t *testing.T) {
	inNetns(t, "TestReadLiveSkipsOtherTablesOnRealKernel", readLiveSkipsOtherTables)
}

func readLiveSkipsOtherTables(t *testing.T) {
	conn, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables: %v", err)
	}
	for _, fam := range []nftables.TableFamily{nftables.TableFamilyINet, nftables.TableFamilyIPv4} {
		other := conn.AddTable(&nftables.Table{Name: "other", Family: fam})
		fwd := conn.AddChain(&nftables.Chain{
			Name: "forward", Table: other, Type: nftables.ChainTypeFilter,
			Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter,
		})
		conn.AddRule(&nftables.Rule{Table: other, Chain: fwd, Exprs: []expr.Any{&expr.Counter{}}})
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("add the other tables: %v", err)
	}
	if err := Apply(context.Background(), Render(State{}), Handles{NFT: conn, NL: realNetlink{}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	live, err := ReadLive(conn)
	if err != nil {
		t.Fatalf("read live counters: %v", err)
	}
	for _, l := range live {
		t.Errorf("ReadLive returned a rule from an empty kuport table: %+v", l)
	}
}
