# refurb-sentry

[English](README.md) · **简体中文**

[![CI](https://github.com/hh-io/refurb-sentry/actions/workflows/ci.yml/badge.svg)](https://github.com/hh-io/refurb-sentry/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/hh-io/refurb-sentry)](https://github.com/hh-io/refurb-sentry/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/hh-io/refurb-sentry)](https://goreportcard.com/report/github.com/hh-io/refurb-sentry)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

监控 Apple 官网**官方翻新产品**的上架、降价与下架,按规格过滤后推送到 Bark / Telegram 等渠道。

单个静态二进制,常驻运行,状态落地为一个 JSON 文件,重启不丢。

<p align="center">
  <img src="assets/notification.zh-CN.png" width="420"
       alt="iPhone 锁屏上的两条 Bark 通知:一条降价、一条上架,各自标出命中的规则">
</p>

## 推送长什么样

```
降价 · CN mac
翻新 14 英寸 MacBook Pro Apple M4 Pro 芯片 (配备 12 核中央处理器和 16 核图形处理器)
RMB 14,999 → RMB 13,499(降 RMB 1,500,10.0%)
命中规则:MacBook Pro 高配
https://www.apple.com.cn/shop/product/...
```

一轮内命中事件超过 `notify.digest_threshold`(默认 5)条时,自动合并成一条摘要,
避免 Apple 批量上架时刷屏:

```
翻新监控 · 上架 6 / 降价 2
[上架] CN 翻新 Mac mini Apple M4 芯片 RMB 4,699
[降价] CN 翻新 14 英寸 MacBook Pro RMB 14,999 → RMB 13,499
……
```

## 它做什么

- **三类事件**:新商品上架、价格下降、商品下架
- **规格过滤**:机型、内存、存储、尺寸、年份、颜色、芯片、CPU/GPU 核心数、价格区间
- **推送渠道**:Bark、通用 webhook(可对接 Telegram、飞书、Server 酱、Discord)

## 它不做什么

- **没有"剩余库存台数"**。数据源只表达"在列表里 = 有货",拿不到具体数量。
- **不做价格历史曲线**。状态库只保留当前价格用于比对降价。
- **不做反爬对抗**。见「关于请求频率」——这个数据源不需要。
- **不提供自动下单、抢购、批量购买**,也不接受这类功能请求。

## 监控范围

**19 个地区**:AU BE CA CH CN DE ES FR HK IE IT JP KR NL NZ SG TW UK US

**8 个分类**:`mac` `ipad` `iphone` `watch` `airpods` `appletv` `homepod` `accessories`

**地区 × 分类的矩阵是稀疏的**,并非任意组合都存在:

- CN、HK 没有 `iphone` 与 `appletv`,请求直接返回 404
- US 的 `appletv`、`airpods`、`homepod` 返回 200,但页面里没有商品数据

首轮启动会逐一校验配置里的组合,不可用的会明确报错并退出,而不是留下一个残缺的基线。
实测 MX、IN 没有翻新店,不要往地区表里加。

## 快速开始

### 安装

从 [最新 release](https://github.com/hh-io/refurb-sentry/releases/latest)
下载对应平台的归档——支持 macOS 与 Linux 的 amd64 / arm64 / armv7,
里面包含二进制、示例配置和 systemd / launchd 单元文件:

```bash
tar xzf refurb-sentry_*_darwin_arm64.tar.gz
```

有 Go 1.26+ 工具链也可以直接从源码安装:

```bash
go install github.com/hh-io/refurb-sentry/cmd/refurb-sentry@latest
```

或者克隆后自己构建,这样同时能拿到示例配置和单元文件:

```bash
git clone https://github.com/hh-io/refurb-sentry && cd refurb-sentry
go build -o refurb-sentry ./cmd/refurb-sentry
```

### 运行

```bash
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
| `-dry-run` | 不推送也不写状态文件,把通知内容打印到标准输出 |
| `-list-dims` | 列出各地区/分类当前可用的过滤维度与取值 |
| `-version` | 打印版本 |

## 写过滤规则

规则的可用维度**随分类而变**(mac 有内存/容量,watch 有表壳尺寸/材质),所以先查:

```console
$ ./refurb-sentry -config configs/config.yaml -list-dims

===== CN/mac(208 件)=====
  refurbClearModel       (机型)       display, imac, macbookair, macbookneo, macbookpro, macmini, macstudio
  dimensionScreensize    (尺寸)       13inch, 14inch, 15inch, 16inch, 24inch, 27inch
  dimensionRelYear       (发布年份)     2022, 2024, 2025, 2026
  dimensionColor         (外观)       blue, midnight, silver, space_gray, spaceblack, starlight, ...
  tsMemorySize           (内存)       128gb, 16gb, 24gb, 32gb, 36gb, 48gb, 64gb, 8gb
  dimensionCapacity      (容量)       1tb, 256gb, 2tb, 4tb, 512gb, 8tb
  chips                  (芯片)       A18 Pro, M2, M4, M4 Max, M4 Pro, M5, M5 Max, M5 Pro
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

### 规则字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 规则名,会出现在通知的「命中规则」里;留空则自动生成 `rule#N` |
| `regions` | 列表 | 限定地区,留空表示不限 |
| `categories` | 列表 | 限定分类,留空表示不限 |
| `dimensions` | 映射 | 页面给出的结构化维度,键随分类而变,用 `-list-dims` 查 |
| `chips` | 列表 | 芯片型号,如 `M4 Pro`。从标题解析,见下节 |
| `min_cpu_cores` | 整数 | CPU 核心数下限。从标题解析 |
| `min_gpu_cores` | 整数 | GPU 核心数下限。从标题解析 |
| `title_match` | 正则 | 对**归一化后**的标题做匹配,写 ASCII 连字符即可 |
| `min_price` | 数字 | 价格下限(含),`0` 表示不限 |
| `max_price` | 数字 | 价格上限(含),`0` 表示不限 |

**匹配语义**:规则之间 OR(命中任一条就推送);单条规则内各字段之间 AND;
字段内多个候选值之间 OR;留空的字段不作限制;取值大小写不敏感。

两个刻意的"从严"取舍:商品**缺少**规则指定的某个维度时视为不匹配;
核心数**未能解析出来**时不满足任何 `min_*_cores` 下限。宁可漏推,不把未知当作符合条件。

规则只在**推送前**过滤。状态库始终记录全部商品,所以你以后放宽规则时,
早就在架上的商品不会被误报成"新上架"。

### 芯片与核心数是唯一的脆弱项

机型、内存、容量等来自页面的结构化字段,稳定可靠。
但**芯片型号和 CPU/GPU 核心数只存在于商品标题字符串里**,而各语言站点的语序完全不同:

```
US  Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU
FR  Mac mini reconditionné avec puce Apple M4, CPU 10 cœurs, GPU 10 cœurs
JP  14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ
```

解析器不按地区分派正则,而是把芯片「指示词」与「型号」解耦、并同时认两种语序,
所以新增地区通常不需要改解析代码。当前在 19 个地区 2000+ 件真实商品上的识别率:
**芯片 100%,核心数 95%**(剩余 5% 是标题本身就没写核心数的机型)。

Apple 还会在同一页面里混用普通空格、不间断空格(U+00A0)和多种 Unicode 连字符,
标题归一化后才做匹配,`title_match` 也作用于归一化后的文本。

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
    sound: ""                     # 留空用 Bark 默认铃声
    icon: https://raw.githubusercontent.com/hh-io/refurb-sentry/main/assets/icon.png
```

`icon` 是通知左侧显示的图标,任何公网可访问的位图都行——iOS 不认 SVG。
留空则保持 Bark 自带的图标。

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

### 模板字段

消息级:`.Title` `.Body` `.URL` `.Group` `.Text` `.Count` `.Events`

商品级(顶层取首条事件,`.Events` 的每一项也有同样的字段):
`.Kind` `.KindLabel` `.Region` `.Category` `.PartNumber` `.ProductTitle`
`.Currency` `.Price` `.PriceCents` `.OldPrice` `.OldPriceCents` `.Rules`

`.Events` 携带本轮**全部**事件,摘要消息里也是完整的,
所以模板可以自己排版列表,不必迁就内置的正文格式:

```yaml
    body: |
      {"content":{{json .Title}},"embeds":[{{range $i, $e := .Events}}{{if $i}},{{end}}
        {"title":{{json $e.ProductTitle}},"url":{{json $e.URL}},
         "description":{{json $e.Price}}}{{end}}]}
```

`.Kind` 是语言中立的 `listed` / `price_drop` / `delisted`,
webhook 模板可据它自行分派任意语言的文案,不受 `notify.lang` 约束;
`.KindLabel` 则是已本地化的形式。

## 通知语言

```yaml
notify:
  lang: zh-CN    # zh-CN(默认)| en
```

只影响推送文案——事件标签、降价句、摘要标题,以及 `-dry-run` 的终端输出。
标点跟着语言走(中文全角、英文半角),英文的计数会区分单复数
(`1 price drop` / `2 price drops`)。

两件它**不改**的事:

- **日志与错误信息**始终是中文。那是给跑这个进程的人看的,翻译它们只会让维护成本翻倍。
- **商品标题**的语言由抓取的地区决定。所以 `lang: en` 配 `regions: [CN]`
  会得到英文外壳 + 中文标题的混合体——这是预期行为,不是 bug。

`lang` 取值无法识别时直接报错退出,而不是静默回退默认值。

## 配置与密钥

配置里的 `${VAR}` 从环境变量展开,`${VAR:-默认值}` 可给回退值。
**引用了未设置且无默认值的变量会直接报错退出**,而不是静默变成空串让推送悄悄失效。
`enabled: false` 的渠道整棵子树跳过展开,所以你不必为用不上的渠道去设环境变量。

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

- **默认间隔 120 秒,调更短没有意义**——CDN 缓存 120 秒,你只会拿到同一份副本,
  徒增请求量。配置低于 120 秒时程序会给出警告。
- 本项目**不做** TLS 指纹伪装、UA 轮换、代理池。UA 固定为一个真实浏览器标识:
  稳定的 UA 比随机变化的更不像自动化流量。
- 同一轮内的请求串行发出,相邻请求间随机停顿 1–3 秒。
- 遇 429/503 按指数退避并遵从 `Retry-After`。

代理(`http.proxy`,支持 http/https/socks5)的用途是**修正出口地区**——
Apple 按 IP 判定地区——而不是隐藏身份。若抓到的商品自报货币与目标地区不符,
程序会报错而不是把别国数据混进状态库。

> [!WARNING]
> 这道护栏只在货币确实不同时有效。**BE、DE、ES、FR、IE、IT、NL 同为 EUR**,
> 代理出口落在错误的欧元区国家时货币一致、内容却是别国的,程序发现不了。
> 同时监控多个欧元区地区时,请自行确认出口 IP 与目标地区匹配。

## 部署

`deploy/` 下有现成的单元文件:

- **mac mini**:`com.refurb-sentry.plist` → `~/Library/LaunchAgents/`,`launchctl load`
- **Linux VPS**:`refurb-sentry.service` → `/etc/systemd/system/`,`systemctl enable --now`

两者都配置了自动重启和 30 秒退避。程序收到 SIGTERM 会先落盘状态再退出。

<details>
<summary>为什么不推荐 GitHub Actions / Cloudflare Workers</summary>

- **GitHub Actions**:cron 最短 5 分钟,官方明确说明高负载时可能延迟甚至丢弃任务;
  公共仓库在 60 天无活动后会自动禁用定时工作流;状态还得 commit 回仓库。
- **Cloudflare Workers**:免费套餐 Cron Trigger 的 CPU 时间上限是 10ms,
  而解析一个 mac 分类页(1.3MB HTML、200+ 件商品)必然超时,需要付费套餐;
  且出口 IP 遍布全球数据中心,地区判定不可控。

</details>

## 状态与可靠性

状态文件默认 `data/state.json`,记录每件商品的货号、标题、当前价格、维度和首见/末见时间。
采用"写临时文件 + rename"的原子替换,断电或被 kill 都不会留下半截文件。
想重建基线:删掉它,下次启动会重新静默建基线。

三条刻意的设计,都是为了不让通知丢失或刷屏:

- **基线按地区/分类分别记录**。给运行中的实例新增一个地区或分类时,新范围会自己静默建基线,
  不会把数百件在架商品当成新上架推给你;首轮若某个地区抓取失败,它也不会被误标为已建基线。
- **抓取失败绝不触发下架**。任何错误都跳过该分类而不改动状态,连续多轮空结果才认定真空。
- **只要有一条变动没能送达任何渠道,整轮基线就回滚**。这批变动会在下一轮重新产生并重试,不会被悄悄吞掉。
  内存基线在比对前就已快照,失败时整体还原,常驻进程因此是真的重试,而不只是跳过落盘。
  回滚是整轮粒度的,同轮里已送达的变动会在重试时再推一次——重复通知优于丢失通知。
  重试有上限:正文超出 Telegram 长度限制、webhook 恒返 400 这类失败重试多少次都不会好,
  无限重试会让你每个 interval 收一次重复通知而基线永不推进。连续回滚若干轮后会强制推进,
  丢失的事件记进 ERROR 日志。
  某条变动只要送达了至少一个渠道就算成功;失败的渠道会丢掉这条消息,日志里有 ERROR 记录。

## 开发

```bash
go test ./... && go vet ./... && gofmt -l .
```

新增地区只需在 `internal/apple/regions.go` 的表里加一行,启动校验会验证其可用性。
新货币记得同时在 `internal/apple/model.go` 的符号表里补一项,否则会退化成 `XXX 999` 的展示。

同样这三项检查会在每次 push 和 pull request 时由 CI 跑一遍。推一个 `v*` 标签则会用
GoReleaser 构建各平台归档并发布成 GitHub release。

## 免责声明

本项目与 Apple Inc. 无任何隶属、赞助或背书关系,也非 Apple 官方产品。
Apple、MacBook、iPad、Apple Watch 等为 Apple Inc. 的商标。

本工具仅抓取 Apple 官网**公开的**翻新产品列表页,用于个人购买决策的辅助监控;
所抓取的商品信息(标题、价格、图片链接等)版权归 Apple Inc. 所有。
请遵守你所在地区的法律法规与 Apple 网站的使用条款,自行承担使用风险。

## License

MIT
