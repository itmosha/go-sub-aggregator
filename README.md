# go-sub-aggregator

Aggregates multiple 3x-ui subscription URLs into a single endpoint per client.

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
              merged proxy list
```

Responses are cached for 12 hours with stale-while-revalidate — client apps always get an instant response.

---

## Paths

| Thing | Path |
|---|---|
| Binary | `/opt/go-sub-aggregator/go-sub-aggregator` |
| Config | `/opt/go-sub-aggregator/config.yaml` |
| Service | `go-sub-aggregator` (systemd) |
| Subscription URL | `https://yourdomain.com/<sub_path>/<token>` |

---

## Deploy a code update

```bash
cd ~/go-sub-aggregator
git pull
go build -o go-sub-aggregator .
sudo systemctl stop go-sub-aggregator
sudo cp go-sub-aggregator /opt/go-sub-aggregator/go-sub-aggregator
sudo systemctl start go-sub-aggregator
```

---

## Edit the config

```bash
sudo nano /opt/go-sub-aggregator/config.yaml
sudo systemctl restart go-sub-aggregator
```

### Add a client

Get the subscription link from each 3x-ui panel for that user, then append to `clients`:

```yaml
clients:
  alice:
    token: "paste-output-of-openssl-rand-hex-16"
    node_urls:
      - "https://<node1_ip>:<port>/<sub_path>/<sub_token>"
      - "https://<node2_ip>:<port>/<sub_path>/<sub_token>"
```

Restart, then give them: `https://yourdomain.com/<sub_path>/<token>`

### Remove a client

Delete their block from `clients`, restart.

### Config reference

```yaml
sub_path: "xK9mP2qRvT"       # random string in the endpoint path — required
port: "8000"                  # default 8000
fetch_timeout: "10s"          # per-node HTTP timeout — lower if nodes are slow
trusted_proxy: "127.0.0.1"   # nginx on same machine — enables X-Forwarded-For
rate_limit_per_min: 10        # max requests/min per IP (0 = disabled)
rate_burst: 5

clients:
  name:
    token: "long-random-secret"
    node_urls:
      - "https://..."
```

---

## Status and logs

```bash
sudo systemctl status go-sub-aggregator
sudo journalctl -u go-sub-aggregator -f       # live
sudo journalctl -u go-sub-aggregator -n 50    # last 50 lines
```

---

## Verify a subscription URL

```bash
curl https://yourdomain.com/<sub_path>/<token> | head -5
# should show vless:// or trojan:// lines
```
