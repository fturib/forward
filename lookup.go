// Package forward implements a forwarding proxy. It caches an upstream net.Conn for some time, so if the same
// client returns the upstream's Conn will be precached. Depending on how you benchmark this looks to be
// 50% faster than just openening a new connection for every client. It works with UDP and TCP and uses
// inband healthchecking.
package forward

import (
	"log"

	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
)

// Forward forward the request in state as-is. Unlike Lookup that adds EDNS0 suffix to the message.
func (f Forward) Forward(state request.Request) (*dns.Msg, error) {
	for _, proxy := range f.list() {
		if proxy.Down(f.maxfails) {
			continue
		}

		ret, err := proxy.connect(state, f.forceTCP, true)
		if err != nil {
			log.Printf("[WARNING] Failed to connect %s: %s", proxy.host.addr, err)
			continue
		}

		return ret, nil
	}
	return nil, errNoHealthy
}

// Lookup will use name and type to forge a new message and will send that upstream. It will
// set any EDNS0 options correctly so that downstream will be able to process the reply.
func (f Forward) Lookup(state request.Request, name string, typ uint16) (*dns.Msg, error) {
	req := new(dns.Msg)
	req.SetQuestion(name, typ)
	state.SizeAndDo(req)

	state2 := request.Request{W: state.W, Req: req}

	return f.Forward(state2)
}
