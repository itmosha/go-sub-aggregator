package main

import (
	"log"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Client holds the per-client data derived from the config.
type Client struct {
	Name     string
	Token    string
	NodeURLs []string // full subscription URLs, one per 3x-ui server
}

// Config holds the validated, ready-to-use server configuration.
type Config struct {
	Clients        map[string]*Client // keyed by token for O(1) lookup
	SubPath        string             // the obscured path segment replacing "sub"
	Port           string
	FetchTimeout   time.Duration
	TrustedProxy   string // IP of the reverse proxy; enables X-Forwarded-For
	RateLimitPerMin int   // max requests per minute per IP on /sub/ (0 = disabled)
	RateBurst      int   // token bucket burst size
}

// rawClientConfig mirrors the per-client YAML block.
type rawClientConfig struct {
	Token    string   `yaml:"token"`
	NodeURLs []string `yaml:"node_urls"`
}

// rawConfig mirrors the top-level YAML structure before validation.
type rawConfig struct {
	SubPath         string                     `yaml:"sub_path"`
	Port            string                     `yaml:"port"`
	FetchTimeout    string                     `yaml:"fetch_timeout"`
	TrustedProxy    string                     `yaml:"trusted_proxy"`
	RateLimitPerMin int                        `yaml:"rate_limit_per_min"`
	RateBurst       int                        `yaml:"rate_burst"`
	Clients         map[string]rawClientConfig `yaml:"clients"`
}

// loadConfig reads and validates the YAML config file at the given path.
// It logs a descriptive message and calls log.Fatal for every missing or
// malformed required value; optional fields fall back to documented defaults.
func loadConfig(path string) Config {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("[CONFIG] cannot open config file %q: %v", path, err)
	}
	defer f.Close()

	var raw rawConfig
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // treat unknown YAML keys as an error
	if err := dec.Decode(&raw); err != nil {
		log.Fatalf("[CONFIG] cannot parse config file %q: %v", path, err)
	}

	var cfg Config

	// --- clients (required, at least one) ---
	if len(raw.Clients) == 0 {
		log.Fatal("[CONFIG] clients is required and must contain at least one entry")
	}
	cfg.Clients = make(map[string]*Client, len(raw.Clients))
	for name, rc := range raw.Clients {
		if rc.Token == "" {
			log.Fatalf("[CONFIG] clients.%s: token is required", name)
		}
		if len(rc.NodeURLs) == 0 {
			log.Fatalf("[CONFIG] clients.%s: node_urls is required and must contain at least one URL", name)
		}
		if _, dup := cfg.Clients[rc.Token]; dup {
			log.Fatalf("[CONFIG] clients.%s: token %q is already used by another client", name, rc.Token)
		}
		var urls []string
		for i, u := range rc.NodeURLs {
			u = strings.TrimSpace(u)
			if u == "" {
				log.Printf("[CONFIG] WARN: clients.%s node_urls[%d] is empty — skipping", name, i)
				continue
			}
			urls = append(urls, u)
		}
		if len(urls) == 0 {
			log.Fatalf("[CONFIG] clients.%s: node_urls has no valid entries", name)
		}
		cfg.Clients[rc.Token] = &Client{
			Name:     name,
			Token:    rc.Token,
			NodeURLs: urls,
		}
	}

	// --- sub_path (required) ---
	if raw.SubPath == "" {
		log.Fatal("[CONFIG] sub_path is required — set it to a random string to obscure the endpoint")
	}
	cfg.SubPath = strings.Trim(raw.SubPath, "/")

	// --- port (optional, default 8000) ---
	if raw.Port == "" {
		log.Printf("[CONFIG] port not set — using default \"8000\"")
		cfg.Port = "8000"
	} else {
		cfg.Port = raw.Port
	}

	// --- trusted_proxy (optional) ---
	cfg.TrustedProxy = strings.TrimSpace(raw.TrustedProxy)
	if cfg.TrustedProxy != "" {
		log.Printf("[CONFIG] trusted proxy set to %q — X-Forwarded-For will be used for client IP", cfg.TrustedProxy)
	}

	// --- rate_limit_per_min / rate_burst (optional) ---
	if raw.RateLimitPerMin < 0 {
		log.Fatal("[CONFIG] rate_limit_per_min must be 0 (disabled) or a positive integer")
	}
	cfg.RateLimitPerMin = raw.RateLimitPerMin
	if raw.RateLimitPerMin > 0 {
		if raw.RateBurst <= 0 {
			cfg.RateBurst = raw.RateLimitPerMin // sensible default: burst == per-minute limit
			log.Printf("[CONFIG] rate_burst not set — defaulting to rate_limit_per_min (%d)", cfg.RateBurst)
		} else {
			cfg.RateBurst = raw.RateBurst
		}
	}

	// --- fetch_timeout (optional, default 10s) ---
	if raw.FetchTimeout == "" {
		log.Printf("[CONFIG] fetch_timeout not set — using default \"10s\"")
		cfg.FetchTimeout = 10 * time.Second
	} else {
		d, err := time.ParseDuration(raw.FetchTimeout)
		if err != nil {
			log.Fatalf("[CONFIG] fetch_timeout %q is not a valid Go duration (e.g. \"10s\", \"500ms\"): %v",
				raw.FetchTimeout, err)
		}
		cfg.FetchTimeout = d
	}

	return cfg
}
