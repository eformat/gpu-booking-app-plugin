package api

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

var (
	visitors   = make(map[string]*visitor)
	visitorsMu sync.Mutex

	// RateLimitRate is the sustained request rate per IP (requests/second).
	RateLimitRate = rate.Limit(10)
	// RateLimitBurst is the maximum burst size per IP.
	RateLimitBurst = 50
)

func init() {
	go visitorCleaner()
}

func visitorCleaner() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		visitorsMu.Lock()
		for ip, v := range visitors {
			if time.Since(v.lastSeen) > 10*time.Minute {
				delete(visitors, ip)
			}
		}
		visitorsMu.Unlock()
	}
}

func getVisitor(ip string) *rate.Limiter {
	visitorsMu.Lock()
	defer visitorsMu.Unlock()

	v, ok := visitors[ip]
	if !ok {
		limiter := rate.NewLimiter(RateLimitRate, RateLimitBurst)
		visitors[ip] = &visitor{limiter: limiter, lastSeen: time.Now()}
		return limiter
	}
	v.lastSeen = time.Now()
	return v.limiter
}

// RateLimitMiddleware returns HTTP 429 when a client exceeds the per-IP rate limit.
func RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}

		if !getVisitor(ip).Allow() {
			slog.Warn("rate limit exceeded", "ip", ip, "path", r.URL.Path)
			HttpError(w, http.StatusTooManyRequests, "rate_limit_exceeded")
			return
		}

		next.ServeHTTP(w, r)
	})
}
