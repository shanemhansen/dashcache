// +build !statsoverride

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"time"
)

func init() {
	prometheus.DefaultRegisterer.MustRegister(query_duration)
	http.Handle("/metrics2", promhttp.Handler()) // don't want to shadow prom /metrics
}

var query_duration = prometheus.NewSummaryVec(prometheus.SummaryOpts{
	Name: "dashcache_duration",
	Help: "How long requests take to serve",
}, []string{"cache"})

func IncrTime(duration time.Duration, keyParts ...interface{}) {
	query_duration.With(map[string]string{"cache": "miss"}).Observe(float64(duration.Nanoseconds()))
}
