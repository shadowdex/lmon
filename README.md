# lmon

LLM usage, latency and endpoint monitor. One static binary, no runtime.

```bash
go install github.com/shadowdex/lmon@latest   # or download a release binary

lmon proxy                      # local recording proxy on 127.0.0.1:8787
export ANTHROPIC_BASE_URL=http://localhost:8787/anthropic
export OPENAI_BASE_URL=http://localhost:8787/openai/v1

lmon stats --since 24h          # tokens, cache hit rate, latency (avg/p50/p95/TTFB)
lmon top                        # live view: w window, s sort, p pause, q quit
lmon prices update              # one-time: download model prices (enables cost estimates)
lmon prices show anthropic claude-sonnet-5-5   # the rates lmon will use
lmon geoip update               # one-time: download the free DB-IP city database
lmon probe api.anthropic.com    # DNS per resolver, IP geo, TCP/TLS/TTFB timing
```

Providers: anthropic, openai, xai, groq, mistral, deepseek, openrouter
(`/<provider>/...`). Add one in `internal/usage/usage.go`.

- `lmon top` tails the same log (5m/15m/1h/24h windows, per-model latency trend). It needs a
  terminal and drops columns on narrow ones; honors `NO_COLOR`.
- **Cost is an estimate, not a bill.** It uses list prices at standard rates: batch, priority
  and flex tiers are not modeled. It covers uncached/cached input, cache writes (5-minute and
  1-hour), output, and the higher rates providers charge above a context length (e.g. 200k).
  Cost is computed when displayed, so `lmon prices update` also reprices old events. A `*`
  means some calls couldn't be priced (unknown model, or no usage in the response), so the
  figure is a lower bound; `n/a` means none could. Failed calls cost nothing. Models are matched
  by exact name or name minus a snapshot date, never by prefix, so an unlisted model shows
  `n/a` instead of a wrong price.
- Events are appended to `~/.lmon/events.jsonl`. Prompts and responses are never stored.
  The log rotates by size (`lmon proxy --max-size-mb 50 --keep 3`, about 200 MB at most;
  `--max-size-mb 0` disables it). `stats` and `top` read the rotated files too. Run one
  proxy per log file.
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
Model prices from [LiteLLM](https://github.com/BerriAI/litellm)'s community price list (MIT).

## License

MIT, see [LICENSE](LICENSE).
