# Deploying Riverbed

## Reachability

The device sends the webhook over HTTPS, so Riverbed needs a public HTTPS URL. On a
home network, use one of these:

- A reverse proxy you already run, such as Caddy, nginx or Traefik, with a
  certificate for a name that resolves publicly.
- A tunnel, such as Cloudflare Tunnel or Tailscale Funnel, pointed at the Riverbed
  port.

Riverbed serves plain HTTP and expects the proxy to terminate TLS. Forward the
`Authorization` header, because the webhook token is carried in it.

A Caddy site block is enough:

```
riverbed.example.com {
    reverse_proxy localhost:8080
}
```

`server.base_url` must match the public URL if any MCP server uses OAuth, because
the OAuth redirect is built under it.

## systemd on Debian

Install the binary and the configuration:

```sh
sudo install -m 0755 riverbed /usr/local/bin/riverbed
sudo install -d -o riverbed -g riverbed -m 0750 /var/lib/riverbed /etc/riverbed
sudo install -m 0644 riverbed.toml /etc/riverbed/riverbed.toml
sudo install -m 0600 -o riverbed -g riverbed riverbed.env /etc/riverbed/riverbed.env
```

Set `store.path` to `/var/lib/riverbed/riverbed.db` in the configuration.

`/etc/systemd/system/riverbed.service`:

```ini
[Unit]
Description=Riverbed
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=riverbed
Group=riverbed
EnvironmentFile=/etc/riverbed/riverbed.env
ExecStart=/usr/local/bin/riverbed serve -config /etc/riverbed/riverbed.toml
Restart=on-failure
RestartSec=5s

# The process needs its data directory and nothing else.
StateDirectory=riverbed
ReadWritePaths=/var/lib/riverbed
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
PrivateDevices=true
RestrictAddressFamilies=AF_INET AF_INET6
# potion caches its model under this directory.
Environment=GO_POTION_HOME=/var/lib/riverbed/models

[Install]
WantedBy=multi-user.target
```

Create the user, then start it:

```sh
sudo useradd --system --home /var/lib/riverbed --shell /usr/sbin/nologin riverbed
sudo systemctl daemon-reload
sudo systemctl enable --now riverbed
journalctl -u riverbed -f
```

Set `RIVERBED_LOG_FORMAT=json` in the environment file if you collect logs.

## Docker and Podman

Both runtimes take the same commands.

```sh
docker build -t riverbed .
docker volume create riverbed-data
docker run -d --name riverbed --restart unless-stopped -p 8080:8080 \
  -v riverbed-data:/var/lib/riverbed \
  -v "$PWD/riverbed.toml:/etc/riverbed/riverbed.toml:ro" \
  --env-file riverbed.env \
  riverbed
```

In the container, `RIVERBED_DB` and `RIVERBED_CONFIG` already point at
`/var/lib/riverbed/riverbed.db` and `/etc/riverbed/riverbed.toml`, and
`GO_POTION_HOME` points into the volume, so the embedding model is downloaded once.

`compose.yaml` in the repository root does the same thing. It reads
`RIVERBED_WEBHOOK_TOKEN` and `RIVERBED_MCP_TOKEN` from the environment, so load
`riverbed.env` before you run it:

```sh
set -a; . ./riverbed.env; set +a
docker compose up -d
```

The image has no shell, so debug it by reading logs rather than by opening a session
inside it.

## Cross compiling

The binary is static and does not link libc, so one build per architecture is
enough for any Linux distribution.

```sh
make dist    # linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
```

Copy `dist/riverbed-linux-amd64` to the server as `/usr/local/bin/riverbed`. Use the
`arm64` build for a Raspberry Pi or another ARM host.

## Backup

The database is one SQLite file. Riverbed runs in WAL mode, so copy it with a tool
that understands that, rather than with `cp` while the service runs:

```sh
sqlite3 /var/lib/riverbed/riverbed.db ".backup '/backup/riverbed.db'"
```

Stopping the service and copying the file also works. The audio is inside the
database, so the file grows with the amount of audio you keep. Set
`audio.retain = false` to store transcriptions only.
