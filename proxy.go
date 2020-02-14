package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
)

func hostWithoutPort(host string) string {
	parts := strings.SplitN(host, ":", 2)
	return parts[0]
}

type mirrors []string

func (m mirrors) parse() (map[string]*url.URL, error) {
	urls := make(map[string]*url.URL, len(m))
	for i := range m {
		u, err := url.Parse(m[i])
		if err != nil {
			return nil, fmt.Errorf("invalid mirror URL: %v", err)
		}
		urls[hostWithoutPort(u.Host)] = u
	}
	return urls, nil
}

type syncer interface {
	Sync() error
}

type dumper struct {
	out  *bufio.Writer
	sync syncer
	mux  sync.Mutex
}

func newDumper(w io.Writer, sync bool) *dumper {
	d := &dumper{out: bufio.NewWriter(w)}
	if sync {
		if s, ok := w.(syncer); ok {
			d.sync = s
		}
	}
	return d
}

func (d *dumper) dump(mirror string, req *http.Request, resp *http.Response) error {
	if d.out == nil {
		return nil
	}

	d.mux.Lock()
	defer d.mux.Unlock()

	body, err := httputil.DumpRequest(req, true)
	if err != nil {
		return fmt.Errorf("cannot dump request: %v", err)
	}

	fmt.Fprintf(d.out, "***** Request %s *****\n", mirror)
	d.out.Write(body)

	body, err = httputil.DumpResponse(resp, true)
	if err != nil {
		return fmt.Errorf("cannot dump response: %v", err)
	}

	d.out.Write(body)
	d.out.Write([]byte("\n\n"))

	if err := d.out.Flush(); err != nil {
		return fmt.Errorf("cannot flush buffer: %v", err)
	}
	if d.sync != nil {
		if err := d.sync.Sync(); err != nil {
			return fmt.Errorf("cannot flush round trip: %v", err)
		}
	}
	return nil
}

type mirrorTransport struct {
	metrics   *metrics
	mirrors   map[string]*url.URL
	transport http.RoundTripper
	reqs      chan *mirrorRequest
	dumper    *dumper
	wg        sync.WaitGroup
}

type readCloser struct {
	io.Reader
	io.Closer
}

type mirrorRequest struct {
	req    *http.Request
	mirror string
}

func newMirrorTransport(metrics *metrics, mirrors map[string]*url.URL, nworkers int, transport http.RoundTripper, dumper *dumper, nbuf int) *mirrorTransport {
	mt := &mirrorTransport{
		metrics:   metrics,
		mirrors:   mirrors,
		transport: transport,
		dumper:    dumper,
		reqs:      make(chan *mirrorRequest, nbuf),
	}
	mt.wg.Add(nworkers)
	for i := 0; i < nworkers; i++ {
		go mt.consumeRequests()
	}
	return mt
}

func (m *mirrorTransport) consumeRequests() {
	for mreq := range m.reqs {
		resp, err := m.transport.RoundTrip(mreq.req)
		if err != nil {
			log.Printf("error: %s: request to mirror failed: %v", mreq.mirror, err)
			m.metrics.failMirror(mreq.mirror)
			continue
		}
		m.metrics.successMirror(mreq.mirror)
		if err := m.dumper.dump(mreq.mirror, mreq.req, resp); err != nil {
			log.Printf("error: %s: cannot dump round trip: %v", mreq.mirror, err)
		}
	}
	m.wg.Done()
}

func (m *mirrorTransport) stop() {
	close(m.reqs)
}

func (m *mirrorTransport) wait() {
	m.wg.Wait()
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
		m.metrics.failUpstream()
		return nil, err
	}
	m.metrics.successUpstream(bodyBuf.Len())
	for mirror, murl := range m.mirrors {
		cloneReq := cloneRequest(req)
		cloneReq.URL = murl
		if req.Body != nil {
			cloneReq.Body = ioutil.NopCloser(bytes.NewReader(bodyBuf.Bytes()))
		}
		m.reqs <- &mirrorRequest{mirror: mirror, req: cloneReq}
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

type proxy struct {
	srv       *http.Server
	transport *mirrorTransport
}

func newProxy(metrics *metrics, ms map[string]*url.URL, listen string, insecure bool, dump string, proxyURL *url.URL, proxyBuf int) *proxy {
	httpTransport := makeTransport(insecure)
	mt := newMirrorTransport(metrics, ms, len(ms), httpTransport, makeDumper(dump), proxyBuf)
	upstream := httputil.NewSingleHostReverseProxy(proxyURL)
	upstream.Transport = mt
	srv := &http.Server{
		Addr:    listen,
		Handler: upstream,
	}
	return &proxy{
		srv:       srv,
		transport: mt,
	}
}

func (p *proxy) stop() error {
	if err := p.srv.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("cannot stop HTTP server: %v", err)
	}
	p.transport.stop()
	p.transport.wait()
	return nil
}

func (p *proxy) listenAndServe() error {
	return p.srv.ListenAndServe()
}
