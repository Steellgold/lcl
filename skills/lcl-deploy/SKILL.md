---
name: lcl-deploy
description: >-
  Install, configure, update, or diagnose the lcl preview broker on Linux with
  an existing Traefik proxy. Use for lcl deployment and upgrade requests,
  including Coolify installations, rather than ordinary preview sessions.
---

# lcl Deployment

## Establish the deployment context

Identify the installation checkout, selected revision, active Compose project, local configuration, and client binary or launcher. Use the user's existing deployment; a development clone may be a different directory. Check Git status before updating it.

Read [references/deployment.md](references/deployment.md) for installation, upgrade, and verification details. The checked-out project's README and Compose file take precedence when they differ from these standard deployment examples.

Installation or upgrade requests authorize work on that deployment. Explain that a broker restart ends active previews and respect authorization already given. For diagnosis alone, inspect configuration and logs before proposing changes.

## Preserve the existing setup

- Keep the configured domain, Traefik network, entrypoints, resolver, UID/GID, and socket mounts unless the task calls for a change. Do not replace the existing reverse proxy.
- Keep `.env` and local Compose overrides private. Inspect needed values without dumping credentials or unrelated environment settings.
- Pull the selected branch with `git pull --ff-only`; `stable` is the project's usual branch. Honor a requested tag or revision. If local changes or divergent history block the update, report the state instead of resetting or discarding work.
- Treat source changes and Git publication as separate tasks from deployment. Do not patch project code or push commits merely to complete an upgrade.

## Prepare before restarting

Build the selected image and stage its matching client binary before replacing the running service. Use existing CI results or project checks to validate the revision when appropriate. Preserve client launchers: a wrapper may supply `LCL_SOCKET`, so replace its underlying executable rather than overwriting the wrapper.

After the authorized restart, install the matching client in each requested execution environment. Use atomic replacement so running clients do not prevent the update. Keep temporary extraction containers and fixtures scoped to this deployment and remove them after use.

## Verify and report

Check service status, startup logs, and the installed client's `--version` and `--help`. Verify a public URL with a temporary HTTP fixture, then stop that fixture's client and confirm the URL closes. Exercise HTTPS only when TLS setup or validation is in scope.

Report the deployed revision, client version, verification result, and which active sessions need to be recreated. If deployment cannot proceed, state the exact missing configuration or permission without claiming the service is ready.
