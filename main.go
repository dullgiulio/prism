package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
)

func makeSelfTestServers() (string, string) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "this call was relayed by the reverse proxy\n\n")
		if _, err := io.Copy(w, r.Body); err != nil {
			log.Fatalf("cannot copy body in backend server: %v", err)
		}
	}))
	mirrorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "mirror server\n\n")
		if _, err := io.Copy(w, r.Body); err != nil {
			log.Fatalf("cannot copy body in mirror server: %v", err)
		}
	}))
	return backendServer.URL, mirrorServer.URL
}

func handleSigterm(stop func()) <-chan struct{} {
	done := make(chan struct{})
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	go func() {
		var called bool
		for range c {
			if called {
				continue
			}
			called = true
			stop()
			close(done)
		}
	}()
	return done
}

func prefixEnv(prefix string, getenv func(string) string) func(*flag.Flag) {
	prefix = prefix + "_"
	return func(f *flag.Flag) {
		key := prefix + strings.Replace(strings.ToUpper(f.Name), "-", "_", -1)
		val := getenv(key)
		if val == "" {
			return
		}
		if err := f.Value.Set(val); err != nil {
			log.Fatalf("error: cannot set flag from environment variable %s: %v", key, err)
		}
	}
}

func main() {
	health := flag.String("health", ":7979", "Listen IP:PORT for health check, empty to disable")
	listen := flag.String("listen", ":8080", "Listen to this IP:PORT")
	proxyHost := flag.String("proxy", "", "Host to reverse proxy")
	rawMirrors := flag.String("mirrors", "", "Comma separated list of endpoints to mirror requests to")
	selfTest := flag.Bool("self-test", false, "Self test mode")
	insecure := flag.Bool("insecure", false, "Allow invalid TLS certificates")
	dump := flag.String("dump", "", "Dump request/responses from mirrors in file; empty disables dumping, '-' means stdout")
	dumpProxy := flag.Bool("dump-proxy", false, "Also dump proxied request to -dump file")
	maxConn := flag.Int("max-conn", 200, "Maximum number of connections to upstream servers")
	timeout := flag.Duration("mirror-timeout", 10*time.Second, "Timeout to send requests to mirrors")
	retries := flag.Int("mirror-retries", 3, "Maximum number of retries against mirrors")

	flag.VisitAll(prefixEnv("PRISM", os.Getenv))
	flag.Parse()

	metricsNamespace := "prism"
	proxyBuf := 10

	if *selfTest {
		*proxyHost, *rawMirrors = makeSelfTestServers()
	}

	ms, err := mirrors(strings.SplitN(*rawMirrors, ",", -1)).parse()
	if err != nil {
		log.Fatalf("cannot parse mirror URLs: %v", err)
	}
	proxyURL, err := url.Parse(*proxyHost)
	if err != nil {
		log.Fatalf("invalid proxy URL: %v", err)
	}

	metrics := newMetrics(metricsNamespace)

	if *health != "" {
		healthStarted := make(chan struct{})
		go startHealth(*health, metrics, healthStarted)
		<-healthStarted
	}

	client := makeClient(*insecure, *maxConn, *timeout)
	proxy := newProxy(metrics, ms, *listen, *retries, client, *dump, *dumpProxy, proxyURL, proxyBuf)

	exited := handleSigterm(func() {
		if err := proxy.stop(); err != nil {
			log.Printf("cannot shutdown gracefully: %v", err)
		}
	})

	log.Printf("%s, mirroring to %v\n", *proxyHost, ms)
	log.Printf("info: listening on %s", *listen)

	if err := proxy.listenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("error: cannot start HTTP server: %v", err)
	}
	<-exited
}
