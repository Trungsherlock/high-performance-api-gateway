package cache

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Eligible supports only the demo's single public representation.
func Eligible(r *http.Request) bool {
	if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/api/public/") || r.ContentLength != 0 || (r.Body != nil && r.Body != http.NoBody) {
		return false
	}
	for name, values := range r.Header {
		switch strings.ToLower(name) {
		case "user-agent":
		case "accept":
			if len(values) != 1 || (values[0] != "*/*" && values[0] != "application/json") {
				return false
			}
		case "accept-encoding":
			if len(values) != 1 || values[0] != "identity" {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func Lifetime(r *http.Response, cap time.Duration) time.Duration {
	if r.StatusCode != 200 || len(r.Trailer) > 0 {
		return 0
	}
	for _, h := range []string{"Set-Cookie", "Vary", "Content-Encoding", "Content-Range"} {
		if _, ok := r.Header[http.CanonicalHeaderKey(h)]; ok {
			return 0
		}
	}
	public, seconds := false, int64(-1)
	for _, part := range strings.Split(strings.Join(r.Header.Values("Cache-Control"), ","), ",") {
		name, value, hasValue := strings.Cut(strings.TrimSpace(part), "=")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "public":
			if public || hasValue {
				return 0
			}
			public = true
		case "max-age":
			if seconds >= 0 || !hasValue {
				return 0
			}
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
			if err != nil || n <= 0 {
				return 0
			}
			seconds = n
		default:
			return 0
		}
	}
	if !public || seconds <= 0 {
		return 0
	}
	if ages := r.Header.Values("Age"); len(ages) > 0 {
		if len(ages) != 1 {
			return 0
		}
		age, err := strconv.ParseInt(ages[0], 10, 32)
		if err != nil || age < 0 {
			return 0
		}
		seconds -= age
	}
	if seconds <= 0 {
		return 0
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl > cap {
		ttl = cap
	}
	return ttl
}
