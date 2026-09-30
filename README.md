```text
██╗     ███╗   ███╗ ██████╗ ███╗   ██╗
██║     ████╗ ████║██╔═══██╗████╗  ██║
██║     ██╔████╔██║██║   ██║██╔██╗ ██║
██║     ██║╚██╔╝██║██║   ██║██║╚██╗██║
███████╗██║ ╚═╝ ██║╚██████╔╝██║ ╚████║
╚══════╝╚═╝     ╚═╝ ╚═════╝ ╚═╝  ╚═══╝
```

**LLM usage, latency and endpoint monitor.** One static binary, any provider, no runtime.

[![CI](https://github.com/shadowdex/lmon/actions/workflows/ci.yml/badge.svg)](https://github.com/shadowdex/lmon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/shadowdex/lmon?include_prereleases)](https://github.com/shadowdex/lmon/releases)
[![License: MIT](https://img.shields.io/github/license/shadowdex/lmon)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/shadowdex/lmon)](go.mod)

[Install](#install) · [Use](#use) · [Prometheus](#prometheus) · [Releases](https://github.com/shadowdex/lmon/releases)

## Install

**macOS and Linux**

```bash
curl -fsSL https://raw.githubusercontent.com/shadowdex/lmon/main/install.sh | sh
```

Installs the latest release into `~/.local/bin` (no sudo) after checking its SHA-256 against
the release's `checksums.txt`. Re-run it to upgrade. `LMON_VERSION=v0.1.0` pins a version
(pre-releases must be named explicitly), `LMON_INSTALL_DIR=/usr/local/bin` changes the
directory, and `LMON_DRY_RUN=1` shows what it would download. Read it first if you like:
[install.sh](install.sh). If the download is cut off, nothing runs.

**Any platform with Go 1.27.1 or newer**

```bash
go install github.com/shadowdex/lmon@latest
```

**Windows, or by hand:** download the `.zip` or `.tar.gz` for your platform from
[Releases](https://github.com/shadowdex/lmon/releases), unpack it and put `lmon` on your `PATH`.
On macOS use `curl` (or `xattr -d com.apple.quarantine lmon`): the binaries aren't notarized, so a
browser download is quarantined. Windows SmartScreen may warn for the same reason.

Everything lmon stores (event log, price table, GeoIP database) is under `~/.lmon`; delete the
binary and that folder to uninstall.

## Use

```bash
lmon proxy                      # local recording proxy on 127.0.0.1:8787
export ANTHROPIC_BASE_URL=http://localhost:8787/anthropic
export OPENAI_BASE_URL=http://localhost:8787/openai/v1

lmon stats --since 24h          # tokens, cache hit rate, latency (avg/p50/p95/TTFB)
lmon top                        # live view: w window, s sort, p pause, q quit
lmon stats --claude             # also count Claude Code sessions (reads its own logs)
lmon top --claude               # same, live
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
- **Claude Code sessions** (`--claude`) are read straight from its logs
  (`$CLAUDE_CONFIG_DIR`, else `~/.claude/projects`, including subagent files), so nothing has
  to go through the proxy. Only usage numbers, model, response id and timestamp are read;
  prompts and replies are never kept. A response spans several log lines whose
  `output_tokens` grow while it streams, so responses are merged by `message.id` taking the
  largest value, and a call seen by both the proxy and Claude Code is counted once (the
  proxy's copy wins because it has latency). Imported calls have no latency (shown as `-`).
  **Treat these figures as approximate and usually slightly low.** Claude Code's logs don't
  contain every billed call. Checked against the totals Claude Code records itself, across
  55 sessions of one user, the logs accounted for 93% of the cost (per session: 69% to 106%,
  median 91%). The per-response token counts and prices matched exactly where the logs were
  complete; I could not tell why some billed usage is missing from the logs.
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

## Prometheus

`lmon proxy` serves Prometheus metrics at `http://127.0.0.1:8787/metrics`. To scrape from a
Prometheus or Grafana running in Docker, which can't reach the host's loopback, add
`--metrics-addr 0.0.0.0:9464` (the metrics hold no API keys but do show models and usage).
Add `--probe-host api.anthropic.com,api.openai.com` to probe endpoints in the background
(`--probe-every 60s`; this makes outbound HTTPS requests).

| Metric | Labels |
|---|---|
| `lmon_requests_total` | provider, model, code |
| `lmon_tokens_total` | provider, model, type = input, output, cache_read, cache_write |
| `lmon_cost_usd_total`, `lmon_unpriced_requests_total` | provider, model (needs `lmon prices update`) |
| `lmon_request_duration_seconds`, `lmon_time_to_first_byte_seconds` (histograms) | provider, model |
| `lmon_web_search_requests_total`, `lmon_inflight_requests`, `lmon_build_info` | |
| `lmon_probe_success`, `_http_status`, `_phase_seconds`, `_dns_seconds`, `_dns_success`, `_dns_answers`, `_ip_info`, `_last_run_timestamp_seconds` | host, resolver, phase, ip, country, city |

```yaml
scrape_configs:
  - job_name: lmon
    static_configs: [{targets: ['host.docker.internal:9464']}]
```

```promql
sum by (model) (increase(lmon_cost_usd_total[1h]))                          # spend per model
histogram_quantile(0.95, sum by (le, model) (rate(lmon_request_duration_seconds_bucket[5m])))
sum(rate(lmon_tokens_total{type="cache_read"}[5m])) / sum(rate(lmon_tokens_total{type=~"input|cache_read|cache_write"}[5m]))   # cache hit ratio
sum(rate(lmon_requests_total{code=~"4..|5.."}[5m])) / sum(rate(lmon_requests_total[5m]))   # error rate
lmon_probe_success == 0                                                     # endpoint unreachable
```

`lmon_probe_ip_info` has one series per resolved address, labeled with its location, so the
endpoint moving shows up as series appearing and disappearing. Only calls through the proxy
are counted (Claude Code's own logs are not part of `/metrics`). The price table reloads by
itself after `lmon prices update`. At most 200 distinct models get their own series; the rest
are folded into `model="other"`. The exposition is hand-written (no client library) and is
checked with Prometheus' `promtool check metrics`.

## Attribution

IP geolocation by [DB-IP.com](https://db-ip.com) (CC BY 4.0).
Model prices from [LiteLLM](https://github.com/BerriAI/litellm)'s community price list (MIT).

## License

MIT, see [LICENSE](LICENSE).
