package hub

// Bonjour (docs/api.md, "Finding the hub (Bonjour)"): the hub announces itself on the LAN, so an app finds it
// without anyone typing a URL. In Kubernetes the multicast reaches the LAN only with hostNetwork (docs/deploy.md).

import (
	"encoding/xml"
	"fmt"
	"log"
	"net"
	"strings"

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

// AvahiService is the announcement as an avahi static service file (/etc/avahi/services/*.service), for nodes
// that already run avahi: the node's daemon announces the hub, so the pod needs no hostNetwork. The chart writes
// it with an init container (mdns.avahi, docs/deploy.md). port is what the record carries; apps use the TXT url.
func AvahiService(name, publicURL string, port int) []byte {
	if name == "" {
		name = mdnsDefaultName
	}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" standalone='no'?>\n")
	b.WriteString("<!DOCTYPE service-group SYSTEM \"avahi-service.dtd\">\n")
	b.WriteString("<!-- written by ai-connector-server avahi-service; replaced on every start of the hub -->\n")
	b.WriteString("<service-group>\n")
	fmt.Fprintf(&b, "  <name>%s</name>\n", xmlText(name))
	b.WriteString("  <service>\n")
	fmt.Fprintf(&b, "    <type>%s</type>\n", mdnsService)
	fmt.Fprintf(&b, "    <port>%d</port>\n", port)
	for _, txt := range mdnsTXT(publicURL) {
		fmt.Fprintf(&b, "    <txt-record>%s</txt-record>\n", xmlText(txt))
	}
	b.WriteString("  </service>\n")
	b.WriteString("</service-group>\n")
	return []byte(b.String())
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
