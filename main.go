package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
)

type mirrors []string

func (m mirrors) parse() ([]*url.URL, error) {
	urls := make([]*url.URL, len(m))
	for i := range m {
		u, err := url.Parse(m[i])
		if err != nil {
			return nil, fmt.Errorf("invalid mirror URL: %v", err)
		}
		urls[i] = u
	}
	return urls, nil
}

type syncer interface {
	Sync() error
}

type dumper struct {
	out  io.Writer
	sync syncer
	mux  sync.Mutex
}

func newDumper(w io.Writer, sync bool) *dumper {
	d := &dumper{out: w}
	if sync {
		if s, ok := w.(syncer); ok {
			d.sync = s
		}
	}
	return d
}

func (d *dumper) dump(req *http.Request, resp *http.Response) error {
	if d.out == nil {
		return nil
	}

	d.mux.Lock()
	defer d.mux.Unlock()

	body, err := httputil.DumpRequest(req, true)
	if err != nil {
		return fmt.Errorf("cannot dump request: %v", err)
	}
	if _, err := d.out.Write(body); err != nil {
		return fmt.Errorf("cannot write request: %v", err)
	}
	body, err = httputil.DumpResponse(resp, true)
	if err != nil {
		return fmt.Errorf("cannot dump response: %v", err)
	}
	if _, err := d.out.Write(body); err != nil {
		return fmt.Errorf("cannot write response: %v", err)
	}
	if _, err := d.out.Write([]byte("\n\n")); err != nil {
		return fmt.Errorf("cannot write separator: %v", err)
	}
	if d.sync != nil {
		if err := d.sync.Sync(); err != nil {
			return fmt.Errorf("cannot flush round trip: %v", err)
		}
	}
	return nil
}

type mirrorTransport struct {
	mirrors   []*url.URL
	transport http.RoundTripper
	reqs      chan *http.Request
	dumper    *dumper
}

type readCloser struct {
	io.Reader
	io.Closer
}

func newMirrorTransport(mirrors []*url.URL, nworkers int, transport http.RoundTripper, dumper *dumper, nbuf int) *mirrorTransport {
	mt := &mirrorTransport{
		mirrors:   mirrors,
		transport: transport,
		dumper:    dumper,
		reqs:      make(chan *http.Request, nbuf),
	}
	for i := 0; i < nworkers; i++ {
		go mt.consumeRequests()
	}
	return mt
}

func (m *mirrorTransport) consumeRequests() {
	for req := range m.reqs {
		resp, err := m.transport.RoundTrip(req)
		if err != nil {
			log.Printf("error: request to mirror failed: %v", err)
			continue
		}
		if err := m.dumper.dump(req, resp); err != nil {
			log.Printf("error: cannot dump round trip: %v", err)
		}
	}
}

func cloneRequest(req *http.Request) *http.Request {
	var r http.Request
	r.Method = req.Method
	r.URL = req.URL
	r.Proto = req.Proto
	r.ProtoMajor = req.ProtoMajor
	r.ProtoMinor = req.ProtoMinor
	r.Header = req.Header
	r.TransferEncoding = req.TransferEncoding
	r.Trailer = req.Trailer
	return &r
}

func (m *mirrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBuf bytes.Buffer
	if req.Body != nil {
		tee := io.TeeReader(req.Body, &bodyBuf)
		req.Body = &readCloser{tee, req.Body}
	}
	resp, err := m.transport.RoundTrip(req)
	if err != nil {
		log.Printf("error: round-trip failed, won't be sent to mirrors: %v", err)
		return nil, err
	}
	for i := range m.mirrors {
		cloneReq := cloneRequest(req)
		cloneReq.URL = m.mirrors[i]
		if req.Body != nil {
			cloneReq.Body = ioutil.NopCloser(bytes.NewReader(bodyBuf.Bytes()))
		}
		m.reqs <- cloneReq
	}
	return resp, nil
}

func makeTransport(insecure bool) *http.Transport {
	transport := *http.DefaultTransport.(*http.Transport)
	transport.ForceAttemptHTTP2 = false
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &transport
}

func startHealth(health string, done chan struct{}) {
	mux := http.NewServeMux()
	healthServer := &http.Server{
		Addr:    health,
		Handler: mux,
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	close(done)
	log.Fatal(healthServer.ListenAndServe())
}

func makeDumper(dump string) *dumper {
	var (
		dumpOut  io.Writer
		dumpSync bool
	)
	switch dump {
	case "":
		dumpOut = nil
	case "-":
		dumpOut = os.Stdout
	default:
		var err error
		dumpSync = true
		dumpOut, err = os.OpenFile(dump, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644|os.ModeAppend)
		if err != nil {
			log.Fatalf("error: cannot create file for round trip dumping: %v", err)
		}
		// no need to close, we will flush after every write
	}
	return newDumper(dumpOut, dumpSync)
}

func makeProxy(ms []*url.URL, listen string, insecure bool, dump string, proxyURL *url.URL, proxyBuf int) *http.Server {
	proxy := httputil.NewSingleHostReverseProxy(proxyURL)
	proxy.Transport = newMirrorTransport(ms, len(ms), makeTransport(insecure), makeDumper(dump), proxyBuf)
	return &http.Server{
		Addr:    listen,
		Handler: proxy,
	}
}

func makeSelfTestServers() (string, string) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "this call was relayed by the reverse proxy")
		if _, err := io.Copy(w, r.Body); err != nil {
			log.Fatalf("cannot copy body in mirror server: %v", err)
		}
	}))
	mirrorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "mirror server\n")
		if _, err := io.Copy(w, r.Body); err != nil {
			log.Fatalf("cannot copy body in mirror server: %v", err)
		}
	}))
	return backendServer.URL, mirrorServer.URL
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

// TODO: prometheus monitoring
// TODO: graceful shutdown

func main() {
	health := flag.String("health", ":7979", "Listen IP:PORT for health check")
	listen := flag.String("listen", ":8080", "Listen to this IP:PORT")
	proxyHost := flag.String("proxy", "", "Host to reverse proxy")
	rawMirrors := flag.String("mirrors", "", "Comma separated list of endpoints to mirror data to")
	selfTest := flag.Bool("self-test", false, "Self test mode")
	insecure := flag.Bool("insecure", false, "Allow invalid TLS certificates")
	dump := flag.String("dump", "", "Dump request/responses from mirrors in file; empty disables dumping, '-' means stdout")

	flag.VisitAll(prefixEnv("PRISM", os.Getenv))
	flag.Parse()

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

	if *health != "" {
		healthStarted := make(chan struct{})
		go startHealth(*health, healthStarted)
		<-healthStarted
	}

	srv := makeProxy(ms, *listen, *insecure, *dump, proxyURL, proxyBuf)
	fmt.Printf("%s, mirroring to %v\n", *proxyHost, ms)
	log.Fatal(srv.ListenAndServe())
}
