# refurb-sentry

**English** · [简体中文](README.zh-CN.md)

[![CI](https://github.com/hh-io/refurb-sentry/actions/workflows/ci.yml/badge.svg)](https://github.com/hh-io/refurb-sentry/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/hh-io/refurb-sentry)](https://github.com/hh-io/refurb-sentry/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/hh-io/refurb-sentry)](https://goreportcard.com/report/github.com/hh-io/refurb-sentry)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Watches Apple's **Certified Refurbished** store for new listings, price drops, and removals, filters them by multi-dimensional hardware specs, and pushes actionable alerts to Bark, Telegram, Feishu, WeCom, DingTalk, and more.

Single static binary, runs as a lightweight daemon, keeps state in one JSON file atomically so restarts lose nothing.

<p align="center">
  <img src="assets/notification.png" width="320"
       alt="Bark notifications on an iPhone lock screen (English)">
  &nbsp;&nbsp;&nbsp;&nbsp;
  <img src="assets/notification.zh-CN.png" width="320"
       alt="Bark notifications on an iPhone lock screen (Chinese)">
</p>

> [!NOTE]
> Notification text is available in English (`notify.lang: en`) and Chinese (`zh-CN`, default). Log output and error messages are always Chinese (designed for operators and troubleshooting). Product titles come directly from the monitored Apple store (e.g. watching the US store yields English titles; watching the CN store yields Chinese titles).

---

## Table of Contents

- [Key Features](#key-features)
- [Quick Start](#quick-start)
- [Notification Preview](#notification-preview)
- [Coverage](#coverage)
- [Filter Rules](#filter-rules)
- [Delivery Channels](#delivery-channels)
- [Configuration & Networking](#configuration--networking)
- [Production Deployment](#production-deployment)
- [Reliability & State](#reliability--state)
- [Development](#development)
- [Disclaimer](#disclaimer)
- [License](#license)

---

## Key Features

- ⚡️ **Near Real-time Monitoring**: Tracks 3 distinct event kinds: new listings, price drops, and delistings. Polls every 120s, the floor imposed by the CDN's cache TTL — see [Request Frequency](#request-frequency--cdn-caching).
- 🎯 **Precise Spec Filtering**: Filter by model, chip (`M2`, `M3`, `M4 Pro`, `M4 Max`, etc.), CPU/GPU core counts, memory, storage capacity, colour, release year, and price range.
- 📱 **Multi-channel Delivery**: Built-in Bark support and a generic webhook (ready for Telegram, Discord, Feishu, WeCom, DingTalk, ServerChan, etc.). Automatic digest bundling prevents restock spam.
- 🌍 **19 Regions Supported**: North America, Europe, East Asia and Oceania — full list under [Coverage](#coverage).
- 🛡️ **Lightweight & Dependable**: Pure Go static binary, no database — state is one JSON file, and the container image is ~17MB. Features **silent initial baselines**, **fetch failures that never cause delistings**, and **round-wide rollback when delivery fails**.

### Out of Scope (Explicit Boundaries)

- **No inventory unit counts**: The official data source only signals presence ("in list = available"); stock quantities do not exist.
- **No price history charts**: State only tracks current prices to detect price drops.
- **No anti-bot evasion**: Listing pages are public static assets served via CDN; proxy pools and UA rotation are unnecessary.
- **No auto-checkout or sniping**: This tool is strictly an alert daemon. Feature requests for automated purchasing will not be accepted.

---

## Quick Start

### 1. Installation

#### macOS (Recommended via Homebrew)

```bash
brew install --cask hh-io/tap/refurb-sentry
```

Upgrade anytime with `brew upgrade --cask refurb-sentry`. The binary is not notarised by Apple; the cask automatically removes the quarantine attribute during installation to bypass Gatekeeper prompts.

#### Linux / VPS (Pre-built Binaries)

Download pre-built archives from the [latest release](https://github.com/hh-io/refurb-sentry/releases/latest) (supports Linux & macOS on amd64, arm64, armv7). The archive includes the binary, example config, and systemd/launchd service units:

```bash
tar xzf refurb-sentry_*_linux_amd64.tar.gz
```

#### Docker

Prefer running containers? Use our official image: `ghcr.io/hh-io/refurb-sentry` (see [Production Deployment → Docker](#docker-compose)).

<details>
<summary>Install from source (Requires Go 1.26+)</summary>

```bash
go install github.com/hh-io/refurb-sentry/cmd/refurb-sentry@latest

# Or clone and build (includes example configuration):
git clone https://github.com/hh-io/refurb-sentry && cd refurb-sentry
go build -o refurb-sentry ./cmd/refurb-sentry
```
</details>

---

### 2. Configuration & Dry Run (3 Steps)

```bash
# 1. Create a local config and export your notification credential (e.g. Bark)
cp configs/config.example.yaml configs/config.yaml
export BARK_KEY=your_bark_device_key

# 2. Inspect available filter dimensions and active product values
./refurb-sentry -config configs/config.yaml -list-dims
# Add -skeleton when writing rules for the first time, for a paste-ready starting point
./refurb-sentry -config configs/config.yaml -list-dims -skeleton

# 3. Dry-run one round: prints parsed notifications to stdout (no state written, no alerts sent)
./refurb-sentry -config configs/config.yaml -once -dry-run

# 4. Start as a persistent daemon
./refurb-sentry -config configs/config.yaml
```

> [!TIP]
> `configs/config.example.yaml` is a **minimal config** of about a dozen lines: required keys only, everything else falls back to built-in defaults (120s polling, state in `data/state.json`, Chinese notification wording, and so on). When you need a proxy, multiple regions, other notification channels, or finer HTTP tuning, copy the relevant snippet from `configs/config.full.yaml` — that file lists every option along with the reasoning behind it.

> [!IMPORTANT]
> **The first run only records a baseline and sends zero notifications.** This prevents hundreds of pre-existing listings from flooding your device. Notifications will begin from the second round onward when actual changes occur.

---

### 3. CLI Flags

| Flag | Description |
|---|---|
| `-config` | Path to configuration file (default: `configs/config.yaml`) |
| `-once` | Run a single round and exit |
| `-dry-run` | Dry-run mode: prints notifications to stdout without modifying state or sending alerts |
| `-list-dims` | Prints currently available dimensions and values for configured regions/categories |
| `-skeleton` | With `-list-dims`, also prints a paste-ready rule skeleton for the `rules:` block; given alone it implies `-list-dims` |
| `-version` | Print version information |

---

## Notification Preview

### Single Price Drop Alert

```text
Price drop · US mac
Refurbished 24-inch iMac Apple M4 Chip with 10-Core CPU and 10-Core GPU - Silver
$2,134.80 → $1,779 (down $355.80, 16.7%)
Matched rule: iMac under 2k
https://www.apple.com/shop/product/...
```

### Digest Alert (Triggered when a round's changes exceed `notify.digest_threshold`, default 5)

```text
Refurb watch · 6 listings / 2 price drops
[Listed] US Refurbished Mac mini Apple M4 chip $499
[Price drop] US Refurbished 14-inch MacBook Pro $1,999 → $1,799
...
```

---

## Coverage

- **19 Regions**: `AU` `BE` `CA` `CH` `CN` `DE` `ES` `FR` `HK` `IE` `IT` `JP` `KR` `NL` `NZ` `SG` `TW` `UK` `US`
- **8 Categories**: `mac` `ipad` `iphone` `watch` `airpods` `appletv` `homepod` `accessories`

> [!NOTE]
> **The region × category matrix is sparse** (not every combination exists on Apple's site):
> - `CN` and `HK` have no `iphone` or `appletv` stores (requests return HTTP 404).
> - `US` returns HTTP 200 for `appletv`, `airpods`, and `homepod`, but the page carries no catalog items.
>
> Unavailable combinations are checked during initial startup and fail loudly to avoid partial baselines. Note that `MX` and `IN` do not have refurbished stores.

---

## Filter Rules

### 1. Discover Active Dimensions

Available dimensions vary by category (e.g. `mac` has memory/capacity, `watch` has case size/material). Inspect active values directly from Apple before writing rules:

```console
$ ./refurb-sentry -config configs/config.yaml -list-dims

===== US/mac(209 件)=====
  refurbClearModel       (Models)   display, imac, macbookair, macbookneo, macbookpro, macmini, macstudio
  dimensionScreensize    (Sizes)    13inch, 14inch, 15inch, 16inch, 24inch, 27inch
  dimensionRelYear       (Release Year) 2022, 2023, 2024, 2025, 2026
  dimensionColor         (Finish)   blue, midnight, silver, space_gray, spaceblack, starlight, ...
  tsMemorySize           (Memory)   128gb, 16gb, 24gb, 32gb, 36gb, 48gb, 64gb, 8gb
  dimensionCapacity      (Capacity) 1tb, 256gb, 2tb, 4tb, 512gb, 8tb
  chips                  (芯片)   A18 Pro, M2, M4, M4 Max, M4 Pro, M5, M5 Max, M5 Pro

  ----- 规则骨架(整段复制到配置的 rules: 下,再删掉不要的取值)-----
  - name: US mac
    regions: [US]
    categories: [mac]
    dimensions:
      refurbClearModel: [display, imac, macbookair, macbookneo, macbookpro, macmini, macstudio]
      dimensionScreensize: [13inch, 14inch, 15inch, 16inch, 24inch, 27inch]
      dimensionRelYear: [2022, 2023, 2024, 2025, 2026]
      dimensionColor: [blue, midnight, silver, space_gray, spaceblack, starlight]
      tsMemorySize: [128gb, 16gb, 24gb, 32gb, 36gb, 48gb, 64gb, 8gb]
      dimensionCapacity: [1tb, 256gb, 2tb, 4tb, 512gb, 8tb]
    chips: [A18 Pro, M2, M4, M4 Max, M4 Pro, M5, M5 Max, M5 Pro]
```

The labels in parentheses come from the Apple store page itself, so they arrive in
that region's language. `chips` is computed by this tool rather than read off the
page, so it is always labelled `芯片` — as is the `件` in the header and the skeleton
banner, which are part of the operator-facing console output. The keys and values on
either side are stable identifiers, and those are what go into a rule.

The block below the table is a **paste-ready rule skeleton**: every key and value in
it is currently in stock for that region/category, so you can copy the whole thing
under `rules:` in your config and start editing. Listing every value is equivalent to
no filtering at all — **the work is deleting, not adding**. Pare it down to the values
you actually want and the rule starts doing something. `min_cpu_cores` and `max_price`
have no enumerable values, so they appear as commented lines to uncomment when needed.
Dimensions with nothing in stock are left out of the skeleton: writing one into a rule
would add a condition that can never match.

### 2. Rule Example

Add filter rules under the `rules` key in `configs/config.yaml`:

```yaml
rules:
  - name: High-end MacBook Pro
    regions: [US]
    categories: [mac]
    dimensions:
      refurbClearModel: [macbookpro]
      tsMemorySize: [24gb, 36gb, 48gb]
      dimensionCapacity: [1tb, 2tb]
    chips: [M4 Pro, M4 Max, M5 Pro, M5 Max]
    min_cpu_cores: 12
    max_price: 2500

  - name: Budget Mac mini
    regions: [US]
    categories: [mac]
    dimensions:
      refurbClearModel: [macmini]
    max_price: 600
```

### 3. Rule Fields Reference

| Field | Type | Description |
|---|---|---|
| `name` | string | Rule label displayed in the notification's "Matched rule" line (defaults to `rule#N`) |
| `regions` | list | Target region codes; empty matches all configured regions |
| `categories` | list | Target categories; empty matches all configured categories |
| `dimensions` | map | Page dimensions (keys vary by category; check with `-list-dims`) |
| `chips` | list | Chip models (e.g. `M4 Pro`), parsed from product titles |
| `min_cpu_cores` | int | Minimum CPU core count, parsed from titles |
| `min_gpu_cores` | int | Minimum GPU core count, parsed from titles |
| `title_match` | regex | Regular expression matched against **normalised** title text |
| `min_price` | number | Lower price limit (inclusive); `0` means no limit |
| `max_price` | number | Upper price limit (inclusive); `0` means no limit |

**Evaluation Semantics**:
- Rules are joined by **OR** (matching any rule triggers notification).
- Fields within a single rule are joined by **AND**.
- Values within a single field list are joined by **OR**; omitted fields impose no constraint; matching is case-insensitive.
- **Strict matching policy**: Missing dimensions on a product do not match; unparseable core counts satisfy no `min_*_cores` threshold (safe-by-default to prevent false matches).
- Filtering occurs strictly at **notification time**. The underlying state stores all products, so loosening rules later will not cause previously-seen items to be flagged as new listings.

### 4. Chip and Core Count Parsing

Model, memory, and capacity come from structured page fields and are fairly reliable
(one exception is covered in the next section). But
**chip models and CPU/GPU core counts exist only inside the product title**, and the
word order differs completely between localised stores:

```text
US  Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU
FR  Mac mini reconditionné avec puce Apple M4, CPU 10 cœurs, GPU 10 cœurs
JP  14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ
```

The parser does not dispatch a regex per region: it decouples the chip *keyword* from
the *model* and accepts both word orders, so adding a region usually needs no parser
change. Measured across 2,000+ real products in all 19 regions: **chip 100%, core
count 95%** (the remaining 5% are titles that simply omit core counts in Apple's copy).

Apple also mixes regular spaces, non-breaking spaces (U+00A0) and several Unicode
hyphens (U+2011, U+2014) within a single page — the Spanish, Italian and French stores
separate `M4 Pro` with U+00A0 while other models on the same page use a plain space.
Titles are therefore normalised before matching, and `title_match` runs against the
normalised text too, so plain ASCII spaces and hyphens are all a regex needs.

Even so, a copy change on Apple's side can break this. When parsing fails the product
is still tracked (chip recorded as unknown), but rules using `chips`, `min_cpu_cores`
or `min_gpu_cores` will skip it. If notifications go missing, run
`./refurb-sentry -config configs/config.yaml -list-dims` and check whether the `chips`
line still looks sane.

### 5. Missing Memory Dimension and `fill_missing_memory`

**Within a single category, upstream is inconsistent about which dimensions it
provides.** Measured on the CN store: 58 of 206 mac products carry no `tsMemorySize` —
the 16-inch MacBook Pro with M5 Pro (22) and M5 Max (15), Studio Display (13), and
MacBook Neo (8). The field is absent from the product data entirely, and the title
does not carry it either.

Combined with the **strict matching** rule above, the failure is silent: a rule with
`tsMemorySize: [32gb, 36gb, 48gb, 64gb]` **quietly misses an entire tier of machines**
— no error, no warning, you simply never hear about them.

When enabled, products missing the memory dimension get one extra request to their
product detail page, and the memory read from it is written back under the same key:

```yaml
http:
  fill_missing_memory: true
```

Off by default, since it breaks the one-request-per-category design. Turn it on only
when your rules filter by memory.

- **The heuristic is language-independent.** The detail page overview lists exactly two
  entries carrying a capacity unit (memory and storage). The storage value is already
  known from the grid (`dimensionCapacity`), so it is used as an anchor to exclude that
  entry; the single remaining one is the memory. If more or fewer than one remains, the
  parse is abandoned — guessing would match a rule against a differently specced
  machine, which is worse than not reading it at all. A product whose grid data carries
  no `dimensionCapacity` at all is abandoned for the same reason: with no anchor, a page
  listing a single capacity would hand back the storage size as if it were memory.
  Verified against 14 regions (US JP DE FR UK KR IT ES NL CH TW CA AU; HK currently has
  nothing to fill in), including the French store's `24 Go` — a non-breaking space with
  French units.
- **Failure never affects the product.** On a 5xx or a page redesign the product is
  kept as-is with the memory dimension left empty; it is never mistaken for a delisting.
  It is logged at WARN, though: an unfilled product is back to being silently missed by
  your memory rules, which is exactly what this option exists to prevent.
- **Results are cached per part number, but only failures that cannot improve.** A part
  number's configuration is fixed, so one successful lookup suffices; a page that simply
  does not state the memory is remembered as such rather than retried forever. Timeouts,
  5xx and connection resets are **not** cached — they are retried next round, because
  treating one hiccup as permanent would drop that machine out of your memory rules for
  the rest of the process's life. Retrying is capped at three rounds per part number,
  though: if upstream keeps failing, retrying forever would not read the memory either
  and would only keep hammering the store.
- **Enabling it without a memory rule is reported at startup.** The fill is skipped
  entirely in that case (it could not change any notification), and the startup warning
  says so instead of leaving you to wonder why nothing changed.
- **Categories with no memory dimension are skipped entirely**, watch among them. This
  is not merely about saving requests: in testing, watch detail pages yielded a
  "memory" for all 28 products — actually the storage capacity — which would have
  polluted `tsMemorySize`.
- **Looked up on demand, not for everything.** Only products that could still match a
  rule once memory is set off are fetched. Model, chip, capacity and price all come
  from the grid; a product those already rule out will not match however much memory it
  turns out to have. The more specific your rules, the fewer lookups — and if no rule
  filters by memory, none happen at all.

The cost is a slower first round, scaling with how specific your rules are. Measured
with the MacBook Pro rule shown above: of the 58 CN mac products missing memory, only
19 were worth looking up (about 50 seconds at the default 1-3s spacing); the other 39
were ruled out from the grid alone. Steady-state rounds cost almost nothing extra.

---

## Delivery Channels

### 1. Bark (iOS)

```yaml
channels:
  - type: bark
    device_key: ${BARK_KEY}
    server: https://api.day.app   # Point to your self-hosted instance if applicable
    sound: ""                     # Empty uses Bark's default sound
    icon: https://raw.githubusercontent.com/hh-io/refurb-sentry/main/assets/icon.png
```

### 2. Telegram / Discord (Generic Webhook)

Rendered via Go templates with safe `{{json .X}}` escaping:

```yaml
channels:
  - type: webhook
    name: telegram
    url: https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage
    body: |
      {"chat_id":{{json "${TELEGRAM_CHAT_ID}"}},"text":{{json .Text}}}
```

### 3. Feishu / WeCom / DingTalk / ServerChan Guide

Ready-to-use template snippets are included in `configs/config.full.yaml`. Keep these platform nuances in mind:

| Channel | Format | Key Requirements & Pitfalls |
|---|---|---|
| **Feishu (Lark) Bot** | `{"msg_type":"text","content":{"text":…}}` | ⚠️ **Signature verification is not supported**. Use "Custom Keywords" or "IP Allowlist". |
| **WeCom Bot** | `{"msgtype":"markdown","markdown":{"content":…}}` | Native `\n` line breaks; max 4,096 bytes per message. |
| **DingTalk Bot** | `{"msgtype":"text","text":{"content":…}}` | ⚠️ **HMAC signature ("加签") is not supported**. Use "Custom Keywords" or "IP Allowlist". Prefer `text` type over markdown. |
| **ServerChan Turbo** | `title=…&desp=…` form | Uses URL-encoded forms. Set `Content-Type: application/x-www-form-urlencoded` and escape with `{{urlquery}}`. |

> [!WARNING]
> **HTTP 200 False-Success Risk**:
> Feishu, DingTalk, and similar enterprise bots **return HTTP 200 even when delivery fails** due to keyword mismatches or permission errors (error codes reside inside the response body). Because generic webhooks rely on HTTP status codes, these rejections are treated as successful, and **events will be permanently lost**.
> - Always verify delivery by sending a real test notification to your device. Do not rely solely on logs.
> - When using custom keywords, embed your keyword directly into the message body template:
>   ```yaml
>   body: |
>     {"msg_type":"text","content":{"text":{{json (printf "refurb-sentry\n%s" .Text)}}}}
>   ```

DingTalk uses `text` rather than `markdown` because it renders standard markdown,
where a single newline does not break a line — markdown would mean padding every line
with two trailing spaces. WeCom's markdown has no such quirk.

ServerChan takes a form rather than JSON, so override `Content-Type` explicitly and
escape with `text/template`'s built-in `urlquery` instead of `json`:

```yaml
  - type: webhook
    name: serverchan
    url: https://sctapi.ftqq.com/${SERVERCHAN_SENDKEY}.send
    headers:
      Content-Type: application/x-www-form-urlencoded
    body: |
      title={{urlquery .Title}}&desp={{urlquery .Text}}
```

A newer SendKey starting with `sctp` uses a different host:
`https://<uid>.push.ft07.com/send/<SendKey>.send`, where `uid` is the run of digits
between `sctp` and `t` in the SendKey.

### 4. Template Variables Reference

- **Message-level**: `.Title`, `.Body`, `.URL`, `.Group`, `.Text` (full text), `.Count`, `.Events` (every event in the round).
- **Product-level** (top-level fields reflect the first event; also available on each item in `.Events`):
  - `.Kind`: Event kind enum (`listed` / `price_drop` / `delisted`, locale-neutral), so a template can pick its own wording in any language regardless of `notify.lang`.
  - `.KindLabel`: the localised form of `.Kind` (`Listed`, `Price drop`, `Delisted`).
  - `.Region`, `.Category`, `.PartNumber`, `.ProductTitle`.
  - `.Currency`: ISO code, e.g. `USD` — not the symbol. `.Price` is formatted for display; `.PriceCents` is the integer, for arithmetic or comparisons.
  - `.OldPrice`, `.OldPriceCents`: **set only on `price_drop` events**, and they hold the price this tool last saw. Apple never exposes a list price, so this is not an official MSRP.
  - `.Rules`: names of the rules this product matched.

`.Events` carries **every** event in the round, digests included, so a template can lay
out its own list instead of reusing the built-in body:

```yaml
    body: |
      {"content":{{json .Title}},"embeds":[{{range $i, $e := .Events}}{{if $i}},{{end}}
        {"title":{{json $e.ProductTitle}},"url":{{json $e.URL}},
         "description":{{json $e.Price}}}{{end}}]}
```

---

## Configuration & Networking

### Notification Language

```yaml
notify:
  lang: en    # en | zh-CN (default)
```

Affects notification text only — event labels, the price-drop line, the digest header
and the `-dry-run` console output. Punctuation follows the language (half-width for
English, full-width for Chinese) and English counts are pluralised (`1 price drop` /
`2 price drops`).

Two things it does **not** change: **log and error messages**, which stay Chinese
because they target whoever runs the process; and **product titles**, which arrive in
the language of the store being watched. That is why `lang: en` with `regions: [CN]`
gives an English shell around Chinese titles — expected, not a bug.

An unrecognised `lang` is a fatal startup error, not a silent fallback to the default.

### Environment Variables & Secrets

- Supports `${VAR}` and `${VAR:-default}` syntax. **Unset variables without defaults cause immediate startup failure**, avoiding silent errors.
- Channels configured with `enabled: false` skip variable expansion entirely.
- Store credentials in `.env` or system environment variables (`.env`, `configs/config.yaml`, and `data/` are gitignored).
- Overridable via environment variables: `REFURB_INTERVAL`, `REFURB_REGIONS`, `REFURB_CATEGORIES`, `REFURB_PROXY`, `REFURB_STATE_PATH`, `REFURB_LOG_LEVEL`, `REFURB_DIGEST_THRESHOLD`.

### Request Frequency & CDN Caching

The Apple refurbished listing page is a public static asset with `cache-control: public, max-age=120, s-maxage=120`. All requests hit global edge CDNs (~50ms latency).

- **The default 120s interval is a physical CDN floor**: Polling faster only fetches identical cached responses; the daemon warns if an interval lower than 120s is specified.
- No anti-bot measures: A stable, real-browser User-Agent is used. UA rotation and proxy pools are unnecessary.
- Serial requests with 1–3s randomized jitter; 429/503 responses trigger exponential backoff adhering to `Retry-After`.

### Proxy & Currency Guard

Configure proxies via `http.proxy` (supports `http://`, `https://`, `socks5://`). Proxies serve to **fix your egress region** (Apple geo-routes by IP).

> [!CAUTION]
> The built-in currency guard halts execution if fetched product currency does not match the target region. However, **BE, DE, ES, FR, IE, IT, and NL all use EUR**. If your proxy exits in the wrong Eurozone country, the currency check cannot detect the mismatch. When monitoring multiple European stores, verify your proxy egress IP.

---

## Production Deployment

### Docker Compose

Images are published at `ghcr.io/hh-io/refurb-sentry` for `linux/amd64` and `linux/arm64` (~17MB, Alpine-based).

```bash
# 1. Setup config and secret
cp configs/config.example.yaml configs/config.yaml
echo 'BARK_KEY=your_bark_key' > deploy/.env

# 2. Start container
docker compose -f deploy/docker-compose.yml up -d

# 3. Dry-run verification
docker compose -f deploy/docker-compose.yml run --rm refurb-sentry -once -dry-run
```

> [!TIP]
> Two things to watch out for:
> - **Relative paths in the compose file resolve against `deploy/`**, not the directory you run the command from. The mounted `../configs/config.yaml` is the one in the repo.
> - **The container runs under non-root `uid 1000`.** State lives in a Docker named volume by default, which Docker initialises with the ownership baked into the image, so no chown is needed. Switch to a host bind mount and you must `chown 1000:1000` the host directory yourself, or `state.json` can't be written and every round rolls its baseline back.

### launchd / systemd (Persistent Daemons)

Pre-configured service definitions are provided in `deploy/`:
- **macOS**: Copy `deploy/com.refurb-sentry.plist` to `~/Library/LaunchAgents/` and load via `launchctl load ...`
- **Linux**: Copy `deploy/refurb-sentry.service` to `/etc/systemd/system/` and run `systemctl enable --now refurb-sentry`

Services automatically restart with a 30s backoff on crash and flush state cleanly on `SIGTERM`.

### Disk usage over long runs

The program writes no log files of its own — everything goes to stdout for the
supervisor to collect. In steady state that is at least one line per polling round
(every 2 minutes by default), roughly 25MB a year; network hiccups add WARN lines, and
`log_level: debug` multiplies it. **Rotation differs by supervisor**, so it is worth
checking before leaving this running for months:

| Deployment | Where logs go | Rotation |
|---|---|---|
| Docker Compose | Docker json-file driver | The repo's compose caps it at 3 × 10MB (30MB total) |
| systemd | journald | Handled system-wide by `journald.conf` (`SystemMaxUse` etc.); nothing to configure |
| launchd (macOS) | `/usr/local/var/log/refurb-sentry.log` | **launchd does not rotate** — hand it to `newsyslog`; see the comment inside the plist |

> [!IMPORTANT]
> Docker's default json-file driver is **unbounded** and will grow until the disk fills.
> `deploy/docker-compose.yml` already sets a `logging` limit; if you wrote your own
> compose file or use `docker run` directly, add
> `--log-opt max-size=10m --log-opt max-file=3` yourself.

The state file does not grow without bound: it records only products **currently in
stock**, dropping entries as they are delisted, so its size tracks the catalogue rather
than accumulating over time (about 200KB measured across all CN categories).

<details>
<summary>Why not GitHub Actions or Cloudflare Workers?</summary>

- **GitHub Actions**: Minimum 5-minute cron granularity, frequent queue delays under load, automatic deactivation after 60 days of inactivity, and tedious git commits required for state persistence.
- **Cloudflare Workers**: Free tier CPU limits (10ms) cannot parse large Mac catalog pages (>1.3MB HTML, 200+ products); dynamic edge routing causes region detection to drift unpredictably.
</details>

---

## Reliability & State

State defaults to `data/state.json`, storing part numbers, titles, current prices, dimensions, and first/last seen timestamps. Updates use atomic writes (temporary file + atomic rename) to guard against corruption during power cuts or crashes. To rebuild the baseline from scratch, delete the file — the next start re-seeds silently.

Three reliability principles prevent missed alerts or alert storms:

1. **Independent per-scope baselines**: Baseline state is tracked per `[region/category]`. Adding a new scope to a running instance baselines silently without triggering alerts for pre-existing stock. Network failures during initial fetch do not mark scopes as baselined.
2. **Fetch failures never cause delistings**: Any error skips that category without touching state. An empty result must repeat for several consecutive rounds before it is believed.
3. **Round-wide rollback when an event reaches nothing**: If any event in a round reaches **no channel at all**, the whole round is rolled back. The in-memory baseline is snapshotted before the diff and restored on failure, so a long-running process genuinely retries rather than merely skipping the disk write, and those events are regenerated next round. Because the rollback is round-wide, events that *were* delivered get sent once more on the retry — a duplicate notification beats a lost one. An event that reached at least one channel counts as delivered; the channels that failed lose that batch, logged at ERROR. Retrying is capped at 5 consecutive rollbacks: some failures never recover (a body past Telegram's length limit, a webhook that always returns 400), and retrying those forever would re-push the same batch every interval while the baseline never advances. Past the cap the round is forced through and the lost events are logged at ERROR.

---

## Development

```bash
# Run unit tests, vet, and format checks
go test ./... && go vet ./... && gofmt -l .
```

- **Adding a region**: Add a row to the table in `internal/apple/regions.go`; the startup check verifies it is actually reachable. A new currency also needs an entry in the symbol table in `internal/apple/model.go`, or prices degrade to `XXX 999`.
- Automated CI validates tests and formatting on every push. Pushing a `v*` tag triggers GoReleaser to publish cross-platform binaries and container images.

---

## Disclaimer

This project is not affiliated with, sponsored by, or endorsed by Apple Inc., and is not an official Apple product. Apple, MacBook, iPad, and Apple Watch are trademarks of Apple Inc.

This tool only reads publicly accessible Apple Certified Refurbished store pages to assist personal purchasing decisions. Product information (titles, prices, images) is copyright Apple Inc. Use at your own risk and comply with local laws and Apple's terms of service.

---

## License

[MIT](LICENSE)
