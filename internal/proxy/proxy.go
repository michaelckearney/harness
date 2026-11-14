// Package proxy is how a bot reaches a model without holding its key. The
// service listens on the loopback address; each agent is given the proxy's
// address as its provider's base URL and a token of the service's as its
// API key. The proxy checks the token, puts the real key in its place, and
// forwards the request. The keys stay in files only the service's user can
// read.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

// Route is one upstream, served under /<Name>/.
type Route struct {
	Name    string
	BaseURL string
	// KeyFile holds the key; "" sends none.
	KeyFile string
	// Beta, if set, is added to the request's anthropic-beta header: a
	// Claude subscription's token works only with the OAuth beta.
	Beta string
}

// Proxy forwards the agents' requests to their models.
type Proxy struct {
	// Token is what an agent sends as its API key.
	Token string
	// URL is where the proxy listens, such as http://127.0.0.1:43123.
	URL    string
	routes map[string]Route
	srv    *http.Server
}

// Start listens on a free loopback port.
func Start(routes []Route) (*Proxy, error) {
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{Token: "harness-" + hex.EncodeToString(tok), URL: "http://" + ln.Addr().String(), routes: map[string]Route{}}
	for _, r := range routes {
		if r.BaseURL == "" {
			continue
		}
		if _, err := url.Parse(r.BaseURL); err != nil {
			ln.Close()
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		p.routes[r.Name] = r
	}
	p.srv = &http.Server{Handler: p}
	go p.srv.Serve(ln)
	return p, nil
}

// Base is the base URL an agent uses for a route.
func (p *Proxy) Base(route string) string { return p.URL + "/" + route }

// Close stops the proxy.
func (p *Proxy) Close() error { return p.srv.Shutdown(context.Background()) }

func (p *Proxy) authorized(r *http.Request) bool {
	got := r.Header.Get("x-api-key")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.Token)) == 1
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "harness proxy: unknown token", http.StatusUnauthorized)
		return
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	route, ok := p.routes[name]
	if !ok {
		http.Error(w, "harness proxy: no route "+name, http.StatusNotFound)
		return
	}
	key := ""
	if route.KeyFile != "" {
		data, err := os.ReadFile(route.KeyFile)
		if err != nil {
			http.Error(w, "harness proxy: cannot read the key for "+name, http.StatusBadGateway)
			return
		}
		key = strings.TrimSpace(string(data))
	}
	target, _ := url.Parse(route.BaseURL)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + "/" + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("x-api-key")
			if key != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+key)
			}
			if route.Beta != "" {
				pr.Out.Header.Set("anthropic-beta", addBeta(pr.Out.Header.Get("anthropic-beta"), route.Beta))
			}
		},
		// Models stream their replies.
		FlushInterval: -1,
	}
	rp.ServeHTTP(w, r)
}

// addBeta adds beta to a comma-separated anthropic-beta header.
func addBeta(header, beta string) string {
	if header == "" {
		return beta
	}
	for _, b := range strings.Split(header, ",") {
		if strings.TrimSpace(b) == beta {
			return header
		}
	}
	return header + "," + beta
}

// OAuthBeta is the anthropic-beta flag a Claude subscription's token needs.
const OAuthBeta = "oauth-2025-04-20"
