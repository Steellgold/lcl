# Configuration and operations

The Compose deployment reads `.env`. This file is ignored by Git; `.env.example` contains placeholders only.

| Variable | Default | Purpose |
| --- | --- | --- |
| `LCL_DOMAIN` | Required | Parent domain for random subdomains. |
| `TRAEFIK_DYNAMIC_DIR` | Required in Compose | Host directory watched by Traefik's file provider. |
| `TRAEFIK_NETWORK` | Required in Compose | Existing Docker network shared with Traefik. |
| `LCL_LISTEN` | `0.0.0.0:19473` in Compose | Broker HTTP listener inside the container; no port is published. |
| `LCL_BACKEND_URL` | `http://lcl-broker:19473` in Compose | Broker URL written into Traefik routes. Set it alongside the listener. |
| `LCL_HTTP_ENTRYPOINT` | `http` | Existing Traefik HTTP entrypoint. |
| `LCL_HTTPS_ENTRYPOINT` | `https` | Existing Traefik HTTPS entrypoint. |
| `LCL_CERT_RESOLVER` | Empty | Existing Traefik ACME resolver. Empty uses certificates already loaded by Traefik. |
| `LCL_UID`, `LCL_GID` | `0`, `0` | Broker identity; needs write access to both mounted directories. |

When running `lcl serve` directly, `LCL_CONFIG` changes the generated file path (default `/dynamic/lcl.yaml`) and `LCL_SOCKET` changes the control socket (default `/run/lcl/control.sock`). The client also accepts `LCL_SOCKET`. Create the parent directories before starting the broker. Outside Compose, `LCL_LISTEN` defaults to `127.0.0.1:19473` and `LCL_BACKEND_URL` defaults to `http://` followed by `LCL_LISTEN`.

## Traefik and Coolify

The broker generates exact-host routes. Random names use priority 100; custom names use priority 1 so existing application routes take precedence. HTTP and HTTPS entrypoint names are configurable. HTTPS sessions include an HTTP-to-HTTPS redirect. Plain HTTP sessions do not request certificates.

Use a dedicated preview domain when possible. If another application already serves a chosen hostname, the HTTP readiness check fails rather than replacing that application's route. Choose another name or configure a separate `LCL_DOMAIN`.

Coolify deployments can use their existing Traefik dynamic directory, Docker network, and resolver. Check the actual values on your installation rather than copying values from another server. Mount the watched directory, not just `lcl.yaml`, because updates use atomic replacement.

Traefik applies file changes asynchronously. A new URL may take a few seconds to route. Removed sessions are rejected immediately by the broker while Traefik catches up. Only `lcl.yaml` belongs to this service; do not edit it manually.

## Lifecycle

The client holds a Unix socket connection for the session's lifetime. The broker authenticates its Linux UID with `SO_PEERCRED` and enforces per-user quotas. Each upstream connection is relayed over a separate Unix socket connection authenticated by UID and a one-time token. Client closure removes the route. Sessions have no timeout by default. With `-t <minutes>` or `--timeout <minutes>`, the broker enforces a fixed deadline from session creation, cancels active requests, and closes backend connections when the deadline is reached.

## Container terminals

The client dials localhost in its own network namespace. To use it in a development container, install the client there and share a directory containing the control socket. No host networking, published application ports, or Docker socket access is required in the development container.

For example, mount a host directory into the broker as `/shared-runtime`, set `LCL_EXTRA_SOCKET=/shared-runtime/control.sock`, then mount the same host directory into the development container. Set the client's `LCL_SOCKET` to the socket's path inside that container. The shared directory must be writable by the broker UID and traversable by client users.

Use a local, ignored `compose.override.yaml` for machine-specific mounts. For an already-running terminal, an administrator can install a launcher in its existing `PATH` that sets `LCL_SOCKET` and executes the static client. Make that launcher part of the development image or a persistent shared directory for subsequent deployments.

Traffic does not change a session's lifetime. The previous `LCL_IDLE_TIMEOUT` setting has been removed; remove it from existing `.env` files when upgrading. Older clients without the timeout option create unlimited sessions after the broker is upgraded.

The broker has no session database. Startup clears its generated routes; a crash or restart ends all sessions. Docker's `unless-stopped` policy restarts the broker after a process failure or host reboot. Clients must run `lcl` again.

## Connection handling

The relay forwards closure in each direction while allowing the other direction to finish sending pending data. When an application closes an idle HTTP connection, the broker can establish a fresh connection for the next request. WebSocket closures are forwarded too; SSE data is streamed as it arrives.

If the client cannot connect to the local application, the broker returns HTTP 502. The upstream response-header timeout is 60 seconds; reaching it returns HTTP 504. These request failures leave the preview session open, so subsequent requests can recover after an application restart. Streaming responses and upgraded connections have no fixed lifetime unless the session was started with `-t` or `--timeout`.

Unexpected proxy errors are logged with the preview hostname. Full request URLs and headers are not included in these messages.

## Logs and upgrades

```sh
docker compose logs --tail 100 broker
docker compose up -d --build
```

An upgrade interrupts current sessions. After rebuilding, repeat the client extraction and installation steps in the README to update `/usr/local/bin/lcl`.

## Remove

```sh
docker compose stop
docker compose down
sudo rm /usr/local/bin/lcl
```

Graceful shutdown empties the generated routes. You can then remove `lcl.yaml` from the configured dynamic directory and remove `/run/lcl` once no broker is using it. Do not remove the dynamic directory itself; it may contain configuration for other applications.
