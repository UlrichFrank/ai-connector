package hub

// Bonjour (docs/api.md, "Finding the hub (Bonjour)"): the hub announces itself on the LAN, so an app finds it
// without anyone typing a URL. In Kubernetes the multicast reaches the LAN only with hostNetwork (docs/deploy.md).

import (
	"log"
	"net"

	"github.com/grandcat/zeroconf"
)

const (
	mdnsService = "_ai-connector._tcp"
	mdnsDomain  = "local."
	// mdnsDefaultName is the instance name without MLXLAB_MDNS_NAME.
	mdnsDefaultName = "AI Connector"
)

// mdnsTXT is the announcement's TXT record: url (the address apps should use, e.g. the https ingress name rather
// than the announced ip:port) when there is a public URL, then role and path.
func mdnsTXT(publicURL string) []string {
	var txt []string
	if publicURL != "" {
		txt = append(txt, "url="+publicURL)
	}
	return append(txt, "role=hub", "path=/")
}

// announce registers the hub on the port of addr and returns the func that withdraws it. A failure only logs:
// the hub serves without being found.
func (s *Server) announce(addr net.Addr) (stop func()) {
	port := 8080
	if a, ok := addr.(*net.TCPAddr); ok && a.Port != 0 {
		port = a.Port
	}
	name := s.cfg.MDNSName
	if name == "" {
		name = mdnsDefaultName
	}
	srv, err := zeroconf.Register(name, mdnsService, mdnsDomain, port, mdnsTXT(s.cfg.PublicURL), nil)
	if err != nil {
		log.Printf("bonjour: not announced: %v", err)
		return func() {}
	}
	log.Printf("bonjour: announced %q as %s on port %d", name, mdnsService, port)
	return srv.Shutdown
}
