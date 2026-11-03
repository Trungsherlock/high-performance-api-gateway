package gateway

import (
	"context"
	"errors"
	"fmt"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/coalesce"
	"high-performance-api-gateway/internal/limiter"
	"high-performance-api-gateway/internal/observe"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// New returns a fixed-upstream proxy and its shared transport, owned by the caller.
func New(upstream string, timeout time.Duration) (http.Handler, *http.Transport, error) {
	return NewWithCache(upstream, timeout, nil)
}

func NewWithCache(upstream string, timeout time.Duration, storage *cache.Cache) (http.Handler, *http.Transport, error) {
	return NewWithOptions(upstream, timeout, storage, Options{})
}

type Options struct {
	Metrics       *observe.Metrics
	Coalesce      bool
	MaxConcurrent int // Zero disables the limiter for baseline experiments.
	PermitWait    time.Duration
	Lifecycle     context.Context
}

func NewWithOptions(upstream string, timeout time.Duration, storage *cache.Cache, options Options) (http.Handler, *http.Transport, error) {
	if options.MaxConcurrent < 0 || options.PermitWait < 0 {
		return nil, nil, fmt.Errorf("limiter settings must be nonnegative")
	}
	if options.Lifecycle == nil {
		options.Lifecycle = context.Background()
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.Fragment != "" || target.RawQuery != "" || target.Opaque != "" {
		return nil, nil, fmt.Errorf("upstream must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("upstream timeout must be positive")
	}
	transport := &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 64,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 3 * time.Second,
		ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second,
		DisableCompression: true,
	}
	var outbound http.RoundTripper = transport
	if options.Metrics != nil {
		outbound = &observe.Transport{Next: outbound, Metrics: options.Metrics}
	}
	if options.MaxConcurrent > 0 {
		limited := limiter.New(outbound, options.MaxConcurrent, options.PermitWait)
		limited.Metrics = options.Metrics
		outbound = limited
	}
	if storage != nil {
		ct := &cacheTransport{metrics: options.Metrics, next: outbound, cache: storage, timeout: timeout, lifecycle: options.Lifecycle}
		if options.Coalesce {
			ct.group = &coalesce.Group[sharedResult]{}
		}
		outbound = ct
	}
	proxy := &httputil.ReverseProxy{
		Transport:  outbound,
		BufferPool: &proxyBuffers{},
		Rewrite: func(r *httputil.ProxyRequest) {
			if !cache.Eligible(r.In) {
				r.Out = r.Out.WithContext(context.WithValue(r.Out.Context(), bypassKey{}, true))
			}
			r.SetURL(target)
			r.Out.URL.RawQuery = r.In.URL.RawQuery
		},
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode == http.StatusSwitchingProtocols {
				return fmt.Errorf("protocol upgrades are unsupported")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			var networkError net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
				status = http.StatusGatewayTimeout
			}
			if errors.Is(err, limiter.ErrOverloaded) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, http.StatusText(status), status)
		},
	}
	return options.Metrics.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
			http.Error(w, "tunneling and protocol upgrades are unsupported", http.StatusNotImplemented)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})), transport, nil
}
