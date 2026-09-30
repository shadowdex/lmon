# lmon

LLM usage, latency and endpoint monitor. One static binary, no runtime.

```bash
go install github.com/shadowdex/lmon@latest   # or download a release binary

lmon proxy                      # local recording proxy on 127.0.0.1:8787
export ANTHROPIC_BASE_URL=http://localhost:8787/anthropic
export OPENAI_BASE_URL=http://localhost:8787/openai/v1

lmon stats --since 24h          # tokens, cache hit rate, latency (avg/p50/p95/TTFB)
lmon geoip update               # one-time: download the free DB-IP city database
lmon probe api.anthropic.com    # DNS per resolver, IP geo, TCP/TLS/TTFB timing
```

Providers: anthropic, openai, xai, groq, mistral, deepseek, openrouter
(`/<provider>/...`). Add one in `internal/usage/usage.go`.

- Events are appended to `~/.lmon/events.jsonl`. Prompts and responses are never stored.
- Cache hit rate = cache-read tokens / total input tokens. `input_tokens` is always
  uncached input; OpenAI's cached tokens are subtracted out to match Anthropic.
- OpenAI chat streaming only reports usage if the request sets
  `stream_options.include_usage`.
- Geo uses the free [DB-IP Lite](https://db-ip.com) city database (CC BY 4.0, no account
  needed, ~120 MB, published monthly). `lmon geoip update` downloads it to `~/.lmon/`; lmon
  never bundles or redistributes it. To use your own City `.mmdb` (e.g. MaxMind GeoLite2),
  pass `--geoip FILE` or set `LMON_GEOIP_DB`.
- The proxy binds to loopback only because it forwards your API keys.
- Release: `goreleaser release --clean` (builds macOS/Linux/Windows, amd64/arm64).

## Attribution

IP geolocation by [DB-IP.com](https://db-ip.com) (CC BY 4.0).

## License

MIT, see [LICENSE](LICENSE).
