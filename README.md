# lcl

Share an HTTP application running on your Linux server with a temporary public URL.

```console
$ lcl 3000

  http://a3f09d72b18c460e.preview.example.com → http://127.0.0.1:3000

  Stays open until you stop it. Press Ctrl+C to stop.
```

`lcl` runs on the same server as your application. A local broker creates a route in your existing Traefik proxy. No external tunnel service or client account is involved.

- Random subdomains, or a chosen name with `--name`.
- Unlimited session lifetime by default, with an optional fixed timeout.
- WebSocket and server-sent event support.
- Ctrl+C closes the session, including open connections.
- One shared installation for all local users, without giving them Docker access.
- Optional public HTTPS with an existing Traefik certificate or ACME resolver.

## Use

Start your application, then run:

```sh
lcl 3000
lcl 3000 --https
lcl 3000 -t 10
lcl 3000 --timeout 10
lcl 3000 --name "First version preview"
```

The application must accept HTTP connections at `127.0.0.1:<port>` from the terminal running `lcl`. Applications listening on `0.0.0.0` work too. Supported ports are 1024–65535. `--https` changes the public URL; the application still uses HTTP locally.

`--name` selects the subdomain under `LCL_DOMAIN`. Names are lowercased, Latin accents are folded, and spaces or punctuation become hyphens. For example, `État de première version` becomes `etat-de-premiere-version.preview.example.com` when `LCL_DOMAIN=preview.example.com`. The normalized name must contain 1–63 ASCII letters, digits, or hyphens. A name already held by an active session is rejected; it becomes available again when that session closes. Without `--name`, a random subdomain is generated.

Keep the command running. Ctrl+C, terminal closure, or client disconnection removes the session. By default, sessions stay open indefinitely, including when there is no traffic.

Use `-t <minutes>` or `--timeout <minutes>` for an automatic cutoff. For example, `lcl 3000 -t 10` closes the session ten minutes after its creation, regardless of HTTP requests, SSE data, or WebSocket traffic. The timeout must be a positive whole number of minutes. Expiration closes active connections as well as the public route.

For HTTP sessions, the client checks the public route before printing the URL. This usually takes a few seconds while Traefik loads the change. The timeout includes this setup time; readiness checks do not call your application.

Each Linux user can hold eight sessions; the broker accepts 256 sessions in total. Sessions are not restored after a broker restart.

## Install

You need Linux, Docker with Compose, an existing Traefik instance with its file provider enabled, and wildcard DNS pointing to your server. `lcl` does not configure DNS, install Traefik, or change existing routes.

```sh
git clone --branch stable https://github.com/Steellgold/lcl.git
cd lcl
cp .env.example .env
```

Edit `.env`:

1. Set `LCL_DOMAIN` to the parent domain for temporary URLs.
2. Set `TRAEFIK_DYNAMIC_DIR` to the host directory watched by Traefik.
3. Set `TRAEFIK_NETWORK` to an existing Docker network shared with Traefik.
4. Keep the default broker listener and backend URL unless they conflict with your setup.
5. Set the entrypoint names to match your Traefik configuration.
6. If the dynamic directory belongs to another UID, set `LCL_UID` and `LCL_GID` accordingly. Create `/run/lcl` with that owner and mode `0755` before starting the broker. The container has no capabilities to bypass file permissions.

The broker joins Traefik's Docker network as `lcl-broker`. It publishes no TCP ports on the host; only Traefik needs to reach its HTTP listener.

Build and start the broker:

```sh
docker compose up -d --build
```

Extract the static client from the image and install it globally:

```sh
mkdir -p bin
container_id=$(docker create lcl:local)
docker cp "$container_id:/usr/local/bin/lcl" bin/lcl
docker rm "$container_id"
sudo install -m 0755 bin/lcl /usr/local/bin/lcl
lcl --help
```

The client requires no runtime dependencies. Local users need access to `/run/lcl/control.sock`; its parent directory should have mode `0755`. The broker creates the socket with mode `0666` and obtains the connecting user's UID from the kernel.

See [configuration and operations](docs/operations.md) for HTTPS, upgrades, and removal.

## HTTPS

For an existing wildcard certificate loaded by Traefik, leave `LCL_CERT_RESOLVER` empty. To request certificates through an existing ACME resolver, set its name in `.env`.

The first HTTPS request may need to wait for certificate issuance. A fresh random hostname requires a new certificate unless a wildcard is available. For frequent use, configure a wildcard certificate through DNS-01 to avoid consuming the domain's [Let's Encrypt issuance allowance](https://letsencrypt.org/docs/rate-limits/).

HTTP works independently of certificate setup.

## Development

Go 1.26 or later is required. The project uses only the standard library.

```sh
make build
make check
```

The tests cover proxy forwarding, the client relay, route removal, unlimited sessions and fixed timeouts, WebSocket upgrades and traffic, user quotas, name normalization and collisions, and input validation. `make check` includes the race detector and requires a C compiler. GitHub Actions runs these checks and builds the binary and Docker image on pushes to `stable` and pull requests.

## AI skills

Give your assistant the skill folder for the task, or install that folder using your assistant's supported skill mechanism:

- [lcl-preview](skills/lcl-preview/SKILL.md): open, verify, and close public previews, including names, fixed timeouts, HTTPS, and container clients.
- [lcl-deploy](skills/lcl-deploy/SKILL.md): install or update the broker and client while preserving the existing Traefik setup.

Each folder contains a `SKILL.md` entrypoint and optional UI metadata. Share the entire folder so any bundled references remain available. The skills are written in English and contain no server-specific configuration. Adding a skill supplies instructions; installing or updating the software is a separate operation.

Example requests for assistants that support `$skill-name` invocation:

```text
Use $lcl-preview to share my app on port 3000 as "First version" for 20 minutes.
Use $lcl-deploy to update my existing lcl installation from stable.
```

## Access model

URLs are public. A random hostname is not authentication. Any local user with socket access can publish an HTTP service accessible in their network namespace, including services started by other users. Use this on servers whose local users are trusted to publish those services.

The client connects to `127.0.0.1` and relays connections over the Unix socket. This also supports applications bound to localhost inside a development container; see [container terminals](docs/operations.md#container-terminals). Relay connections use one-time random tokens and must belong to the session's Linux UID.

The broker writes one generated file, `lcl.yaml`, in the configured dynamic directory and does not receive the Docker socket. Other Traefik configuration files remain untouched. Slugs contain 64 random bits from the operating system's cryptographic random source.

## License

[MIT](LICENSE).
