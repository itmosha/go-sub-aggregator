# go-sub-aggregator

Aggregates multiple 3x-ui subscription URLs into a single endpoint per client.
Each client app (Hiddify, V2Box, Clash, etc.) gets one URL that merges proxies from all your nodes.

```
client app  ──►  https://yourdomain.com/<sub_path>/<token>
                         │
                         ▼
              go-sub-aggregator
              ┌──────────────────┐
              │  fetches in parallel
              ▼                  ▼
       node Austria         node Sweden
    (3x-ui sub URL)      (3x-ui sub URL)
              │                  │
              └────────┬─────────┘
                       ▼
              merged + re-encoded
                  base64 blob
```

---

## How it works

- **Nodes** are your 3x-ui servers. Each node has a subscription base URL (`https://<ip>:<port>/<sub_path>`).
- **Clients** are your users. Each client has a UUID set identically on every node, so the aggregator can construct `<node_base>/<uuid>` for each node automatically.
- The aggregator exposes `/<sub_path>/<token>` — both segments are random strings you choose, nothing reveals what the service is.

---

## Prerequisites

- A Linux VPS reachable from the internet (Ubuntu 22.04 / Debian 12 recommended)
- Two or more 3x-ui servers already running
- Go 1.21+ (for building; not needed on the server if you copy the binary)
- nginx
- certbot

---

## 1. Prepare your 3x-ui nodes

For each client you want to add, you need to create them with the **same UUID** on every node. This lets the aggregator use a single `client_id` across all servers.

In 3x-ui:
1. Go to a client list for your inbound
2. Click **Add client**
3. Set the UUID manually (generate one with `uuidgen` and reuse it on every node)
4. Save

Repeat on every node with the same UUID.

The subscription base URL for a node looks like:
```
https://<ip>:<port>/<random_string>
```
You can find it in **Panel Settings → Subscription** in 3x-ui. The full per-client URL is `<base>/<uuid>` — the aggregator constructs this for you.

---

## 2. Build the binary

On your local machine or directly on the server:

```bash
git clone https://github.com/itmosha/go-sub-aggregator
cd go-sub-aggregator
go build -o sub-aggregator .
```

Copy the binary to the server if you built locally:

```bash
scp sub-aggregator user@yourserver:/usr/local/bin/sub-aggregator
```

---

## 3. Configure

Copy the example config and edit it:

```bash
cp config.yaml.example config.yaml
nano config.yaml
```

```yaml
nodes:
  - "https://<ip_1>:<port_1>/<sub_path>"   # Austria
  - "https://<ip_2>:<port_2>/<sub_path>"   # Sweden

sub_path: "xK9mP2qRvT"      # random string — replaces /sub/ in your URL
port: "8000"
fetch_timeout: "10s"
trusted_proxy: "127.0.0.1"  # nginx runs on the same machine
rate_limit_per_min: 10
rate_burst: 5

clients:
  user:
    token: "long-random-secret"          # goes into the URL
    client_id: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"  # same UUID on all nodes
  onemoreuser:
    token: "another-long-random-secret"
    client_id: "yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy"
```

Generate random values for `sub_path` and `token`:

```bash
openssl rand -hex 16
```

---

## 4. Run as a systemd service

Create the service file:

```bash
sudo nano /etc/systemd/system/sub-aggregator.service
```

```ini
[Unit]
Description=go-sub-aggregator
After=network.target

[Service]
Type=simple
User=nobody
ExecStart=/usr/local/bin/sub-aggregator
WorkingDirectory=/etc/sub-aggregator
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Put the config in the working directory:

```bash
sudo mkdir /etc/sub-aggregator
sudo cp config.yaml /etc/sub-aggregator/config.yaml
sudo chmod 600 /etc/sub-aggregator/config.yaml
sudo chown nobody:nogroup /etc/sub-aggregator/config.yaml
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable sub-aggregator
sudo systemctl start sub-aggregator
sudo systemctl status sub-aggregator
```

Check logs:

```bash
sudo journalctl -u sub-aggregator -f
```

---

## 5. Set up nginx as a reverse proxy

Install nginx if needed:

```bash
sudo apt install nginx
```

Create the site config:

```bash
sudo nano /etc/nginx/sites-available/sub-aggregator
```

```nginx
server {
    listen 80;
    server_name yourdomain.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name yourdomain.com;

    ssl_certificate     /etc/letsencrypt/live/yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/yourdomain.com/privkey.pem;

    # Only the subscription path is exposed — everything else returns 404.
    location /<sub_path>/ {
        proxy_pass         http://127.0.0.1:8000;
        proxy_set_header   X-Forwarded-For $remote_addr;
        proxy_set_header   Host $host;
        proxy_read_timeout 30s;
    }
}
```

Replace `yourdomain.com` and `<sub_path>` with your values.

Enable the site:

```bash
sudo ln -s /etc/nginx/sites-available/sub-aggregator /etc/nginx/sites-enabled/
sudo nginx -t
```

---

## 6. Obtain a TLS certificate

```bash
sudo apt install certbot python3-certbot-nginx
sudo certbot --nginx -d yourdomain.com
```

Certbot edits the nginx config to fill in the certificate paths and sets up auto-renewal. Reload nginx after:

```bash
sudo nginx -t && sudo systemctl reload nginx
```

---

## 7. Verify

Check the service is healthy:

```bash
curl https://yourdomain.com/<sub_path>/<your_token>
```

You should get a base64 blob back. If you paste it into a base64 decoder you'll see the merged proxy URIs from all your nodes.

Check that unknown paths return 404 and nothing leaks:

```bash
curl -o /dev/null -w "%{http_code}" https://yourdomain.com/
# → 404

curl -o /dev/null -w "%{http_code}" https://yourdomain.com/<sub_path>/wrongtoken
# → 404
```

---

## 8. Adding a new client

1. In 3x-ui on **each node**: add a client with a manually set UUID (same UUID on every node)
2. In `config.yaml` on the server:
   ```yaml
   clients:
     newperson:
       token: "$(openssl rand -hex 16)"
       client_id: "the-uuid-you-set-in-3x-ui"
   ```
3. Restart the service:
   ```bash
   sudo systemctl restart sub-aggregator
   ```
4. Hand them their URL: `https://yourdomain.com/<sub_path>/<token>`

---

## Config reference

| Field | Required | Default | Description |
|---|---|---|---|
| `nodes` | yes | — | List of 3x-ui subscription base URLs (without the client UUID) |
| `sub_path` | yes | — | Random string that replaces `/sub/` in the endpoint path |
| `port` | no | `8000` | Port the Go service listens on (localhost only) |
| `fetch_timeout` | no | `10s` | Timeout for upstream node requests |
| `trusted_proxy` | no | — | IP of the reverse proxy; enables `X-Forwarded-For` for real client IPs |
| `rate_limit_per_min` | no | `0` (off) | Max requests per minute per client IP on the subscription endpoint |
| `rate_burst` | no | `rate_limit_per_min` | Token bucket burst size |
| `clients.<name>.token` | yes | — | Secret that goes into the client's subscription URL |
| `clients.<name>.client_id` | yes | — | UUID of the client, identical across all nodes |
