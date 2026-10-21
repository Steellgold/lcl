# Standard lcl deployment

Project source: https://github.com/Steellgold/lcl

## Requirements and configuration

The standard deployment uses Linux, Docker Compose, an existing Traefik instance with its file provider enabled, and wildcard DNS for the chosen domain. Find the actual network and watched directory in that Traefik installation. The broker joins that network; it publishes no host TCP port.

For a new installation, obtain the requested revision. With no revision preference:

```sh
git clone --branch stable https://github.com/Steellgold/lcl.git
cd lcl
cp .env.example .env
```

Reuse existing configuration when upgrading. The required Compose settings are:

| Setting | Purpose |
| --- | --- |
| `LCL_DOMAIN` | Parent domain whose wildcard DNS points to this server. |
| `TRAEFIK_DYNAMIC_DIR` | Existing host directory watched by Traefik. |
| `TRAEFIK_NETWORK` | Existing Docker network shared with Traefik. |

The usual internal listener is `0.0.0.0:19473`, with `LCL_BACKEND_URL=http://lcl-broker:19473`. Do not use container localhost to reach another container or the host. Match `LCL_HTTP_ENTRYPOINT` and `LCL_HTTPS_ENTRYPOINT` to the proxy. `LCL_CERT_RESOLVER` selects an existing resolver; leave it empty for a certificate already loaded by Traefik. Repeated certificates for random HTTPS names consume issuance allowances; a wildcard certificate can avoid that.

Match `LCL_UID` and `LCL_GID` to an identity that can write the dynamic directory and runtime directory. The container drops capabilities, so UID 0 does not bypass another owner's private directory. Preserve existing proxy directory permissions rather than changing ownership across all applications.

The runtime directory is normally mounted at `/run/lcl`, with traversal available to client users. Mount the entire Traefik dynamic directory: `lcl.yaml` is updated by atomic replacement. Do not edit generated routes manually or mount the Docker socket into the broker.

## Build, stage, and upgrade

For an existing checkout, inspect local changes and use `git pull --ff-only` on the chosen branch. Validate against available CI results or `make check`; the latter needs Go 1.26 and a C compiler for the race detector. There are no external Go module dependencies.

Build before interrupting the service:

```sh
docker compose build
mkdir -p bin
container_id=$(docker create lcl:local)
docker cp "$container_id:/usr/local/bin/lcl" bin/lcl
docker rm "$container_id"
```

Once artifacts are ready, apply the authorized update:

```sh
docker compose up -d
```

For a standard global executable, atomic replacement can use:

```sh
sudo install -m 0755 bin/lcl /usr/local/bin/lcl.next
sudo mv /usr/local/bin/lcl.next /usr/local/bin/lcl
```

Adapt this to the existing binary location and available administrator access. Preserve wrappers, including any socket environment variables they provide. Update separate client environments only when included in the user's request.

`LCL_IDLE_TIMEOUT` is obsolete. Current sessions are unlimited by default; `-t` or `--timeout` specifies a fixed lifetime in minutes. A restart clears existing routes, so users must relaunch their preview commands.

## Container clients

The client connects to localhost in its own network namespace. A development container needs the binary and a shared control socket, not host networking or a published application port.

An optional second broker socket uses `LCL_EXTRA_SOCKET`. Set it in the broker's environment and mount a shared runtime directory using a local Compose override. Mount the same directory into the development container and set that client's `LCL_SOCKET` to its in-container socket path. Verify traversal and socket access for the client's UID.

## Verify and troubleshoot

Check `docker compose ps`, `docker compose logs --tail 100 broker`, and the intended client's `lcl --version` and `lcl --help`.

For a public smoke check, create a small temporary HTTP fixture on an unused local port in the client environment. Run `lcl <port> -t 1`, fetch the printed URL, and check the expected response body. Then send Ctrl+C or SIGINT to that client; its URL should stop serving the fixture. Clean up only this test's processes. Do not publish an unrelated private service as a deployment test.

If the broker works locally but the public URL fails, check Traefik's DNS resolution and connection to `lcl-broker:19473` from the proxy network. For file permission errors, check the configured UID/GID and both writable mounts. For missing client sockets, check the mount paths in that client's container.
