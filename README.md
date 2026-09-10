# refurb-sentry

**English** · [简体中文](README.zh-CN.md)

Watches Apple's **Certified Refurbished** store for new listings, price drops and
removals, filters them by spec, and pushes what you care about to Bark / Telegram
and friends.

Single static binary, runs as a daemon, keeps its state in one JSON file so a
restart loses nothing.

> [!NOTE]
> **Notification text and log messages are in Chinese.** Event labels (上架 = listed,
> 降价 = price drop, 下架 = delisted) are hardcoded. Everything you configure —
> config keys, region and category codes, dimension values, rule fields — is in
> English/ASCII, and product titles come through in whatever language the target
> Apple store uses. Localisation of the output is not implemented yet.

## What a notification looks like

```
降价 · US mac                                    ← "price drop · US mac"
Refurbished 14-inch MacBook Pro Apple M4 Pro chip with 12-Core CPU and 16-Core GPU
$1,999 → $1,799(降 $200,10.0%)                  ← "down $200, 10.0%"
命中规则:MacBook Pro high-end                    ← "matched rule: ..."
https://www.apple.com/shop/product/...
```

When a single round produces more than `notify.digest_threshold` events
(default 5), they are merged into one digest so a bulk restock doesn't flood
your phone:

```
翻新监控 · 上架 6 / 降价 2                        ← "refurb watch · 6 listed / 2 price drops"
[上架] US Refurbished Mac mini Apple M4 chip $499
[降价] US Refurbished 14-inch MacBook Pro $1,999 → $1,799
……
```

## What it does

- **Three event kinds**: new listing, price drop, delisting
- **Spec filtering**: model, memory, storage, screen size, release year, colour,
  chip, CPU/GPU core count, price range
- **Delivery**: Bark, or a generic webhook (works with Telegram, Discord,
  Feishu/Lark, Server 酱, anything that takes a JSON POST)

## What it does not do

- **No "units left in stock."** The data source only says "present in the list =
  available"; there is no quantity anywhere.
- **No price history charts.** State keeps the current price only, to detect drops.
- **No anti-bot evasion.** See [Request frequency](#request-frequency) — this
  source doesn't need any.
- **No auto-checkout, sniping or bulk buying.** Feature requests for those are
  not accepted.

## Coverage

**19 regions**: AU BE CA CH CN DE ES FR HK IE IT JP KR NL NZ SG TW UK US

**8 categories**: `mac` `ipad` `iphone` `watch` `airpods` `appletv` `homepod` `accessories`

**The region × category matrix is sparse** — not every combination exists:

- CN and HK have no `iphone` or `appletv`; those URLs return 404.
- US returns 200 for `appletv`, `airpods` and `homepod`, but the page carries no
  product data.

On the first run every configured combination is validated up front, and an
unavailable one fails loudly instead of leaving a partial baseline behind.
MX and IN have no refurbished store at all (404) — don't add them.

## Quick start

Requires Go 1.26 or newer.

```bash
git clone https://github.com/hh-io/refurb-sentry && cd refurb-sentry
go build -o refurb-sentry ./cmd/refurb-sentry

cp configs/config.example.yaml configs/config.yaml
export BARK_KEY=your_bark_device_key

# See which dimensions and values the categories you care about expose right now
./refurb-sentry -config configs/config.yaml -list-dims

# One round, printing notifications to the terminal instead of sending them
./refurb-sentry -config configs/config.yaml -once -dry-run

# Run for real
./refurb-sentry -config configs/config.yaml
```

**The first run only records a baseline and sends nothing** — otherwise several
hundred already-listed products would land on your phone at once. Changes are
reported from the second round on.

### Flags

| Flag | Meaning |
|---|---|
| `-config` | Config file path, default `configs/config.yaml` |
| `-once` | Run a single round and exit |
| `-dry-run` | Send nothing, write no state, print notifications to stdout |
| `-list-dims` | Print the filter dimensions and values currently available per region/category |
| `-version` | Print version |

## Writing filter rules

Which dimensions exist **depends on the category** (mac has memory/capacity,
watch has case size/material), so look them up first:

```console
$ ./refurb-sentry -config configs/config.yaml -list-dims

===== US/mac(209 件)=====
  refurbClearModel       (Models)   display, imac, macbookair, macbookneo, macbookpro
  dimensionScreensize    (Sizes)    13inch, 14inch, 15inch, 16inch, 24inch, 27inch
  dimensionRelYear       (Release Year) 2022, 2023, 2024, 2025, 2026
  dimensionColor         (Finish)   blue, midnight, silver, spaceblack, starlight, ...
  tsMemorySize           (Memory)   128gb, 16gb, 24gb, 32gb, 36gb, 48gb, 8gb
  dimensionCapacity      (Capacity) 1tb, 256gb, 2tb, 4tb, 512gb, 8tb
  chips                  (芯片)       A18 Pro, M3, M4, M4 Max, M4 Pro, M5, M5 Max, M5 Pro
```

The labels in parentheses come from the Apple store page itself, so they arrive
in that region's language; `chips` is computed by this tool and labelled in Chinese.
The keys and values on either side are stable identifiers — those are what you
put in a rule:

```yaml
rules:
  - name: MacBook Pro high-end
    regions: [US]
    categories: [mac]
    dimensions:
      refurbClearModel: [macbookpro]
      tsMemorySize: [24gb, 36gb, 48gb]
      dimensionCapacity: [1tb, 2tb]
    chips: [M4 Pro, M4 Max, M5 Pro, M5 Max]
    min_cpu_cores: 12
    max_price: 2500
```

### Rule fields

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Shown in the notification's "matched rule" line; defaults to `rule#N` |
| `regions` | list | Restrict to these regions; empty means no restriction |
| `categories` | list | Restrict to these categories; empty means no restriction |
| `dimensions` | map | Structured dimensions from the page; keys vary by category, use `-list-dims` |
| `chips` | list | Chip model, e.g. `M4 Pro`. Parsed from the title — see below |
| `min_cpu_cores` | int | Minimum CPU cores. Parsed from the title |
| `min_gpu_cores` | int | Minimum GPU cores. Parsed from the title |
| `title_match` | regex | Matched against the **normalised** title; plain ASCII hyphens are fine |
| `min_price` | number | Lower bound, inclusive. `0` means no bound |
| `max_price` | number | Upper bound, inclusive. `0` means no bound |

**Matching semantics**: rules are OR'd (any match sends); fields within one rule
are AND'ed; values within one field are OR'd; an omitted field constrains nothing;
values are case-insensitive.

Two deliberately strict choices: a product **missing** a dimension the rule asks
for does not match, and a core count that **could not be parsed** satisfies no
`min_*_cores` bound. Better to miss a notification than to treat unknown as
qualifying.

Rules filter **only at send time**. The state file always tracks every product,
so when you later loosen a rule, products that were already listed won't be
reported as new arrivals.

### Chip and core count are the one fragile part

Model, memory and capacity come from structured fields on the page and are
reliable. But **chip model and CPU/GPU core counts exist only inside the product
title**, and the word order differs completely between localised stores:

```
US  Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU
FR  Mac mini reconditionné avec puce Apple M4, CPU 10 cœurs, GPU 10 cœurs
JP  14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ
```

The parser does not dispatch a regex per region. It decouples the chip *keyword*
from the *model* and accepts both word orders, so adding a region usually needs no
parser change. Measured across 2000+ real products in all 19 regions: **chip 100%,
core count 95%** (the remaining 5% are titles that simply don't state core counts).

Apple also mixes regular spaces, non-breaking spaces (U+00A0) and several Unicode
hyphens within a single page, so titles are normalised before matching —
`title_match` runs against the normalised text too.

Even so, a copy change on Apple's side can break this. When parsing fails the
product is kept (chip recorded as unknown), but rules using `chips`,
`min_cpu_cores` or `min_gpu_cores` will skip it. If notifications go missing,
check whether the `chips` line in `-list-dims` still looks sane.

## Delivery channels

### Bark

```yaml
channels:
  - type: bark
    device_key: ${BARK_KEY}
    server: https://api.day.app   # point at your own server here
```

### Telegram / Discord / anything else

The generic webhook builds its body from a Go template; `{{json .X}}` escapes
values safely:

```yaml
channels:
  - type: webhook
    name: telegram
    url: https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage
    body: |
      {"chat_id":{{json "${TELEGRAM_CHAT_ID}"}},"text":{{json .Text}}}
```

Available template fields: `.Title` `.Body` `.URL` `.Group` `.Text` `.Kind`
`.KindLabel` `.Count` `.Region` `.Category` `.PartNumber` `.ProductTitle`
`.Currency` `.Price` `.PriceCents` `.OldPrice` `.OldPriceCents` `.Rules`.
On a digest message, the per-product fields come from the first event.

## Config and secrets

`${VAR}` in the config expands from the environment; `${VAR:-fallback}` supplies a
default. **Referencing an unset variable with no default is a fatal error** rather
than a silent empty string that makes delivery quietly fail. Channels with
`enabled: false` skip expansion entirely, so you don't need to set variables for
channels you don't use.

Keep secrets in the environment or a `.env` file, not in the config. `data/`,
`configs/config.yaml` and `.env` are already in `.gitignore`.

A few frequently-changed settings can be overridden directly from the environment:
`REFURB_INTERVAL` `REFURB_REGIONS` `REFURB_CATEGORIES` `REFURB_PROXY`
`REFURB_STATE_PATH` `REFURB_LOG_LEVEL` `REFURB_DIGEST_THRESHOLD`

## Request frequency

The refurbished listing page is a **public static page** served with
`cache-control: public, max-age=120, s-maxage=120`. Requests hit the CDN edge
(~50ms measured) and never reach Apple's origin. `robots.txt` does not disallow
`/shop/refurbished`.

Therefore:

- **The default 120s interval is a floor, not a knob.** The CDN caches for 120
  seconds; polling faster just fetches the same copy again. The program warns if
  you configure less.
- **No** TLS fingerprint spoofing, UA rotation or proxy pools. The UA is a fixed,
  real browser string — a stable UA looks *less* like automation than a rotating one.
- Requests within a round are issued serially, with a random 1–3s pause between them.
- 429/503 triggers exponential backoff and honours `Retry-After`.

A proxy (`http.proxy`, http/https/socks5) exists to **fix your exit region** —
Apple decides the store by IP — not to hide anything. If fetched products report a
currency that doesn't match the target region, the program errors out instead of
mixing another country's data into the state file.

> [!WARNING]
> That guard only works when the currencies actually differ. **BE, DE, ES, FR, IE,
> IT and NL all use EUR**, so a proxy exiting in the wrong Eurozone country passes
> the check while serving another country's catalogue. When watching several
> Eurozone regions at once, verify the exit IP yourself.

## Deployment

Ready-made unit files live in `deploy/`:

- **mac mini**: `com.refurb-sentry.plist` → `~/Library/LaunchAgents/`, `launchctl load`
- **Linux VPS**: `refurb-sentry.service` → `/etc/systemd/system/`, `systemctl enable --now`

Both restart automatically with a 30s backoff. On SIGTERM the program flushes
state before exiting.

<details>
<summary>Why not GitHub Actions or Cloudflare Workers</summary>

- **GitHub Actions**: cron granularity bottoms out at 5 minutes, and GitHub
  explicitly warns that runs may be delayed or dropped under load; scheduled
  workflows in public repos are disabled after 60 days of inactivity; and you'd
  have to commit state back into the repo.
- **Cloudflare Workers**: the free tier caps Cron Trigger CPU time at 10ms, and
  parsing one mac category page (1.3MB of HTML, 200+ products) blows past that —
  it needs a paid plan. Exit IPs are also spread across global datacenters, which
  makes region detection unpredictable.

</details>

## State and reliability

State lives in `data/state.json` by default and records each product's part number,
title, current price, dimensions and first/last seen timestamps. It's replaced
atomically (temp file + rename), so a power cut or a `kill` never leaves a
truncated file. To rebuild the baseline, delete it — the next start re-seeds
silently.

Three deliberate design choices, all aimed at never losing or spamming notifications:

- **The baseline is tracked per region/category.** Add a region or category to a
  running instance and the new scope seeds its own baseline silently instead of
  reporting hundreds of existing products as new. Likewise, a region that failed to
  fetch on the first round is not wrongly marked as baselined.
- **A fetch failure never causes a delisting.** Any error skips that category
  without touching state, and an empty result must repeat for several rounds before
  it's believed.
- **If every channel fails in a round, the baseline does not advance.** Those
  changes are regenerated and retried next round rather than being swallowed. If
  only some channels fail, delivery counts as successful and the baseline advances —
  the failed channels lose that batch, and it's logged at ERROR.

## Development

```bash
go test ./... && go vet ./... && gofmt -l .
```

Adding a region is one row in the table in `internal/apple/regions.go`; the startup
check verifies it's actually reachable. A new currency also needs an entry in the
symbol table in `internal/apple/model.go`, or prices degrade to `XXX 999`.

## Disclaimer

This project is not affiliated with, sponsored by, or endorsed by Apple Inc., and
is not an Apple product. Apple, MacBook, iPad and Apple Watch are trademarks of
Apple Inc.

It reads only the **publicly accessible** refurbished listing pages, as an aid to
personal purchasing decisions. The product information it retrieves (titles,
prices, image links) is copyright Apple Inc. Comply with the laws of your
jurisdiction and Apple's terms of use; you use this at your own risk.

## License

MIT
