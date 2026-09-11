# refurb-sentry 项目约定

> 这份文档只记**红线、结论与跨文件的约定**。每条「为什么这么做」的完整推理,
> 都写在它守护的那段代码旁边——动某块代码前先读那个文件的注释,不要只读这里。

## 数据源的既定事实

这些都是实测结论,不要凭直觉推翻:

- 翻新列表页是**公开静态页**,裸请求即 200,响应头 `cache-control: public, max-age=120, s-maxage=120`,
  命中 CDN 边缘缓存(约 50ms)。**没有风控**,不要引入 TLS 指纹伪装、UA 轮换或代理池。
- `s-maxage=120` 是轮询间隔的物理下限。任何「提高抓取频率以更早发现新品」的改动都是无效的
  (`Validate` 已为此给 warning)。
- 商品数据全量内嵌在 `window.REFURB_GRID_BOOTSTRAP` 的 `tiles[]` 里,
  **一分类一请求拿全量,无分页**。唯一的例外是 `http.fill_missing_memory`(默认关闭)。
- 价格**必须**取 `price.currentPrice.raw_amount`;`amount` 在部分分类(watch)里混有 HTML。
  `fullMrpPrice` 恒为空,拿不到官方原价,降价只能靠自己的历史快照比对。见 `internal/apple/model.go`。
- `filters.dimensions` 的 key 集合**随分类而变**,因此它是 `map[string]string`,
  规则匹配也必须是通用 key-value,不能定义成固定字段。
- **同一分类内,上游给不给某个维度也不一致**(实测 CN 站 206 件 mac 里有 58 件没有 `tsMemorySize`)。
  而规则把维度缺失判为不匹配,于是「内存 32GB 以上」这类规则会**静默漏掉整整一档机型**。
  `http.fill_missing_memory` 为此补抓详情页;它的解析判据、四条边界与「懒补齐」的取舍
  见 `internal/apple/detail.go` 与 `runner.go` 的 `fillMissingMemory`。
- 数据源**没有库存数量**。事件只能是上架/降价/下架。
- 地区×分类矩阵是稀疏的,三种情况必须分开处理:CN/HK 的 iphone、appletv 返回 **404**;
  US 的 appletv/airpods/homepod 返回 200 但无 bootstrap。见 `internal/apple/fetch.go`。
- 货币护栏只能发现跨币种的串站。**BE/DE/ES/FR/IE/IT/NL 同为 EUR**,
  代理落到错误的欧元区国家时它发现不了,README 已如实说明。

### 两个脆弱解析点

改动前务必读该文件的注释。两处的失败方式都是**静默的**:

1. `internal/filter/chip.go` — 芯片型号与 CPU/GPU 核心数不在 dimensions 里,只在 `title` 字符串,
   且各语言语序完全不同。正则因此把芯片指示词与型号**解耦**、并同时认两种语序,不按地区分派。
2. `internal/apple/detail.go` — 详情页补内存。判据语言无关:用列表页已知的 `dimensionCapacity`
   作锚点排除存储,剩余不是恰好一条就放弃。**缺锚点时必须直接放弃**。

两处栽在同一个坑上:**标题与容量文本都混用多种 Unicode 分隔符**,同一页面内都不统一
(U+00A0 西/意/法、U+2011 德、U+2014 澳)。码位一律写 `\u` 转义而非字面字符,
空白一律用 `\p{Zs}` 整类而非手抄码位表——两种写法的退化都不会报错,只会让某个地区悄悄失灵。

## 不能破坏的正确性约束

1. **冷启动必须静默,且按 region/category 分别记录**。`State.Bootstrapped` 是 `map[string]bool`
   而非全局布尔,后者会在首轮部分失败、以及给运行中的实例新增地区时刷屏。
2. **抓取失败绝不能触发下架**。任何 error / `ErrNoBootstrap` 都必须跳过该分类而不调用 `State.Apply`。
   连续空结果要攒够 `emptyStreakThreshold` 轮才认定真空。
3. **首轮遇到不可用分类必须在写入任何状态前退出**。`RunOnce` 为此刻意分成
   「先抓全、再统一比对」两阶段,否则会留下只覆盖部分范围的残缺基线。
4. **`-dry-run` 不得有任何副作用**,包括不写状态文件、不推进 `LastSummaryAt`、不清零计数器。
5. **有事件未送达任何渠道时,整轮基线必须回滚**。关键在于只跳过落盘是不够的:
   `settle` 在 `Apply` 前 `Clone` 快照,失败时 `Restore` **原地覆盖**(不能换指针,
   `main` 还持有同一个 `*State`)。回滚有上限 `maxRollbacks`,攒够后强制推进并记 ERROR 日志——
   丢一批通知是坏结果,无限刷屏是更坏的结果。

推导过程分别在 `internal/state/state.go` 与 `internal/app/runner.go` 的注释里。
`State` 的 `Clone` 是逐字段手写的,新增字段漏拷不会有编译错误,后果是回滚时该字段不退回。

## 设计取舍

- **每日汇总(`notify.daily_summary`)默认开启,定位是活性证明而不是报表**。
  这个项目的失败模式全是静默的(进程挂了、规则维度键名抄错、芯片正则被上游打挂、
  webhook 返回 200 但业务失败),用户看到的都是同一个现象:手机安静。
  日报把「安静」从二义变成单义,因此它报的数字必须**同时覆盖抓取与推送两条链路**:
  全部事件数证明抓取还在产出,命中规则数证明规则还在命中。它只读已有状态,不产生额外抓取。
  相关的五条约束——`Validate` 绝不许把空的 `daily_summary` 回填成默认时刻(那是唯一的关闭手段)、
  YAML 的 null 同样算关闭、送达失败不回滚基线、独立于本轮推送的成败、陈旧范围要在日报里标出——
  见 `internal/config/config.go`、`load.go` 与 `runner.go` 的注释。
- **日报的时区取自进程的 `TZ` 环境变量(未设置则是系统时区),刻意不做成配置项**。
  再加一个 `notify.timezone` 会出现两套时间口径,日志里 09:00 的那行旁边配着一份
  「09:00 的日报」却不是同一时刻。docker-compose 已默认 `TZ: ${TZ:-Asia/Shanghai}`(镜像装了 tzdata);
  裸机部署要留意云主机的系统时区常常是 UTC。两版 README 与三份部署文件都写明了。
- **送达判断只看 HTTP 状态码,这是约束 5 的已知盲区**。飞书/企微/钉钉/Server 酱业务失败时
  照样返回 200,那批事件会被永久丢失而日志里只有「推送成功」。为什么不在 `webhook.go` 里
  解析各家错误码、以及要收紧时的正确做法,见该文件注释。四家的示例配置与两版 README 都写明了。
- 通用 webhook 只做模板渲染,**不算签名**。飞书/钉钉的「加签」安全模式因此用不了,
  改用自定义关键词或 IP 白名单。
- 规则过滤发生在 **diff 之后、推送之前**。状态库始终记录全部商品,
  这样以后放宽规则时,早已在架的商品不会被误报成新上架。
- 配置在 **YAML 节点层**展开环境变量,不是文本替换——否则注释里的 `${VAR}` 会被误当引用。
  且 `enabled: false` 的渠道整棵子树跳过展开:示例配置里禁用的 telegram 渠道
  不该逼用户去设 `TELEGRAM_BOT_TOKEN`。
- 依赖只有 `gopkg.in/yaml.v3` 和 `golang.org/x/net`(SOCKS5),其余全标准库。
  加新依赖前先确认标准库真的做不到。
- **日志走 stderr,控制台通知与 `-list-dims` 的输出走 stdout**。三种守护方式都同时收两个流,
  所以搞错不影响功能,只会误导那些自己重定向输出的人——只写 `>log.txt` 会把全部日志漏掉。
- 日志用自定义的 `consoleHandler` 而非 slog 的 TextHandler。理由、`WithAttrs` 的分组语义
  与引号规则见 `cmd/refurb-sentry/log.go`。
- `-list-dims` 的规则骨架**默认不打印**,由 `-skeleton` 开启。骨架里也**不许出现
  `min_cpu_cores`、`max_price` 这类没有可枚举取值的字段**:骨架其余每一行都来自当前真实在售的
  商品,凭空塞一个数字等于给出一个假结论(曾实测误导过用户)。这些字段的说明属于 README 的
  规则字段速查表。

## 同一事实出现在多处时

这个项目的可信度建立在「这些都是实测结论」上,而**同一个事实写在两处,过期的那份不会有任何东西报错**。
以下是已知的、必须手工同步的重复:

| 改了什么 | 必须一起改 |
|---|---|
| 日志行格式 | 稳态体积估算(**每行约 40 字节、一年约 10MB**)出现在两版 README、`deploy/docker-compose.yml` 与 `deploy/com.refurb-sentry.plist` 的注释里——这四处的读者都要就地看到这个数。曾经 README 写 25MB 而 compose 写 50MB,谁都不知道该信哪个 |
| 参数、规则字段、地区/分类、行为约束 | `README.md`(英)与 `README.zh-CN.md`(中)内容对等、顶部互链。只改一版会留下一份静默过期的文档 |
| 新增地区 | `internal/apple/regions.go` 加一行(启动校验会验证可用性);`.github/ISSUE_TEMPLATE/spec_parse.yml` 的地区下拉是**手抄清单**,漏了不报错,只会让新地区的用户提不了规格解析 issue;新货币还要补 `internal/apple/model.go` 的符号表,否则展示退化成 "XXX 999" |

配置示例分两份:`configs/config.example.yaml` 是十来行的**最小配置**(README 快速开始、
cask caveats、compose 注释里让用户 cp 的都是它),`configs/config.full.yaml` 是全量参考,
列出每个可配项与五家群机器人的 webhook 模板。**最小配置里不许重复任何事实**(地区清单、
`s-maxage=120` 的理由、各家返回 200 的坑),那些只留在全量版与 README 里;它靠默认值补全其余字段,
且加载**不得产生任何 warning**——起手第一次运行就看到 WARN 会让人以为自己配错了。
全量版里的 webhook 模板写错既不编译报错也碰不到其它测试,只会在真收到事件那天静默 400,
测试为此把每份模板真发一遍到本地服务器校验载荷。

实测 MX、IN 没有翻新店(返回 404),不要加。

## 语言约定

- **日志与错误信息一律保持中文**,不要顺手翻译:那是给运维和开发者看的,译了没有收益、
  维护成本翻倍。
- 推送文案集中在 `internal/notify/lang.go` 的 `phrases` 表里,由 `notify.lang` 选择(zh-CN / en),
  加语言只需往表里加一项。标点也属于文案的一部分:中文全角、英文半角。
- **日志、`consoleHeader` 这类终端输出以及代码注释一律保持半角**,
  那是等宽文本里的常规写法,不要顺手一起改成全角。
- `EventKind` 只保留语言中立的 `listed` / `price_drop` / `delisted`;
  展示用的 label 属于 notify 层,不要再往 state 里塞 `Label()` 之类的方法。

## 开发

```bash
go test ./... && go vet ./... && gofmt -l .
./refurb-sentry -config configs/config.yaml -list-dims              # 查当前可用的过滤维度
./refurb-sentry -config configs/config.yaml -list-dims -skeleton    # 另附可粘贴的规则骨架
./refurb-sentry -config configs/config.yaml -once -dry-run
```

CI(`.github/workflows/ci.yml`)在每次 push / PR 上跑同样的三项检查。

发版是推 `v*` 标签,`.github/workflows/release.yml` 用 GoReleaser(`.goreleaser.yaml`)构建
darwin/linux 的 amd64、arm64、armv7 归档并发布 release。版本号通过 `-ldflags -X main.version=`
注入,`go install` 装的会显示 `dev`。

- Homebrew 走 **cask 而非 formula**——GoReleaser 从 v2.16 起完全弃用了 `brews`,
  预编译二进制本来就该走 cask。推到 `hh-io/homebrew-tap`,需要仓库 secret
  `HOMEBREW_TAP_GITHUB_TOKEN`(对该 tap 仓库有 contents 写权限的 PAT;
  workflow 自带的 `GITHUB_TOKEN` 只能写当前仓库)。未配置时 `skip_upload` 的模板会跳过 cask,
  不让一个可选渠道拖垮归档与镜像的发布。二进制没有 Apple 签名与公证,
  cask 用 postflight 钩子摘 quarantine 属性,否则用户第一次运行就被 Gatekeeper 拦下——
  这一点在 caveats 里对用户如实写明了。
- 同一个 workflow 并行构建并推送 `ghcr.io/hh-io/refurb-sentry` 的 amd64/arm64 镜像
  (根目录 `Dockerfile`,同样注入版本号)。镜像**在构建机的原生架构上交叉编译**而不是靠 QEMU
  模拟目标架构——纯 Go 关掉 CGO 就能直接出目标架构的静态二进制,快一个数量级。
- 容器以 **uid 1000** 运行,`deploy/docker-compose.yml` 因此默认用命名卷存状态:
  Docker 会按镜像里 `/app/data` 的属主初始化命名卷,用户不必 chown。
  换成 bind mount 就得自己 `chown 1000:1000` 宿主目录,否则状态写不进去、每轮基线回滚。
- Docker 下 `.env` 只用于 compose 文件自身的 `${VAR}` 插值,**不注入容器**:
  环境变量开关必须写进 compose 的 `environment:` 段,compose 里已留了注释掉的一行。

`assets/icon.png`(推送通知图标)采用 macOSicons 上的 Apple Store 图标
(https://macosicons.com/?icon=ijSPtRVRMC),规格为原始 1024x1024 PNG。
`assets/make-icon.py` 为此前纯代码生成的自制图标脚本,保留供参考。
