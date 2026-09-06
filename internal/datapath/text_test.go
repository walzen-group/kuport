package datapath

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// TestGolden renders each case's nftables ruleset and compares it to its golden
// file. With -update it rewrites the goldens instead. The goldens are syntax
// checked against the real parser separately, in the acceptance gate.
func TestGolden(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			got := Render(c.state).String()
			path := filepath.Join("testdata", c.name+".golden")

			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run go test -update to create): %v", err)
			}
			if got != string(want) {
				t.Errorf("ruleset mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", c.name, got, want)
			}
		})
	}
}

// TestNFTText pins the exact text of one rule of each kind, so a change to the
// renderer is visible here and not only in a whole-ruleset diff.
func TestNFTText(t *testing.T) {
	dst := pod
	src := pod
	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{
			name: "dnat single",
			rule: Rule{Chain: ChainPre, Kind: KindDNAT, IifName: "enp1s0", Port: PortSel{"udp", 3000, 3000}, ToAddr: pod},
			want: `iifname "enp1s0" udp dport 3000 counter dnat to 10.244.18.107:3000`,
		},
		{
			name: "dnat range",
			rule: Rule{Chain: ChainPre, Kind: KindDNAT, IifName: "wt0", Port: PortSel{"udp", 27015, 27115}, ToAddr: pod},
			want: `iifname "wt0" udp dport 27015-27115 counter dnat to 10.244.18.107:27015-27115`,
		},
		{
			name: "accepting exemption",
			rule: Rule{Chain: ChainPost, Kind: KindSNAT, OifName: "cilium_host", DstAddr: &dst, Port: PortSel{"udp", 3000, 3000}},
			want: `oifname "cilium_host" ip daddr 10.244.18.107 udp dport 3000 counter snat to ip saddr`,
		},
		{
			name: "reply exemption",
			rule: Rule{Chain: ChainPost, Kind: KindSNAT, OifName: "cilium_*", OifNeg: true, SrcAddr: &src, Port: PortSel{"udp", 3000, 3000}, PortIsSrc: true},
			want: `oifname != "cilium_*" ip saddr 10.244.18.107 udp sport 3000 counter snat to ip saddr`,
		},
		{
			name: "mark",
			rule: Rule{Chain: ChainMangle, Kind: KindMark, SrcAddr: &src, Port: PortSel{"udp", 3000, 3000}, PortIsSrc: true, Mark: 0x6b700001},
			want: `ip saddr 10.244.18.107 udp sport 3000 counter meta mark set 0x6b700001`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nftText(tt.rule); got != tt.want {
				t.Errorf("nftText:\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// TestTableHeaders confirms the three chains render with their pinned base-chain
// headers and in the fixed order.
func TestTableHeaders(t *testing.T) {
	got := Render(State{}).String()
	for _, want := range []string{
		"table ip kuport {",
		"chain kup-pre {",
		"type nat hook prerouting priority dstnat - 10; policy accept;",
		"chain kup-post {",
		"type nat hook postrouting priority srcnat - 10; policy accept;",
		"chain kup-mangle {",
		"type filter hook prerouting priority mangle + 10; policy accept;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("empty ruleset missing %q:\n%s", want, got)
		}
	}
}
