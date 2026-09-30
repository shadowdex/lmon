# lmon

LLM usage, latency and endpoint monitor. One static binary, no runtime.

```bash
go install github.com/shadowdex/lmon@latest   # or download a release binary

lmon proxy                      # local recording proxy on 127.0.0.1:8787
export ANTHROPIC_BASE_URL=http://localhost:8787/anthropic
export OPENAI_BASE_URL=http://localhost:8787/openai/v1

lmon stats --since 24h          # tokens, cache hit rate, latency (avg/p50/p95/TTFB)
lmon probe api.anthropic.com    # DNS per resolver, IP geo, TCP/TLS/TTFB timing
```

Providers: anthropic, openai, xai, groq, mistral, deepseek, openrouter
(`/<provider>/...`). Add one in `internal/usage/usage.go`.

- Events are appended to `~/.lmon/events.jsonl`. Prompts and responses are never stored.
- Cache hit rate = cache-read tokens / total input tokens. `input_tokens` is always
  uncached input; OpenAI's cached tokens are subtracted out to match Anthropic.
- OpenAI chat streaming only reports usage if the request sets
  `stream_options.include_usage`.
- Geo needs a GeoLite2-City `.mmdb` (`--geoip` or `LMON_GEOIP_DB`).
- The proxy binds to loopback only because it forwards your API keys.
- Release: `goreleaser release --clean` (builds macOS/Linux/Windows, amd64/arm64).

## License

MIT, see [LICENSE](LICENSE).
