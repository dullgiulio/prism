package main

import (
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	received       *prometheus.CounterVec
	receivedMirror *prometheus.CounterVec
	recvSize       prometheus.Counter
	failedUpstream prometheus.Counter
	failedMirror   *prometheus.CounterVec
}

func newMetrics(namespace string) *metrics {
	m := &metrics{
		received: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "received",
			Help:      "Number of received requests successfully proxied",
		}, []string{"code"}),
		receivedMirror: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "received_mirror",
			Help:      "Number of requests successfully proxied to mirrors",
		}, []string{"mirror"}),
		recvSize: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "received_bytes",
			Help:      "Size of received body sizes in bytes",
		}),
		failedUpstream: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "failed_upstream",
			Help:      "Number of failed requests to reverse proxy",
		}),
		failedMirror: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "failed_mirror",
			Help:      "Number of failed requests to mirrors",
		}, []string{"mirror"}),
	}
	prometheus.MustRegister(m.received)
	prometheus.MustRegister(m.receivedMirror)
	prometheus.MustRegister(m.recvSize)
	prometheus.MustRegister(m.failedUpstream)
	prometheus.MustRegister(m.failedMirror)
	return m
}

func (m *metrics) successUpstream(size int, code string) {
	m.received.With(prometheus.Labels{"code": code}).Inc()
	m.recvSize.Add(float64(size))
}

func (m *metrics) successMirror(mirror string) {
	m.receivedMirror.WithLabelValues(mirror).Inc()
}

func (m *metrics) failUpstream() {
	m.failedUpstream.Inc()
}

func (m *metrics) failMirror(mirror string) {
	m.failedMirror.WithLabelValues(mirror).Inc()
}

func startHealth(health string, metrics *metrics, done chan struct{}) {
	mux := http.NewServeMux()
	healthServer := &http.Server{
		Addr:    health,
		Handler: mux,
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	mux.Handle("/metrics", promhttp.Handler())
	close(done)
	log.Fatal(healthServer.ListenAndServe())
}
