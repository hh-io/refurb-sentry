# refurb-sentry

监控 Apple 官网**官方翻新产品**的上架、降价与下架,按规格过滤后实时推送到 Bark / Telegram 等渠道。

单个静态二进制,常驻运行,状态落地为一个 JSON 文件,重启不丢。

## 它做什么

- **三类事件**:新商品上架、价格下降、商品下架
- **多地区**:AU / BE / CA / CH / CN / DE / ES / FR / HK / IE / IT / JP / KR / NL / NZ / SG / TW / UK / US(共 19 个)
- **规格过滤**:机型、内存、存储、尺寸、年份、颜色、芯片、CPU/GPU 核心数、价格区间
- **推送渠道**:Bark、通用 webhook(可对接 Telegram、飞书、Server 酱、Discord)

## 它不做什么

- **没有"剩余库存台数"**。数据源只表达"在列表里 = 有货",拿不到具体数量。
- **不做价格历史曲线**。状态库只保留当前价格用于比对降价。
- **不做反爬对抗**。见下面「关于请求频率」——这个数据源不需要。

## 快速开始

```bash
git clone https://github.com/hh/refurb-sentry && cd refurb-sentry
go build -o refurb-sentry ./cmd/refurb-sentry

cp configs/config.example.yaml configs/config.yaml
export BARK_KEY=你的_bark_device_key

# 先看看你关心的分类当前有哪些可过滤的维度和取值
./refurb-sentry -config configs/config.yaml -list-dims

# 试运行一轮,通知打印到终端而不真的推送
./refurb-sentry -config configs/config.yaml -once -dry-run

# 正式常驻
./refurb-sentry -config configs/config.yaml
```

**首次运行只建立基线,不会推送任何通知**——否则数百件在架商品会一次性涌进你的手机。
从第二轮起才开始报变化。

### 命令行参数

| 参数 | 说明 |
|---|---|
| `-config` | 配置文件路径,默认 `configs/config.yaml` |
| `-once` | 只跑一轮就退出 |
| `-dry-run` | 不真的推送,把通知内容打印到标准输出 |
| `-list-dims` | 列出各地区/分类当前可用的过滤维度与取值 |
| `-version` | 打印版本 |

## 写过滤规则

规则的可用维度**随分类而变**(mac 有内存/容量,watch 有表壳尺寸/材质),所以先查:

```console
$ ./refurb-sentry -config configs/config.yaml -list-dims

===== CN/mac(215 件)=====
  refurbClearModel         (机型)     [display imac macbookair macbookneo macbookpro macmini macstudio]
  dimensionScreensize      (尺寸)     [13inch 14inch 15inch 16inch 24inch 27inch]
  dimensionRelYear         (发布年份)  [2022 2024 2025 2026]
  tsMemorySize             (内存)     [128gb 16gb 24gb 32gb 36gb 48gb 64gb 8gb]
  dimensionCapacity        (容量)     [1tb 256gb 2tb 4tb 512gb 8tb]
  chips                    (芯片)     [A18 Pro M2 M4 M4 Max M4 Pro M5 M5 Max M5 Pro]
```

然后照着写:

```yaml
rules:
  - name: MacBook Pro 高配
    regions: [CN]
    categories: [mac]
    dimensions:
      refurbClearModel: [macbookpro]
      tsMemorySize: [24gb, 36gb, 48gb]
      dimensionCapacity: [1tb, 2tb]
    chips: [M4 Pro, M4 Max, M5 Pro, M5 Max]
    min_cpu_cores: 12
    max_price: 20000
```

**匹配语义**:规则之间 OR(命中任一条就推送);单条规则内各字段之间 AND;
字段内多个候选值之间 OR;留空的字段不作限制。

规则只在**推送前**过滤。状态库始终记录全部商品,所以你以后放宽规则时,
早就在架上的商品不会被误报成"新上架"。

### 芯片与核心数是唯一的脆弱项

机型、内存、容量等来自页面的结构化字段,稳定可靠。
但**芯片型号和 CPU/GPU 核心数只存在于商品标题字符串里**,而各语言站点的语序完全不同:

```
US  Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU
NL  Refurbished 13-inch MacBook Air Apple M4-chip met 10-core CPU en 10-core GPU
FR  Mac mini reconditionné avec puce Apple M4, CPU 10 cœurs, GPU 10 cœurs
ES  iMac reacondicionado de 24 pulgadas con chip M4 de Apple, CPU de 8 núcleos
CN  翻新 Mac mini Apple M4 芯片 (配备 10 核中央处理器和 10 核图形处理器)
HK  翻新產品 14 吋 MacBook Pro Apple M5 晶片 (配備 10 核心 CPU 及 10 核心 GPU)
JP  14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ
KR  리퍼비쉬 MacBook Pro 14 Apple M5 Pro 칩 모델(15코어 CPU 및 16코어 GPU)
```

解析器不按地区分派正则,而是把芯片「指示词」与「型号」解耦、并同时认两种语序,
所以新增地区通常不需要改解析代码。当前在 19 个地区 2000+ 件真实商品上的识别率:
**芯片 100%,核心数 95%**(剩余 5% 是标题本身就没写核心数的机型)。

Apple 还会在同一个页面里混用普通空格与不间断空格(U+00A0)、以及多种 Unicode 连字符,
标题在归一化后才做匹配。你写的 `title_match` 正则也作用于归一化后的文本,
所以直接写 ASCII 连字符即可。

即便如此,Apple 调整文案时这里仍可能失配。解析失败时商品会被保留(芯片记为未知),
但用了 `chips` / `min_cpu_cores` / `min_gpu_cores` 的规则会漏掉它。
如果发现漏推,先用 `-list-dims` 看看 `chips` 一行是否还正常。

## 推送渠道

### Bark

```yaml
channels:
  - type: bark
    device_key: ${BARK_KEY}
    server: https://api.day.app   # 自建服务端改这里
```

### Telegram / 飞书 / 其它

通用 webhook 用 Go 模板拼请求体,`{{json .X}}` 会安全转义:

```yaml
channels:
  - type: webhook
    name: telegram
    url: https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage
    body: |
      {"chat_id":{{json "${TELEGRAM_CHAT_ID}"}},"text":{{json .Text}}}
```

模板可用字段:`.Title` `.Body` `.URL` `.Text` `.Kind` `.KindLabel` `.Count`
`.Region` `.Category` `.PartNumber` `.ProductTitle` `.Price` `.OldPrice`
`.PriceCents` `.OldPriceCents` `.Currency` `.Rules`

一轮内命中事件超过 `notify.digest_threshold`(默认 5)时会自动合并成一条摘要,
避免 Apple 批量上架时刷屏。

## 配置与密钥

配置里的 `${VAR}` 从环境变量展开,`${VAR:-默认值}` 可给回退值。
**引用了未设置且无默认值的变量会直接报错退出**,而不是静默变成空串让推送悄悄失效。

密钥请走环境变量或 `.env`,不要写进配置文件。`data/`、`configs/config.yaml`、`.env`
已在 `.gitignore` 中。

少量常改项也可以直接用环境变量覆盖:
`REFURB_INTERVAL` `REFURB_REGIONS` `REFURB_CATEGORIES` `REFURB_PROXY`
`REFURB_STATE_PATH` `REFURB_LOG_LEVEL` `REFURB_DIGEST_THRESHOLD`

## 关于请求频率

翻新列表页是**公开静态页**,响应头为 `cache-control: public, max-age=120, s-maxage=120`,
请求命中 CDN 边缘缓存(实测约 50ms 返回),不会打到 Apple 源站。
`robots.txt` 也未禁止 `/shop/refurbished`。

因此:

- **默认间隔 120 秒,把它调更短没有意义**——CDN 缓存 120 秒,你只会拿到同一份副本,
  徒增请求量。配置低于 120 秒时程序会给出警告。
- 本项目**不做** TLS 指纹伪装、UA 轮换、代理池。UA 固定为一个真实浏览器标识:
  稳定的 UA 比随机变化的更不像自动化流量。
- 同一轮内的请求串行发出,相邻请求间随机停顿 1–3 秒。
- 遇 429/503 按指数退避并遵从 `Retry-After`。

代理(`http.proxy`,支持 http/https/socks5)的用途是**修正出口地区**——
Apple 按 IP 判定地区——而不是隐藏身份。若抓到的货币与目标地区不符,
程序会报错而不是把别国数据混进你的状态库。

## 部署

`deploy/` 下有现成的单元文件:

- **mac mini**:`com.refurb-sentry.plist` → `~/Library/LaunchAgents/`,`launchctl load`
- **Linux VPS**:`refurb-sentry.service` → `/etc/systemd/system/`,`systemctl enable --now`

两者都配置了自动重启和 30 秒退避。程序收到 SIGTERM 会先落盘状态再退出。

### 为什么不推荐 GitHub Actions / Cloudflare Workers

- **GitHub Actions**:cron 最短 5 分钟,官方明确说明高负载时可能延迟甚至丢弃任务;
  公共仓库在 60 天无活动后会自动禁用定时工作流;状态还得 commit 回仓库。
- **Cloudflare Workers**:免费套餐 Cron Trigger 的 CPU 时间上限是 10ms,
  而解析一个 mac 分类页(1.3MB HTML、239 件商品)必然超时,需要付费套餐;
  且出口 IP 遍布全球数据中心,地区判定不可控。

## 状态文件

默认 `data/state.json`,记录每件商品的货号、标题、当前价格、维度和首见/末见时间。
采用"写临时文件 + rename"的原子替换,断电或被 kill 都不会留下半截文件。

想重建基线(比如换了监控范围):删掉它,下次启动会重新静默建基线。

## 开发

```bash
go test ./...
go vet ./...
gofmt -l .
```

新增地区只需在 `internal/apple/regions.go` 的表里加一行,启动校验会验证其可用性。

## 免责声明

本项目与 Apple Inc. 无任何隶属、赞助或背书关系,也非 Apple 官方产品。
Apple、MacBook、iPad、Apple Watch 等为 Apple Inc. 的商标。

本工具仅抓取 Apple 官网**公开的**翻新产品列表页,用于个人购买决策的辅助监控;
所抓取的商品信息(标题、价格、图片链接等)版权归 Apple Inc. 所有。
请遵守你所在地区的法律法规与 Apple 网站的使用条款,自行承担使用风险。

本项目**不提供也不接受**任何自动下单、抢购、批量购买相关的功能请求。

## License

MIT
