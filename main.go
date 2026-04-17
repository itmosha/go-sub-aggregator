package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// fetchNode downloads one upstream subscription and decodes it.
// 3x-ui serves subscriptions as a base64-encoded blob of newline-separated
// proxy URIs (vless://, trojan://, ss://, etc.).
// Returns a slice of individual proxy URI strings.
func fetchNode(url string, timeout time.Duration) ([]string, error) {
	client := &http.Client{Timeout: timeout}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// base64 padding is sometimes missing — adding "==" is safe because
	// the standard decoder ignores excess padding.
	raw := strings.TrimSpace(string(body))
	decoded, err := base64.StdEncoding.DecodeString(raw + "==")
	if err != nil {
		// Some panels use URL-safe base64
		decoded, err = base64.URLEncoding.DecodeString(raw + "==")
		if err != nil {
			return nil, fmt.Errorf("base64 decode: %w", err)
		}
	}

	var proxies []string
	for _, line := range strings.Split(string(decoded), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			proxies = append(proxies, line)
		}
	}
	return proxies, nil
}

// fetchAll fetches all nodes for a client concurrently and merges the results.
// Nodes that fail are logged but don't abort the whole request.
func fetchAll(c *Client, timeout time.Duration) []string {
	type result struct {
		proxies []string
		err     error
		url     string
	}

	results := make([]result, len(c.NodeURLs))
	var wg sync.WaitGroup

	for i, url := range c.NodeURLs {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			proxies, err := fetchNode(url, timeout)
			results[i] = result{proxies: proxies, err: err, url: url}
		}(i, url)
	}

	wg.Wait()

	var merged []string
	for _, r := range results {
		if r.err != nil {
			log.Printf("[WARN] client=%s node=%s error: %v", c.Name, r.url, r.err)
			continue
		}
		log.Printf("[INFO] client=%s node=%s proxies=%d", c.Name, r.url, len(r.proxies))
		merged = append(merged, r.proxies...)
	}
	return merged
}

// clientIP returns the real client IP, honouring X-Forwarded-For when the
// request arrives from the configured trusted proxy.
func clientIP(r *http.Request, trustedProxy string) string {
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if trustedProxy != "" && remoteIP == trustedProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			// X-Forwarded-For may be a comma-separated list; the leftmost is the client.
			return strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0])
		}
	}
	return remoteIP
}

// ipRateLimiter tracks per-IP token-bucket limiters and evicts stale entries.
type ipRateLimiter struct {
	mu    sync.Mutex
	ips   map[string]*limiterEntry
	limit rate.Limit
	burst int
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(perMin int, burst int) *ipRateLimiter {
	rl := &ipRateLimiter{
		ips:   make(map[string]*limiterEntry),
		limit: rate.Limit(float64(perMin) / 60.0),
		burst: burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *ipRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.ips[ip]
	if !ok {
		e = &limiterEntry{limiter: rate.NewLimiter(rl.limit, rl.burst)}
		rl.ips[ip] = e
	}
	e.lastSeen = time.Now()
	return e.limiter.Allow()
}

// cleanup removes entries that haven't been seen for 10 minutes.
func (rl *ipRateLimiter) cleanup() {
	for range time.Tick(5 * time.Minute) {
		rl.mu.Lock()
		for ip, e := range rl.ips {
			if time.Since(e.lastSeen) > 10*time.Minute {
				delete(rl.ips, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func main() {
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cfg := loadConfig(configPath)

	var limiter *ipRateLimiter
	if cfg.RateLimitPerMin > 0 {
		limiter = newIPRateLimiter(cfg.RateLimitPerMin, cfg.RateBurst)
		log.Printf("[INFO] rate limiting enabled: %d req/min per IP, burst %d", cfg.RateLimitPerMin, cfg.RateBurst)
	}

	mux := http.NewServeMux()

	// Health check — useful for uptime monitors, nothing sensitive here.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok clients=%d", len(cfg.Clients))
	})

	// The actual subscription endpoint.
	// Pattern: /{sub_path}/{token}
	// We deliberately return 404 for wrong tokens to avoid fingerprinting
	// (a 401/403 would reveal that an endpoint exists).
	subPrefix := "/" + cfg.SubPath + "/"
	mux.HandleFunc(subPrefix, func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, cfg.TrustedProxy)

		if limiter != nil && !limiter.allow(ip) {
			log.Printf("[WARN] rate limit exceeded ip=%s", ip)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		token := strings.TrimPrefix(r.URL.Path, subPrefix)
		client, ok := cfg.Clients[token]
		if !ok {
			http.NotFound(w, r)
			return
		}

		proxies := fetchAll(client, cfg.FetchTimeout)
		if len(proxies) == 0 {
			http.Error(w, "all upstream nodes failed", http.StatusBadGateway)
			return
		}

		// Re-encode the merged list back into base64 for the client.
		merged := base64.StdEncoding.EncodeToString([]byte(strings.Join(proxies, "\n")))

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Hint V2Box / Hiddify / Clash to auto-refresh every 12 hours.
		w.Header().Set("Profile-Update-Interval", "12")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, merged)

		log.Printf("[INFO] client=%s ip=%s proxies=%d", client.Name, ip, len(proxies))
	})

	addr := "127.0.0.1:" + cfg.Port
	log.Printf("[INFO] listening on %s with %d client(s)", addr, len(cfg.Clients))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
