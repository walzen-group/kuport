package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
)

// BindConfig says where the metrics endpoint listens. The agent runs with
// hostNetwork, so it never listens on a wildcard address: a wildcard would
// open the endpoint on every node address, a public uplink included.
type BindConfig struct {
	// Bind is empty for the node's InternalIP, an interface name such as wt0
	// for every address on it but link-local ones, or "off".
	Bind string
	// HostIP is the node's InternalIP, from the downward API's status.hostIP.
	HostIP string
	Port   uint16
}

// ResolveAddrs turns a BindConfig into the addresses to listen on. "off" gives
// none. lookup lists an interface's addresses.
func ResolveAddrs(c BindConfig, lookup func(string) ([]netip.Addr, error)) ([]netip.AddrPort, error) {
	switch c.Bind {
	case "off":
		return nil, nil
	case "":
		a, err := netip.ParseAddr(c.HostIP)
		if err != nil {
			return nil, fmt.Errorf("HOST_IP %q is not an address; the DaemonSet sets it from status.hostIP", c.HostIP)
		}
		if a.IsUnspecified() {
			return nil, fmt.Errorf("HOST_IP %s is a wildcard address", a)
		}
		return []netip.AddrPort{netip.AddrPortFrom(a.Unmap(), c.Port)}, nil
	}
	if _, err := netip.ParseAddr(c.Bind); err == nil {
		return nil, fmt.Errorf("--metrics-bind %q is an address; it takes an interface name or off", c.Bind)
	}
	addrs, err := lookup(c.Bind)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", c.Bind, err)
	}
	var out []netip.AddrPort
	for _, a := range addrs {
		a = a.Unmap()
		if a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, netip.AddrPortFrom(a, c.Port))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("interface %s has no address to listen on", c.Bind)
	}
	return out, nil
}

// HostAddrs lists the addresses on a named interface, both families.
func HostAddrs(name string) ([]netip.Addr, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}
	list, err := netlink.AddrList(l, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range list {
		if ip, ok := netip.AddrFromSlice(a.IP); ok {
			out = append(out, ip.Unmap())
		}
	}
	return out, nil
}

// Server serves Handler on the addresses Config resolves to. A missing address
// or a failed bind never stops the agent: the Server logs it once and retries
// every Retry until it listens, while the datapath keeps reconciling.
type Server struct {
	Config  BindConfig
	Lookup  func(string) ([]netip.Addr, error)
	Handler http.Handler
	Log     logr.Logger
	Retry   time.Duration // 30s when zero

	mu    sync.Mutex
	bound []net.Addr
}

// Bound lists the addresses the Server listens on, empty before it binds.
func (s *Server) Bound() []net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]net.Addr(nil), s.bound...)
}

// NeedLeaderElection is false: every agent serves its own node's metrics.
func (s *Server) NeedLeaderElection() bool { return false }

// Start binds, retrying until it can, then serves until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	if s.Config.Bind == "off" {
		s.Log.Info("metrics endpoint off")
		<-ctx.Done()
		return nil
	}
	retry := s.Retry
	if retry == 0 {
		retry = 30 * time.Second
	}
	var lns []net.Listener
	failing := false
	for {
		var err error
		lns, err = s.listen()
		if err == nil {
			break
		}
		if !failing {
			s.Log.Error(err, "metrics endpoint not listening; retrying", "every", retry)
			failing = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}

	srv := &http.Server{Handler: s.Handler, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		s.Log.Info("metrics endpoint listening", "address", ln.Addr().String())
		go func() { errs <- srv.Serve(ln) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			s.Log.Error(err, "metrics endpoint stopped")
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// listen resolves the addresses and opens one listener on each, or none.
func (s *Server) listen() ([]net.Listener, error) {
	addrs, err := ResolveAddrs(s.Config, s.Lookup)
	if err != nil {
		return nil, err
	}
	var lns []net.Listener
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a.String())
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return nil, err
		}
		lns = append(lns, ln)
	}
	s.mu.Lock()
	for _, ln := range lns {
		s.bound = append(s.bound, ln.Addr())
	}
	s.mu.Unlock()
	return lns, nil
}
