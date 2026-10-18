# Switchboard

An OpenAI-compatible LLM gateway in Go: one `/v1/chat/completions` endpoint in front of several model providers,
with routing, fallback, rate limits, caching and cost accounting, measured against local mock providers.

**Status: day 1 of 5** (design and scaffold). See [DESIGN.md](DESIGN.md).

## Run the checks

```bash
make check
```

## License

MIT
