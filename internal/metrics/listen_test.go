package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestResolveAddrs(t *testing.T) {
	wt0 := []netip.Addr{
		netip.MustParseAddr("100.64.215.50"),
		netip.MustParseAddr("fd00:64::32"),
		netip.MustParseAddr("fe80::1"), // link-local, left out
	}
	lookup := func(name string) ([]netip.Addr, error) {
		switch name {
		case "wt0":
			return wt0, nil
		case "empty0":
			return []netip.Addr{netip.MustParseAddr("fe80::2")}, nil
		}
		return nil, errors.New("Link not found")
	}
	tests := []struct {
		name    string
		cfg     BindConfig
		want    []netip.AddrPort
		wantErr bool
	}{
		{name: "host ip default", cfg: BindConfig{HostIP: "100.64.215.50", Port: 9790},
			want: []netip.AddrPort{netip.MustParseAddrPort("100.64.215.50:9790")}},
		{name: "interface", cfg: BindConfig{Bind: "wt0", Port: 9790},
			want: []netip.AddrPort{netip.MustParseAddrPort("100.64.215.50:9790"), netip.MustParseAddrPort("[fd00:64::32]:9790")}},
		{name: "off", cfg: BindConfig{Bind: "off", HostIP: "100.64.215.50", Port: 9790}},
		{name: "no host ip", cfg: BindConfig{Port: 9790}, wantErr: true},
		{name: "wildcard host ip", cfg: BindConfig{HostIP: "0.0.0.0", Port: 9790}, wantErr: true},
		{name: "address as bind", cfg: BindConfig{Bind: "0.0.0.0", Port: 9790}, wantErr: true},
		{name: "unknown interface", cfg: BindConfig{Bind: "wt9", Port: 9790}, wantErr: true},
		{name: "interface with only link-local", cfg: BindConfig{Bind: "empty0", Port: 9790}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveAddrs(tt.cfg, lookup)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("addrs = %v, want %v", got, tt.want)
			}
			for _, a := range got {
				if a.Addr().IsUnspecified() {
					t.Errorf("resolved a wildcard address %s", a)
				}
			}
		})
	}
}

// TestServerRetriesUntilAddressAppears: the interface has no address for two
// lookups, then gets one, and the endpoint answers.
func TestServerRetriesUntilAddressAppears(t *testing.T) {
	var calls atomic.Int32
	lookup := func(string) ([]netip.Addr, error) {
		if calls.Add(1) <= 2 {
			return nil, errors.New("Link not found")
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	s := &Server{
		Config:  BindConfig{Bind: "wt0", Port: 0},
		Lookup:  lookup,
		Handler: mux,
		Log:     logr.Discard(),
		Retry:   10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	deadline := time.Now().Add(time.Second)
	for len(s.Bound()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not listening after a second; %d lookups", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get("http://" + s.Bound()[0].String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if n := calls.Load(); n < 3 {
		t.Errorf("lookups = %d, want at least 3", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v", err)
	}
}
