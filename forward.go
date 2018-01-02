// Package forward implements a forwarding proxy. It caches an upstream net.Conn for some time, so if the same
// client returns the upstream's Conn will be precached. Depending on how you benchmark this looks to be
// 50% faster than just openening a new connection for every client. It works with UDP and TCP and uses
// inband healthchecking.
package forward

import (
	"crypto/tls"
	"errors"
	"log"
	"math/rand"
	"strconv"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

// Forward represents a plugin instance that can proxy requests to another (DNS) server. It has a list
// of proxies each representing one upstream proxy.
type Forward struct {
	proxies []*proxy

	from    string
	ignored []string

	tlsConfig     *tls.Config
	tlsServerName string
	maxfails      uint32
	expire        time.Duration

	forceTCP   bool          // also here for testing
	hcInterval time.Duration // also here for testing

	Next plugin.Handler
}

// Len returns the number of configured proxies.
func (f Forward) Len() int { return len(f.proxies) }

// Name implements plugin.Handler.
func (f Forward) Name() string { return "forward" }

// ServeDNS implements plugin.Handler.
func (f Forward) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {

	state := request.Request{W: w, Req: r}
	if !f.match(state) {
		return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
	}

	for _, proxy := range f.list() {
		if proxy.Down(f.maxfails) {
			continue
		}

		start := time.Now()

		proto := state.Proto()
		if f.forceTCP {
			proto = "tcp"
		}
		if proxy.host.tlsConfig != nil {
			proto = "tcp-tls"
		}

		conn, err := proxy.Dial(proto)
		if err != nil {
			log.Printf("[WARNING] Failed to connect with %s to %s: %s", proto, proxy.host.addr, err)
			continue
		}

		// Set buffer size correctly for this client.
		conn.UDPSize = uint16(state.Size())
		if conn.UDPSize < 512 {
			conn.UDPSize = 512
		}

		conn.SetWriteDeadline(time.Now().Add(timeout))
		if err := conn.WriteMsg(state.Req); err != nil {
			log.Printf("[WARNING] Failed to write with %s to %s: %s", proto, proxy.host.addr, err)
			conn.Close() // not giving it back
			continue
		}

		conn.SetReadDeadline(time.Now().Add(timeout))
		ret, err := conn.ReadMsg()
		if err != nil {
			log.Printf("[WARNING] Failed to read with %s to %s: %s", proto, proxy.host.addr, err)
			conn.Close() // not giving it back
			continue
		}

		w.WriteMsg(ret)

		proxy.Yield(conn)

		rc, ok := dns.RcodeToString[ret.Rcode]
		if !ok {
			rc = strconv.Itoa(ret.Rcode)
		}

		RequestCount.WithLabelValues(proxy.host.addr).Add(1)
		RcodeCount.WithLabelValues(rc, proxy.host.addr).Add(1)
		RequestDuration.WithLabelValues(proxy.host.addr).Observe(time.Since(start).Seconds())

		return 0, nil
	}

	return dns.RcodeServerFailure, errNoHealthy
}

func (f Forward) match(state request.Request) bool {
	from := f.from

	if !plugin.Name(from).Matches(state.Name()) || !f.isAllowedDomain(state.Name()) {
		return false
	}

	return true
}

func (f Forward) isAllowedDomain(name string) bool {
	if dns.Name(name) == dns.Name(f.from) {
		return true
	}

	for _, ignore := range f.ignored {
		if plugin.Name(ignore).Matches(name) {
			return false
		}
	}
	return true
}

// list returns a randomized set of proxies to be used for this client. If the client was
// know to any of the proxies it will be put first.
func (f Forward) list() []*proxy {
	switch len(f.proxies) {
	case 1:
		return f.proxies
	case 2:
		if rand.Int()%2 == 0 {
			return []*proxy{f.proxies[1], f.proxies[0]} // swap

		}
		return f.proxies // normal
	}

	perms := rand.Perm(len(f.proxies))
	rnd := make([]*proxy, len(f.proxies))

	for i, p := range perms {
		rnd[i] = f.proxies[p]
	}
	return rnd
}

var (
	errInvalidDomain = errors.New("invalid domain for proxy")
	errNoHealthy     = errors.New("no healthy proxies")
)
