# Contributing

Open an issue for changes that affect deployment, configuration, or session behavior before starting a large patch.

Use Go 1.26 or later. Run `make check` and `make build` before submitting a pull request against `stable`. Keep code, messages, and documentation in English. Use example domains and addresses in documentation; keep machine-specific configuration in an ignored `.env` file.

Tests should cover observable behavior, especially route cleanup, activity tracking, and connection handling. The broker should remain usable without external Go dependencies or access to the Docker socket.
