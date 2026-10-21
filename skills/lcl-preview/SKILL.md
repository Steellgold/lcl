---
name: lcl-preview
description: >-
  Publish and manage public preview URLs for local HTTP apps using the lcl Linux CLI.
  Use when a user wants to share an app through lcl, choose a preview name, set a
  lifetime, enable public HTTPS, or close a preview. Broker installation is separate.
---

# lcl Preview

## Find the application and client

Check `command -v lcl` and `lcl --help` in the environment where the application runs. Confirm the requested app responds over HTTP at `127.0.0.1:<port>` there before creating a public route.

Localhost belongs to the **client's network namespace**. For a containerized app, run `lcl` inside that container with access to the broker's shared Unix socket. An installed client on the host does not automatically reach an app bound to a container's localhost. `LCL_SOCKET` selects a non-default control socket.

Publishing a URL makes the app public; the generated hostname is not authentication. Use the app and port the user intended to share.

## Open the requested preview

Use the options supported by the installed version:

```sh
lcl 3000
lcl 3000 --name "First version" -t 20
lcl 3000 --https
```

- With no timeout, the session stays open until the client stops or disconnects. There is no idle expiry.
- `-t N` and `--timeout N` impose a fixed lifetime in positive whole minutes, including route setup. Traffic does not extend it. Add a timeout when the user requests one.
- `--name` selects a label under the broker's configured domain. Latin accents are folded, spaces and punctuation become hyphens, and the normalized label must fit 63 characters. Preserve the chosen name; report collisions rather than terminating someone else's session or silently renaming it.
- `--https` enables public TLS. The local app must still speak HTTP. Certificates must already be available or issued by the configured Traefik resolver.

Keep `lcl` running in a managed terminal session and retain its handle or PID. Let the tool yield while the process continues. If the execution tool cannot preserve a process after responding, give the user the terminal command and explain that they need to keep it running.

## Verify and hand over

Wait for the URL printed by `lcl`; HTTP sessions check route readiness before printing it. Fetch that exact URL and confirm the intended app responds. HTTPS may need additional certificate issuance time. Return the URL, requested lifetime, and how to stop the session. Leave the preview running when the user intends to use it.

Stop the intended client using its terminal handle, Ctrl+C, or targeted SIGINT. Closing it removes the route and active connections. Retain the handle when you start a client; avoid blanket process kills that would close unrelated sessions. There are no global list/stop flags.

## Diagnose a failed preview

- Local connection failure: check the app's port and the client's network namespace.
- Service unavailable: check the control socket, `LCL_SOCKET`, permissions, and any container mount.
- Route not ready: check wildcard DNS and Traefik-to-broker connectivity. A chosen name may belong to another application.

A preview failure alone is not a reason to restart the shared broker: a restart closes all users' sessions.
