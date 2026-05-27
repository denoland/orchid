// Minimal cfrelaytun client. Exposes a local HTTP server at a public
// relay URL.
//
//	go run . -url wss://tun.example.com/agent -token $TOKEN -addr :3000
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"syscall"

	"github.com/divy/orchid/cfrelaytun/go/cfrelaytun"
)

func main() {
	relayURL := flag.String("url", "", "wss:// URL of the relay /agent endpoint")
	token := flag.String("token", "", "agent token")
	addr := flag.String("addr", "", "upstream local address to proxy (e.g. :3000)")
	flag.Parse()

	if *relayURL == "" || *token == "" || *addr == "" {
		flag.Usage()
		log.Fatal("missing flags")
	}

	target, _ := url.Parse("http://" + addr2host(*addr))
	proxy := httputil.NewSingleHostReverseProxy(target)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := cfrelaytun.Run(ctx, cfrelaytun.Config{
		URL:     *relayURL,
		Token:   *token,
		Handler: http.HandlerFunc(proxy.ServeHTTP),
		Logger:  log.Printf,
	})
	if err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}

func addr2host(a string) string {
	if len(a) > 0 && a[0] == ':' {
		return "127.0.0.1" + a
	}
	return a
}
