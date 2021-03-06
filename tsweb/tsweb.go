// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package tsweb contains code between various Tailscale webservers.
package tsweb

import (
	"expvar"
	_ "expvar"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/interfaces"
	"tailscale.com/metrics"
)

// DevMode controls whether extra output in shown, for when the binary is being run in dev mode.
var DevMode bool

// NewMux returns a new ServeMux with debugHandler registered (and protected) at /debug/.
func NewMux(debugHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	RegisterCommonDebug(mux)
	mux.Handle("/debug/", Protected(debugHandler))
	return mux
}

func RegisterCommonDebug(mux *http.ServeMux) {
	expvar.Publish("counter_uptime_sec", expvar.Func(func() interface{} { return int64(Uptime().Seconds()) }))
	mux.Handle("/debug/pprof/", Protected(http.DefaultServeMux)) // to net/http/pprof
	mux.Handle("/debug/vars", Protected(http.DefaultServeMux))   // to expvar
	mux.Handle("/debug/varz", Protected(http.HandlerFunc(varzHandler)))
}

func DefaultCertDir(leafDir string) string {
	cacheDir, err := os.UserCacheDir()
	if err == nil {
		return filepath.Join(cacheDir, "tailscale", leafDir)
	}
	return ""
}

// IsProd443 reports whether addr is a Go listen address for port 443.
func IsProd443(addr string) bool {
	_, port, _ := net.SplitHostPort(addr)
	return port == "443" || port == "https"
}

// AllowDebugAccess reports whether r should be permitted to access
// various debug endpoints.
func AllowDebugAccess(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		// TODO if/when needed. For now, conservative:
		return false
	}
	ipStr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(ipStr)
	return interfaces.IsTailscaleIP(ip) || ip.IsLoopback() || ipStr == os.Getenv("ALLOW_DEBUG_IP")
}

// Protected wraps a provided debug handler, h, returning a Handler
// that enforces AllowDebugAccess and returns forbiden replies for
// unauthorized requests.
func Protected(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !AllowDebugAccess(r) {
			msg := "debug access denied"
			if DevMode {
				ipStr, _, _ := net.SplitHostPort(r.RemoteAddr)
				msg += fmt.Sprintf("; to permit access, set ALLOW_DEBUG_IP=%v", ipStr)
			}
			http.Error(w, msg, http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

var timeStart = time.Now()

func Uptime() time.Duration { return time.Since(timeStart).Round(time.Second) }

// Port80Handler is the handler to be given to
// autocert.Manager.HTTPHandler.  The inner handler is the mux
// returned by NewMux containing registered /debug handlers.
type Port80Handler struct{ Main http.Handler }

func (h Port80Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.RequestURI
	if path == "/debug" || strings.HasPrefix(path, "/debug") {
		h.Main.ServeHTTP(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "Use HTTPS", http.StatusBadRequest)
		return
	}
	if path == "/" && AllowDebugAccess(r) {
		// Redirect authorized user to the debug handler.
		path = "/debug/"
	}
	target := "https://" + stripPort(r.Host) + path
	http.Redirect(w, r, target, http.StatusFound)
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return net.JoinHostPort(host, "443")
}

// varzHandler is an HTTP handler to write expvar values into the
// prometheus export format:
//
//   https://github.com/prometheus/docs/blob/master/content/docs/instrumenting/exposition_formats.md
//
// It makes the following assumptions:
//
//   * *expvar.Int are counters.
//   * a *tailscale/metrics.Set is descended into, joining keys with
//     underscores. So use underscores as your metric names.
//   * an expvar named starting with "gauge_" or "counter_" is of that
//     Prometheus type, and has that prefix stripped.
//   * anything else is untyped and thus not exported.
//   * expvar.Func can return an int or int64 (for now) and anything else
//     is not exported.
//
// This will evolve over time, or perhaps be replaced.
func varzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	var dump func(prefix string, kv expvar.KeyValue)
	dump = func(prefix string, kv expvar.KeyValue) {
		name := prefix + kv.Key
		var typ string
		switch v := kv.Value.(type) {
		case *expvar.Int:
			// Fast path for common value type.
			fmt.Fprintf(w, "# TYPE %s counter\n%s %v\n", name, name, v.Value())
			return
		case *metrics.Set:
			v.Do(func(kv expvar.KeyValue) {
				dump(name+"_", kv)
			})
			return
		}
		if strings.HasPrefix(kv.Key, "gauge_") {
			typ = "gauge"
			name = prefix + strings.TrimPrefix(kv.Key, "gauge_")
		} else if strings.HasPrefix(kv.Key, "counter_") {
			typ = "counter"
			name = prefix + strings.TrimPrefix(kv.Key, "counter_")
		}
		if fn, ok := kv.Value.(expvar.Func); ok {
			val := fn()
			switch val.(type) {
			case int64, int:
				if typ != "" {
					fmt.Fprintf(w, "# TYPE %s %s\n%s %v\n", name, typ, name, val)
					return
				}
			}
			fmt.Fprintf(w, "# skipping expvar func %q returning unknown type %T\n", name, val)
			return
		}
		fmt.Fprintf(w, "# skipping func %q returning unknown type %T\n", name, kv.Value)
	}
	expvar.Do(func(kv expvar.KeyValue) {
		dump("", kv)
	})
}
