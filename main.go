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

	// Try base64 (standard, then URL-safe). Some panels skip encoding entirely
	// and return plain newline-separated URIs — fall back to that if decoding fails.
	text := raw
	if decoded, err := base64.StdEncoding.DecodeString(raw + "=="); err == nil {
		text = string(decoded)
	} else if decoded, err := base64.URLEncoding.DecodeString(raw + "=="); err == nil {
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

func main() {
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cfg := loadConfig(configPath)

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

		log.Printf("[INFO] client=%s served proxies=%d to %s", client.Name, len(proxies), r.RemoteAddr)
	})

	addr := "127.0.0.1:" + cfg.Port
	log.Printf("[INFO] listening on %s with %d client(s)", addr, len(cfg.Clients))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
