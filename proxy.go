package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/dullgiulio/filebuf"
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
	proxyURL  *url.URL
	metrics   *metrics
	mirrors   map[string]*url.URL
	client    *http.Client
	reqs      chan *mirrorRequest
	dumper    *dumper
	dumpProxy bool
	wg        sync.WaitGroup
}

type readCloser struct {
	io.Reader
	io.Closer
}

type mirrorRequest struct {
	mirror   string
	req      *http.Request
	contents *filebuf.Filebuf
}

func (m *mirrorRequest) roundTrip(client *http.Client, contents *filebuf.Filebuf) (*http.Response, error) {
	if m.contents != nil {
		m.req.Body = contents
	}
	return client.Do(m.req)
}

func newMirrorTransport(proxyURL *url.URL, metrics *metrics, mirrors map[string]*url.URL, nworkers int, client *http.Client, retries int, dumper *dumper, dumpProxy bool, nbuf int) *mirrorTransport {
	mt := &mirrorTransport{
		proxyURL:  proxyURL,
		metrics:   metrics,
		mirrors:   mirrors,
		client:    client,
		dumper:    dumper,
		dumpProxy: dumpProxy,
		reqs:      make(chan *mirrorRequest, nbuf),
	}
	mt.wg.Add(nworkers)
	for i := 0; i < nworkers; i++ {
		go mt.consumeRequests(retries)
	}
	return mt
}

func (m *mirrorTransport) retry(mreq *mirrorRequest, contents *filebuf.Filebuf, ntimes int) (*http.Response, error) {
	for i := 0; i < ntimes; i++ {
		resp, err := mreq.roundTrip(m.client, contents)
		if err == nil {
			return resp, nil
		}
		log.Printf("error: %s: request to mirror failed (%d/%d): %v", mreq.mirror, i, ntimes, err)
		if err := contents.Rewind(); err != nil {
			return nil, fmt.Errorf("error: cannot rewind body contents: %v", err)
		}
	}
	return nil, fmt.Errorf("request failed after %d attempts", ntimes)
}

func (m *mirrorTransport) consumeRequests(ntimes int) {
	for mreq := range m.reqs {
		resp, err := m.retry(mreq, mreq.contents, ntimes)
		if err != nil {
			log.Printf("error: %s: retries failed: %v", mreq.mirror, err)
			m.metrics.failMirror(mreq.mirror)
			continue
		}
		m.metrics.successMirror(mreq.mirror)
		//if err := m.dumper.dump(mreq.mirror, mreq.req, resp); err != nil {
		//	log.Printf("error: %s: cannot dump round trip: %v", mreq.mirror, err)
		//}
		if _, err := io.Copy(ioutil.Discard, resp.Body); err != nil {
			log.Printf("error: %s: cannot discard response body: %v", mreq.mirror, err)
		}
		if err := resp.Body.Close(); err != nil {
			log.Printf("error: %s: cannot close mirror response body: %v", mreq.mirror, err)
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

func removeConnectionHeaders(h http.Header) {
	for _, f := range h["Connection"] {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				h.Del(sf)
			}
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
	removeConnectionHeaders(r.Header)
	return &r
}

func (m *mirrorTransport) sendToMirrors(req *http.Request, bodyBuf *filebuf.Filebuf) {
	var bodyBufUsed bool
	for mirror, murl := range m.mirrors {
		var contents *filebuf.Filebuf
		cloneReq := cloneRequest(req)
		cloneReq.URL = murl
		if req.Body != nil {
			var err error
			if !bodyBufUsed {
				contents = bodyBuf
				bodyBufUsed = true
			} else {
				contents, err = bodyBuf.Clone()
			}
			if err != nil {
				log.Printf("cannot clone request body buffer, skip mirror %s: %v", mirror, err)
				continue
			}
		}
		m.reqs <- &mirrorRequest{mirror: mirror, req: cloneReq, contents: contents}
	}
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; https://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

func (m *mirrorTransport) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	bodyBuf := filebuf.New(1 << 18)
	if req.Body != nil {
		tee := io.TeeReader(req.Body, bodyBuf)
		req.Body = &readCloser{tee, req.Body}
	}
	req.URL.Scheme = m.proxyURL.Scheme
	req.URL.Host = m.proxyURL.Host
	req.URL.Path = path.Join(m.proxyURL.Path, req.URL.Path)
	req.Close = false
	if _, ok := req.Header["User-Agent"]; !ok {
		// explicitly disable User-Agent so it's not set to default value
		req.Header.Set("User-Agent", "")
	}
	for _, h := range hopHeaders {
		hv := req.Header.Get(h)
		if hv == "" {
			continue
		}
		req.Header.Del(h)
	}

	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		req.Header.Set("X-Forwarded-For", clientIP)
	}

	resp, err := m.client.Transport.RoundTrip(req)
	if err != nil {
		log.Printf("error: round-trip failed, won't be sent to mirrors: %v", err)
		m.metrics.failUpstream()
		return
	}
	/*
		if m.dumpProxy {
			// request body has been closed, need to restore it for dumping
			req.Body = bodyBuf
			m.dumper.dump("rproxy", req, resp)
			if err := bodyBuf.Rewind(); err != nil {
				log.Printf("cannot rewind request body after dump, won't be sent to mirrors: %w", err)
				return resp, nil
			}
		}
	*/

	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(rw, resp.Body); err != nil {
		log.Printf("error: could not copy response back to client: %v", err)
		m.metrics.failUpstream()
		return
	}
	resp.Body.Close()

	m.sendToMirrors(req, bodyBuf)
	m.metrics.successUpstream(bodyBuf.Len())
}

func makeClient(insecure bool, maxConn int, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.MaxIdleConns = maxConn
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
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

func newProxy(metrics *metrics, ms map[string]*url.URL, listen string, retries int, client *http.Client, dump string, dumpProxy bool, proxyURL *url.URL, proxyBuf int) *proxy {
	mt := newMirrorTransport(proxyURL, metrics, ms, len(ms), client, retries, makeDumper(dump), dumpProxy, proxyBuf)
	srv := &http.Server{
		Addr:         listen,
		Handler:      mt,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  10 * time.Second,
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
