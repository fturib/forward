package forward

import (
	"crypto/tls"
	"net"
	"strconv"
	"time"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnsutil"
	pkgtls "github.com/coredns/coredns/plugin/pkg/tls"

	"github.com/mholt/caddy"
)

func init() {
	caddy.RegisterPlugin("forward", caddy.Plugin{
		ServerType: "dns",
		Action:     setup,
	})
}

func setup(c *caddy.Controller) error {
	f, err := parseForward(c)
	if err != nil {
		return err
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		f.Next = next
		return f
	})

	c.OnStartup(func() error {
		return f.OnStartup()
	})
	c.OnShutdown(func() error {
		return f.OnShutdown()
	})

	return nil
}

// OnStartup starts a goroutines for all proxies.
func (f *Forward) OnStartup() (err error) {
	for _, p := range f.proxies {
		go p.healthCheck()
	}
	return nil
}

// OnShutdown stops all configures proxies.
func (f *Forward) OnShutdown() error {
	for _, p := range f.proxies {
		p.close()
	}
	return nil
}

func parseForward(c *caddy.Controller) (Forward, error) {
	f := Forward{maxfails: 2, tlsConfig: new(tls.Config), expire: 10 * time.Second}

	protocols := map[int]int{}

	for c.Next() {
		if !c.Args(&f.from) {
			return f, c.ArgErr()
		}
		f.from = plugin.Host(f.from).Normalize()

		to := c.RemainingArgs()
		if len(to) == 0 {
			return f, c.ArgErr()
		}

		// A bit fiddly, but first check if we've got protocols and if so add them back in when we create the proxies.
		protocols = make(map[int]int)
		for i := range to {
			protocols[i], to[i] = protocol(to[i])
		}

		// If parseHostPortOrFile expands a file with a lot of nameserver our accounting in protocols doesn't make
		// any sense anymore... For now: lets don't care.
		toHosts, err := dnsutil.ParseHostPortOrFile(to...)
		if err != nil {
			return f, err
		}

		for i, h := range toHosts {
			// Double check the port, if e.g. is 53 and the transport is TLS make it 853.
			// This can be somewhat annoying because you *can't* have TLS on port 53 then.
			switch protocols[i] {
			case TLS:
				h1, p, err := net.SplitHostPort(h)
				if err != nil {
					break
				}

				if p == "53" {
					h = net.JoinHostPort(h1, "853")
				}
			}

			// We can't set tlsConfig here, because we haven't parsed it yet.
			// We set it below at the end of parseBlock.
			p := newProxy(h)
			f.proxies = append(f.proxies, p)
		}

		for c.NextBlock() {
			if err := parseBlock(c, &f); err != nil {
				return f, err
			}
		}
	}

	if f.tlsServerName != "" {
		f.tlsConfig.ServerName = f.tlsServerName
	}
	for i := range f.proxies {
		// Only set this for proxies that need it.
		if protocols[i] == TLS {
			f.proxies[i].SetTLSConfig(f.tlsConfig)
		}
		f.proxies[i].SetExpire(f.expire)
	}
	return f, nil
}

func parseBlock(c *caddy.Controller, f *Forward) error {
	switch c.Val() {
	case "except":
		ignore := c.RemainingArgs()
		if len(ignore) == 0 {
			return c.ArgErr()
		}
		for i := 0; i < len(ignore); i++ {
			ignore[i] = plugin.Host(ignore[i]).Normalize()
		}
		f.ignored = ignore
	case "max_fails":
		if !c.NextArg() {
			return c.ArgErr()
		}
		n, err := strconv.Atoi(c.Val())
		if err != nil {
			return err
		}
		f.maxfails = uint32(n)
	case "health_check":
		if !c.NextArg() {
			return c.ArgErr()
		}
		dur, err := time.ParseDuration(c.Val())
		if err != nil {
			return err
		}
		for i := range f.proxies {
			f.proxies[i].hcInterval = dur
		}
	case "force_tcp":
		if c.NextArg() {
			return c.ArgErr()
		}
		f.forceTCP = true
		for i := range f.proxies {
			f.proxies[i].forceTCP = true
		}
	case "tls":
		args := c.RemainingArgs()
		if len(args) != 3 {
			return c.ArgErr()
		}

		tlsConfig, err := pkgtls.NewTLSConfig(args[0], args[1], args[2])
		if err != nil {
			return err
		}
		f.tlsConfig = tlsConfig
	case "tls_servername":
		if !c.NextArg() {
			return c.ArgErr()
		}
		f.tlsServerName = c.Val()
	case "expire":
		if !c.NextArg() {
			return c.ArgErr()
		}
		dur, err := time.ParseDuration(c.Val())
		if err != nil {
			return err
		}
		f.expire = dur

	default:
		return c.Errf("unknown property '%s'", c.Val())
	}

	return nil
}
