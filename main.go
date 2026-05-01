package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// cacheTTL matches the Profile-Update-Interval hint sent to clients (12 hours).
const cacheTTL = 12 * time.Hour

// subscriptionCache holds a single client's last-known aggregated subscription.
// Stale-while-revalidate: requests always get the cached body immediately; a
// background goroutine fetches fresh data when the TTL has elapsed.
type subscriptionCache struct {
	mu         sync.Mutex
	body       string
	fetchedAt  time.Time
	refreshing bool
}

// serve returns the cached body and signals whether a background refresh
// should be triggered (true at most once per TTL window).
func (c *subscriptionCache) serve() (body string, triggerRefresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetchedAt) > cacheTTL && !c.refreshing {
		c.refreshing = true
		triggerRefresh = true
	}
	return c.body, triggerRefresh
}

// update stores a freshly built subscription and clears the refreshing flag.
func (c *subscriptionCache) update(body string) {
	c.mu.Lock()
	c.body = body
	c.fetchedAt = time.Now()
	c.refreshing = false
	c.mu.Unlock()
}

// cancelRefresh clears the refreshing flag when a background fetch fails,
// so the next request can try again.
func (c *subscriptionCache) cancelRefresh() {
	c.mu.Lock()
	c.refreshing = false
	c.mu.Unlock()
}

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

	raw := strings.TrimSpace(string(body))

	// Normalise before decoding: remove any mid-string newlines (some encoders
	// wrap at 76 chars) and strip existing padding so RawStdEncoding can handle
	// both padded and unpadded variants without the += "==" hack breaking things.
	cleaned := strings.TrimRight(strings.ReplaceAll(raw, "\n", ""), "=")

	text := raw
	if decoded, err := base64.RawStdEncoding.DecodeString(cleaned); err == nil {
		text = string(decoded)
	} else if decoded, err := base64.RawURLEncoding.DecodeString(cleaned); err == nil {
		text = string(decoded)
	}

	var proxies []string
	for line := range strings.SplitSeq(text, "\n") {
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

// buildSub fetches and merges the subscription for a client.
// Returns an empty string if all upstream nodes failed.
// The response is plain newline-separated proxy URIs (the "links" format that
// all V2Ray clients support — no base64 encoding needed).
func buildSub(c *Client, timeout time.Duration) string {
	proxies := fetchAll(c, timeout)
	if len(proxies) == 0 {
		return ""
	}
	return strings.Join(proxies, "\n")
}

func runLinks(cfg Config, filter string) {
	if cfg.Domain == "" {
		fmt.Fprintln(os.Stderr, "error: domain is not set in config.yaml — add: domain: \"https://yourdomain.com\"")
		os.Exit(1)
	}
	// Build a name→client map for display (cfg.Clients is keyed by token).
	type entry struct{ name, url string }
	var entries []entry
	for _, c := range cfg.Clients {
		if filter != "" && c.Name != filter {
			continue
		}
		entries = append(entries, entry{
			name: c.Name,
			url:  fmt.Sprintf("%s/%s/%s", cfg.Domain, cfg.SubPath, c.Token),
		})
	}
	if filter != "" && len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "error: client %q not found\n", filter)
		os.Exit(1)
	}
	for _, e := range entries {
		fmt.Printf("%-20s %s\n", e.name, e.url)
	}
}

func main() {
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}

	if len(os.Args) > 1 && os.Args[1] == "links" {
		cfg := loadConfig(configPath)
		filter := ""
		if len(os.Args) > 2 {
			filter = os.Args[2]
		}
		runLinks(cfg, filter)
		return
	}

	cfg := loadConfig(configPath)

	// Build a cache entry per client and pre-warm them in the background so the
	// first real HTTP request is served from cache rather than waiting on upstreams.
	caches := make(map[string]*subscriptionCache, len(cfg.Clients))
	for token, client := range cfg.Clients {
		sc := &subscriptionCache{}
		caches[token] = sc
		go func(c *Client, sc *subscriptionCache) {
			body := buildSub(c, cfg.FetchTimeout)
			if body != "" {
				sc.update(body)
				log.Printf("[INFO] pre-warmed cache for client=%s proxies fetched", c.Name)
			} else {
				log.Printf("[WARN] pre-warm failed for client=%s — will retry on first request", c.Name)
			}
		}(client, sc)
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
		token := strings.TrimPrefix(r.URL.Path, subPrefix)
		client, ok := cfg.Clients[token]
		if !ok {
			http.NotFound(w, r)
			return
		}

		sc := caches[token]
		body, triggerRefresh := sc.serve()

		if triggerRefresh {
			go func() {
				fresh := buildSub(client, cfg.FetchTimeout)
				if fresh != "" {
					sc.update(fresh)
					log.Printf("[INFO] client=%s cache refreshed", client.Name)
				} else {
					sc.cancelRefresh()
					log.Printf("[WARN] client=%s background refresh failed", client.Name)
				}
			}()
		}

		if body == "" {
			// Cache is empty (pre-warm not finished or failed) — fetch synchronously.
			body = buildSub(client, cfg.FetchTimeout)
			if body == "" {
				http.Error(w, "all upstream nodes failed", http.StatusBadGateway)
				return
			}
			sc.update(body)
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Hint V2Box / Hiddify / Clash to auto-refresh every 12 hours.
		w.Header().Set("Profile-Update-Interval", "12")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)

		log.Printf("[INFO] client=%s served proxies to %s (refresh=%v)", client.Name, r.RemoteAddr, triggerRefresh)
	})

	addr := "127.0.0.1:" + cfg.Port
	log.Printf("[INFO] listening on %s with %d client(s)", addr, len(cfg.Clients))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
