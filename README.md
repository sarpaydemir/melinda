# dost

Self-hosted AI teammate for Mattermost, written in Go.

dost answers direct messages and mentions with any OpenAI-compatible model, keeps conversation and long-term
memory in PostgreSQL with pgvector, learns shared knowledge from public channels, reads attachments, searches the
web, generates images, and works with Plane. The agent's name, persona, language, and prompts are configuration,
so one deployment model serves any number of agents.

Status: pre-release.

## Build

```bash
make build    # static binary at bin/melinda
make check    # gofmt, go vet, staticcheck, tests with -race
```

Integration tests run against a disposable pgvector container: `make test-integration`.

## License

[Apache 2.0](LICENSE)
