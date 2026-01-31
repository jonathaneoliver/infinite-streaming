package proxy

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// New returns a reverse proxy handler to the upstream upload service.
func New(rawURL string) http.Handler {
	target, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("invalid UPLOAD_SERVICE_URL: %v", err)
	}

	p := httputil.NewSingleHostReverseProxy(target)
	originalDirector := p.Director
	p.Director = func(req *http.Request) {
		originalDirector(req)
		// Ensure Host header matches upstream for correct routing.
		req.Host = target.Host
	}
	return p
}
