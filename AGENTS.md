# Repository Guidelines

## Project Structure & Module Organization

This Linux CLI uses Go 1.26 and the standard library. `main.go` contains the client, broker, Unix socket protocol, HTTP proxy, and session lifecycle. `main_test.go` contains integration-style tests and shared helpers. There is no frontend or asset directory.

`README.md` covers installation and usage; `docs/operations.md` describes deployment and configuration. `Dockerfile` builds the static binary and minimal runtime image. `compose.yaml` connects the broker to Traefik. `.github/workflows/ci.yml` defines CI.

## Build, Test, and Development Commands

- `make build`: compile a static executable into `bin/lcl`.
- `make test`: run all tests with the race detector.
- `make check`: check formatting, run `go vet`, and execute race-enabled tests. A C compiler is required.
- `gofmt -w *.go`: format Go source files.
- `go test -run TestClientRelay -v ./...`: run one focused test.
- `docker compose up -d --build`: build and start the broker after configuring `.env`, Traefik, and wildcard DNS.
- `bin/lcl 3000`: publish an existing local HTTP application through a running broker.

## Coding Style & Naming Conventions

Use `gofmt` formatting, including tabs for Go indentation. Follow Go naming conventions: exported identifiers use PascalCase, internal identifiers use camelCase, and tests use `TestBehavior` names. Keep code, messages, comments, and documentation in English. Keep dependencies limited to the standard library and configuration in environment variables rather than deployment-specific constants.

## Testing Guidelines

Use Go's `testing` package and `net/http/httptest`. Cover observable behavior: forwarding, container relays, WebSockets, readiness, idle expiration, cleanup, and quotas. Use temporary directories and ephemeral ports; close sockets and servers during cleanup. No coverage percentage is enforced. Run `make check` and `make build` before submitting changes. Use isolated broker configurations: restarting the deployed broker closes active sessions.

## Commit & Pull Request Guidelines

Follow `type(scope): description` in English, as in `feat(proxy): add idle expiry` or `docs(setup): clarify installation`. Target `stable`. Describe the problem, resulting behavior, and validation; link relevant issues. Update documentation and `.env.example` when configuration changes. Discuss large deployment or protocol changes in an issue first.

## Security & Configuration

Keep `.env`, local Compose overrides, binaries, credentials, and generated routes out of Git. Use example domains in documentation. Preserve UID authentication, one-time relay tokens, and session limits. The broker must operate without Docker socket access.
