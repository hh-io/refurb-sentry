# refurb-sentry 项目约定

## 数据源的既定事实(改代码前先读,这些都是实测结论,不要凭直觉推翻)

- 翻新列表页是**公开静态页**,裸请求即 200,响应头 `cache-control: public, max-age=120, s-maxage=120`,
  命中 CDN 边缘缓存(约 50ms)。**没有风控**,不要引入 TLS 指纹伪装、UA 轮换或代理池。
- `s-maxage=120` 决定了轮询间隔的物理下限。任何"提高抓取频率以更早发现新品"的改动都是无效的。
- 商品数据全量内嵌在 `window.REFURB_GRID_BOOTSTRAP` 的 `tiles[]` 里,**一分类一请求拿全量,无分页**。
  唯一的例外是 `http.fill_missing_memory`(默认关闭),见下面关于内存维度缺失的条目。
- 价格**必须**取 `price.currentPrice.raw_amount`(纯数字字符串)。
  `amount` 字段在部分分类(watch)里混有 HTML,如 `<span class="visuallyhidden">Now </span>$209.00`。
- `price.fullMrpPrice` 恒为空,拿不到官方原价;降价只能靠自己的历史快照比对。
- `filters.dimensions` 的 key 集合**随分类而变**(mac 有 `tsMemorySize`/`dimensionCapacity`,
  watch 是 `dimensionCaseSize`/`dimensionCaseMaterial`/`dimensionConnection`)。
  因此它是 `map[string]string`,规则匹配也必须是通用 key-value,不能定义成固定字段。
- **同一分类内,上游给不给某个维度也不一致**。实测 CN 站 206 件 mac 里有 58 件没有
  `tsMemorySize`:16 英寸 MacBook Pro 的 M5 Pro(22 件)与 M5 Max(15 件)、
  Studio Display(13 件)、MacBook Neo(8 件)。整个 tile 里都没有内存字段,标题里也没有。
  而规则把维度缺失判为不匹配(`rule.go` 的 `matches`),于是「内存 32GB 以上」这类规则
  会**静默漏掉整整一档机型**——用户看不到任何异常,只是永远收不到通知。
  `http.fill_missing_memory` 为此对缺失的商品补抓一次详情页,
  从 `window.pageLevelData.Overview` 里把内存读出来写回同一个键。
  解析判据是**语言无关**的:概述栏里恰好两个带容量单位的条目(内存与存储),
  用列表页已知的 `dimensionCapacity` 作锚点排除存储,剩下唯一一条就是内存;
  剩余不是恰好一条就放弃——猜错会让规则匹配到配置完全不同的机器,比读不到更糟。
  **`dimensionCapacity` 缺失时必须直接放弃**:没有锚点就排除不掉存储条目,
  而只列出一条容量的页面会把存储当成内存交回来(实测 watch 详情页正是这样)。
  `scopeHasMemory` 那道分类闸只挡得住整类没有内存维度的分类,挡不住同一分类里
  个别既无 `tsMemorySize` 也无 `dimensionCapacity` 的商品,
  `TestParseOverviewMemoryRequiresCapacityAnchor` 守着这条。
  见 `internal/apple/detail.go`,这是第二个脆弱解析点。
  已在 14 个地区实测通过(US JP DE FR UK KR IT ES NL CH TW CA AU,HK 当前无缺失商品)。
- **容量文本里的空白同样混用 Unicode 码位**,与标题那条是同一个坑的两处现场:
  实测法国站写的是 "24\u00a0Go"(不间断空格 + 法语单位 Go/To)。
  Go 的 `\s` 只等价于 `[\t\n\f\r ]`,**不含 U+00A0**,
  只写 `\s*` 会让整个法国站一条容量条目都匹配不到、补齐静默失效。
  `detail.go` 的 `unicodeSpaces` 因此用 `\p{Zs}` 整类而不是手抄码位表——
  Zs 里有十几个码位,手抄表漏一个的失败方式同样是静默的;`\s` 仍要留着,
  它含 `\t\n\f\r` 而这些不属于 Zs。
  `TestParseOverviewMemoryUnicodeSpaces` 与 `TestParseOverviewMemoryFrenchUnits` 防这个回归——
  后者的码位得用 `nbsp := "\u00a0"` 拼进反引号字符串:
  反引号是原始字符串,里面的 `\u00a0` 只是六个字面字符,那样测试照样通过却防不住任何东西。
- 三种解析失败(缺变量 / JSON 解不开 / 候选不唯一)必须分开报告。
  排查方向完全不同,混成一句会让人查错方向——法国站那次报的是「未找到 Overview」,
  真实原因却是不间断空格导致候选为 0,白查了一轮页面结构。
  补齐是**懒的**:只有「除内存外其余条件都还可能命中某条规则」的商品才会去看详情页。
  机型、芯片、容量、价格列表页都已给全,凭它们就能否决的商品再看详情页也是白看——
  内存是它唯一还没定的条件时才值得查。实测 CN mac 因此从 45 个请求降到 19 个,
  且不再徒劳地去查 Studio Display 这类根本没有内存的商品(failed 从 13 降到 0)。
  判据见 `filter.MayMatchWithout` 与 `filter.UsesDimension`。
  四条边界由测试守着:详情页失败必须保留商品原样(否则一次 5xx 会让几十台机器凭空下架,
  `TestFillMissingMemoryFailureKeepsProduct`);结果按货号缓存
  (`TestFillMissingMemoryCachesAcrossRounds`);没有任何规则按内存过滤时整个跳过
  (`TestFillMissingMemorySkipsWhenNoRuleUsesMemory`);**整个分类都没有内存维度时一件都不抓**
  (`TestFillMissingMemorySkipsCategoryWithoutMemory`)——最后这条不只是省请求:
  实测 watch 的详情页会让 28 件手表全部「解析出内存」,那其实是表壳存储容量,
  不设这道闸就会把脏数据写进 `tsMemorySize`。
- **缓存里只许躺永久性失败**。`IsPermanentMemoryFailure` 把「页面读不出内存」
  (`ErrNoOverview`/`ErrOverviewDecode`/`ErrMemoryAmbiguous`/`ErrProductGone`)
  与「这次没读到」(超时、5xx、连接重置)分开:前者重试多少轮都是同一个结果,
  记进缓存不再查;后者**绝不能入缓存**——一次抖动就让该货号在整个进程生命周期里
  再也不被补齐,按内存过滤的规则从此静默漏掉它,而那正是这个功能要消除的问题。
  `TestFillMissingMemoryRetriesTransientFailure` 与
  `TestFillMissingMemoryCachesUnreadablePage` 一正一反守着。
  但重试必须有上限(`maxMemoryAttempts`,同一货号 3 轮),理由与 `maxRollbacks` 完全相同:
  上游持续 5xx 或把详情页整个封了,无限重试既补不到内存,又让每一轮都为同一批商品
  白发几十个请求。`TestFillMissingMemoryStopsRetryingAfterRepeatedFailure` 防这个。
  详情页 404 用单独的 `ErrProductGone` 而不复用 `ErrCategoryNotAvailable`:
  翻新品常在抓列表与抓详情之间被买走,后者的文案会把运维引向 `categories` 配置。
  补齐失败打 **Warn**(不是 Debug):默认 `log_level: info`,留在 Debug 就又成了静默失效;
  但只对本轮新出现的失败告警,已缓存的那批每轮都命中缓存,一起算会每个 interval 刷一遍。
- **开了 `fill_missing_memory` 却没有规则约束 `tsMemorySize` 时,`Validate` 给 warning**。
  补齐确实该整个跳过(补了也改变不了推送结果),但沉默会让人以为它在生效。
- **芯片型号与 CPU/GPU 核心数不在 dimensions 里,只在 `title` 字符串**,且各语言语序完全不同:
  FR「puce Apple M4, CPU 10 cœurs」型号在指示词之后、数字在量词之前;
  IT/ES「chip Apple M2, CPU 8-core」;NL「Apple M4-chip」用连字符连写;
  KR「Apple M5 Pro 칩(15코어 CPU)」;CN「芯片/核中央处理器」;HK/TW「晶片/核心 CPU」;JP「チップ/コアCPU」。
  因此正则**把芯片指示词与型号解耦**、并同时认两种语序,不按地区分派。
  这是全项目最脆弱的解析点,见 `internal/filter/chip.go`。
- **标题里混用多种 Unicode 分隔符**,同一页面内都不统一:西/意/法站用 U+00A0 分隔
  "M4\u00a0Pro" 而同页其它机型用普通空格;德国站用 U+2011;澳洲站用 U+2014。
  `dashNormalizer` 里的码位**必须写成 `\u` 转义**,绝不能写字面字符——
  曾因 U+00A0 在编辑过程中退化成 U+0020,替换规则变成「空格换空格」而静默失效,
  导致西/意/法站的 "A18 Pro" 全被截成 "A18",配了 `chips: [M4 Pro]` 的规则莫名漏推。
  `TestNormalizeTitleCoversCodepoints` 与 `TestParseSpecNormalizesSeparators` 专门防这个回归。
- 数据源**没有库存数量**。事件只能是上架/降价/下架。
- 地区×分类矩阵是稀疏的:CN/HK 的 iphone、appletv 返回 **404**;
  US 的 appletv/airpods/homepod 返回 200 但无 bootstrap。这三种情况必须分开处理。

## 三条不能破坏的正确性约束

1. **冷启动必须静默,且按 region/category 分别记录**。首次抓取时数百件在架商品全是"新增",
   直接推送会刷屏。`State.Bootstrapped` 是 `map[string]bool` 而非全局布尔——
   全局标志有两个漏洞:首轮某地区失败而其余成功会把它也标记为已建基线;
   给运行中的实例新增地区/分类时同样会刷屏。改动这里前先看
   `TestNewScopeBootstrapsSilently` 与 `TestFailedScopeStaysUnbootstrapped`。
2. **抓取失败绝不能触发下架**。任何 error / `ErrNoBootstrap` 都必须跳过该分类而不调用 `State.Apply`。
   连续空结果要攒够 `emptyStreakThreshold` 轮才认定真空。
3. **首轮遇到不可用分类必须在写入任何状态前退出**。`RunOnce` 为此刻意分成
   「先抓全、再统一比对」两阶段,否则会留下只覆盖部分范围的残缺基线。

4. **`-dry-run` 不得有任何副作用**,包括不写状态文件。一次 dry-run 若撞上真实降价,
   把它吸收进基线会让正式进程永远不再推送该降价。
5. **有事件未送达任何渠道时,整轮基线必须回滚**,让这批变动下一轮重新产生并重试,
   而不是被永久吞掉。`Multi.Send` 为此返回成功渠道数。
   关键在于**只跳过落盘是不够的**:`State.Apply` 原地推进内存基线,常驻进程下一轮
   拿的是同一个指针,比对不出这批事件——所以 `settle` 在 Apply 前先 `Clone` 快照,
   失败时 `Restore` 原地覆盖(不能换指针,`main` 还持有同一个 `*State`)。
   逐条推送时只要有一条全败就算整轮失败:回滚是整轮粒度的,已送达的那几条
   下一轮会重复推送一次——**重复优于永久丢失**。
   但回滚必须有上限(`maxRollbacks`):正文超长、webhook 恒返 400 这类失败重试多少次
   都不会好,无限回滚会让用户每个 interval 收一次重复通知且基线永不推进。
   攒够轮数后强制推进并把丢失的事件记进 ERROR 日志——丢一批通知是坏结果,
   无限刷屏是更坏的结果。
   `TestFailedDispatchRollsBackBaseline`、`TestPartialDeliveryIsNotSuccess`
   与 `TestPermanentFailureStopsRollingBack` 防这个回归。

## 设计取舍

- **每日汇总(`notify.daily_summary`)默认开启,定位是活性证明而不是报表**。
  这个项目的失败模式全是静默的(进程挂了、规则维度键名抄错、芯片正则被上游打挂、
  webhook 返回 200 但业务失败),用户看到的都是同一个现象:手机安静。
  日报把「安静」从二义变成单义——它到了说明抓取与推送链路都通。
  因此它报的数字必须同时覆盖两条链路:**全部事件数**证明抓取还在产出,
  **命中规则数**证明规则还在命中。只报后者的话,规则写错时日报显示「0 变动」,
  又和「上游真没动」分不清了,等于把要解决的问题原样带回来。
  它只读已有状态,不产生任何额外抓取。
  默认开启而非关闭:这个工具的常态是配一条窄规则等上几个月,
  等待期越长,「安静」这个信号越没用;首次启动那一份还会立刻告诉用户
  规则当前命中几件,把「规则写错」从几个月后的困惑提前到第一分钟。
  **`Validate` 因此绝不能把空的 `daily_summary` 回填成默认时刻**——
  默认值由 `Default()` 给,显式写空串是唯一的关闭手段,一回填就再也关不掉,
  而这是个会往手机上推东西的功能。`REFURB_DAILY_SUMMARY` 也因此用 `LookupEnv`
  而不是 `Getenv` 判空:配置只读挂载时(compose 正是这么挂的)它是唯一的开关。
  `TestDailySummaryDefaultsToEnabled`、`TestDailySummaryExplicitlyDisabled`
  与 `TestDailySummaryEnvCanDisableAndOverride` 三条一起守着这对相反的语义。
- **日报送达失败绝不回滚基线**。它是派生信息,为它回滚会让本轮已送达的真实事件
  下一轮重复推送。失败只是不推进 `LastSummaryAt`,下一轮重试;
  但重试有上限(`maxSummaryAttempts`,理由同 `maxRollbacks`:渠道恒定失败时
  120s 一轮会让它一天重试几百次)。放弃时**只推进 LastSummaryAt 而不清零计数器**,
  那批变动并进下一份日报。`CountersSince` 与 `LastSummaryAt` 因此是两个字段:
  前者是计数区间的起点,只在成功送达后推进,「自上次汇总以来」才始终准确。
  `TestFailedSummaryDoesNotRollBackBaseline` 与
  `TestDailySummaryStopsRetryingButKeepsCounters` 守着这两条。
- **计数器放在 `State` 里,因此必须手动加进 `Clone`**。`Clone` 是逐字段手写的,
  新增字段漏拷不会有任何编译错误,后果却是推送全败回滚时计数不退回、
  下一轮重新产生的同一批事件被计第二次,日报数字凭空翻倍。
  `TestCloneCoversEveryField` 用反射遍历每个字段守这个:map 字段必须既非空
  又不与原状态共享底层数组——新加的字段自动纳入,不必手动登记。
- **`-dry-run` 同样不得触碰日报状态**:推进 `LastSummaryAt` 会让正式进程当天不再汇总,
  清零计数则会把那批变动从下一份日报里抹掉,都属于约束 4。
- **日报独立于本轮推送的成败**,`settle` 因此拆成 `reconcile` + 日报两段。
  挂在成功路径上会正好在系统出问题的那几轮没有日报:`dispatch` 失败最典型的场景是
  「某条事件的载荷被拒」(正文超长、webhook 对该载荷恒返 400,即 `maxRollbacks`
  存在的理由),这时渠道本身是好的,短小的日报照样发得出去,而那恰恰是最需要它的时候。
  `TestSummarySentEvenWhenDispatchFails` 防这个。
- **抓取失败的范围必须在日报里标出「数据陈旧」**(连续三轮没更新,与
  `emptyStreakThreshold` 同样的理由)。这类范围会被 `RunOnce` 跳过、商品原样留在状态里,
  不标的话它与「一切正常但没有变动」完全一样——等于在范围粒度上把刚消除的二义放回来。
  判据是该范围全部 `Entry.LastSeen` 的最大值,`TestStaleScopeIsMarkedInSummary` 守着。
- **日报的时区取自进程的 `TZ` 环境变量(未设置则是系统时区),刻意不做成配置项**。
  日志时间戳本来就跟着 `TZ` 走,再加一个 `notify.timezone` 会出现两套时间口径,
  日志里 09:00 的那一行旁边配着一份「09:00 的日报」却不是同一时刻。
  docker-compose 已默认 `TZ: ${TZ:-Asia/Shanghai}`(镜像装了 tzdata);
  裸机部署要留意云主机的系统时区常常是 UTC。两版 README 与三份部署文件都写明了。
- **日报判重按自然日,不是拿 `LastSummaryAt` 与当天的触发点比大小**。
  从未汇总过时刻意不等到点就发一份(两版 README 把它写成了安装确认),
  那份就落在触发点之前,再用触发点判重会在同一天里发出第二份。
  `TestFirstSummarySendsImmediatelyThenOncePerDay` 防这个。
  重试计数同理要按 `summaryFor` 跨天归零,否则昨天失败一次的余额会留给今天。

- 规则过滤发生在 **diff 之后、推送之前**。状态库始终记录全部商品,
  这样以后放宽规则时,早已在架的商品不会被误报成新上架。
- `-list-dims` 的规则骨架**默认不打印**,由 `-skeleton` 开启。这个命令的日常用途是
  「现在有哪些取值」与「芯片解析还正常吗」(见 README 的排错一节),骨架对这两件事
  都是噪音,而且每个 scope 十几行,地区一多就把维度表淹没了。
  骨架里也**不许出现 `min_cpu_cores`、`max_price` 这类没有可枚举取值的字段**:
  骨架其余每一行都来自当前真实在售的商品,读者会合理地认为整段都是这个性质,
  往里塞一个凭空的数字就等于给出一个假结论。曾经附过 `# max_price: 20000`,
  实测误导过用户(「为什么会有个最大价格 2 万」),而顺手去掉那个 `#` 之后,
  它会把 CN 站 2.1 万起步的高配 MacBook Pro 全部静默挡在门外。
  这些字段的说明属于 README 的规则字段速查表。
  `TestRuleSkeletonCarriesNoInventedValues` 防这个回归。
- 依赖只有 `gopkg.in/yaml.v3` 和 `golang.org/x/net`(SOCKS5),其余全标准库。
  加新依赖前先确认标准库真的做不到。
- 配置在 **YAML 节点层**展开环境变量,不是文本替换——否则注释里的 `${VAR}` 会被误当引用。
  且 `enabled: false` 的渠道整棵子树跳过展开:示例配置里禁用的 telegram 渠道
  不该逼用户去设 `TELEGRAM_BOT_TOKEN`(这曾让 README 的快速开始必然失败)。
- **送达判断只看 HTTP 状态码,这是约束 5 的已知盲区**。飞书/企微/钉钉/Server 酱
  业务失败时照样返回 200,错误码在响应体里(钉钉 `errcode`、飞书 `code`)。
  于是 `Send` 返回 nil、`Multi.Send` 记为送达、基线照常推进,那批事件永久丢失,
  日志里只有「推送成功」。四家的示例配置与两版 README 都对用户写明了这一点。
  没有在 `webhook.go` 里解析各家错误码:那会把渠道特定逻辑塞进通用实现,
  而通用实现覆盖一切渠道正是它的价值。要收紧的话,正确做法是加一个通用的
  响应体断言配置项(例如「响应必须包含某字符串才算成功」),而不是硬编码渠道。
- 通用 webhook 只做模板渲染,**不算签名**。飞书/钉钉的「加签」安全模式因此用不了,
  两版 README 与示例配置都写明改用自定义关键词或 IP 白名单——
  为一个渠道引入 HMAC 分支不如把边界说清楚。
  模板写错既不编译报错也碰不到其它测试,只会在真收到事件那天静默 400,
  `TestFullConfigWebhookTemplatesRender` 为此把每份模板真发一遍到本地服务器,
  按 Content-Type 校验载荷是合法 JSON / 表单,且商品标题确实落进了载荷。
- 配置示例分两份:`configs/config.example.yaml` 是十来行的**最小配置**,
  README 快速开始、cask caveats、compose 注释里让用户 cp 的都是它;
  `configs/config.full.yaml` 是全量参考,列出每个可配项与五家群机器人的 webhook 模板。
  最小配置里**不许重复任何事实**(地区清单、`s-maxage=120` 的理由、各家返回 200 的坑),
  那些只留在全量版与 README 里——同一个事实写在两处,过期的那份不会有任何东西报错。
  它靠默认值补全其余字段,`TestExampleConfigIsMinimalAndRunnable` 守着它能加载、
  且**不产生任何 warning**:起手第一次运行就看到 WARN 会让人以为自己配错了。
- 货币护栏只能发现跨币种的串站。**BE/DE/ES/FR/IE/IT/NL 同为 EUR**,
  代理落到错误的欧元区国家时它发现不了,README 已如实说明。

## 开发

```bash
go test ./... && go vet ./... && gofmt -l .
./refurb-sentry -config configs/config.yaml -list-dims              # 查当前可用的过滤维度
./refurb-sentry -config configs/config.yaml -list-dims -skeleton    # 另附可粘贴的规则骨架
./refurb-sentry -config configs/config.yaml -once -dry-run
```

CI(`.github/workflows/ci.yml`)在每次 push / PR 上跑同样的三项检查。
发版是推 `v*` 标签,`.github/workflows/release.yml` 用 GoReleaser
(`.goreleaser.yaml`)构建 darwin/linux 的 amd64、arm64、armv7 归档并发布 release。
版本号通过 `-ldflags -X main.version=` 注入,`go install` 装的会显示 `dev`。
Homebrew 走 **cask 而非 formula**——GoReleaser 从 v2.16 起完全弃用了 `brews`,
预编译二进制本来就该走 cask。推到 `hh-io/homebrew-tap`,需要仓库 secret
`HOMEBREW_TAP_GITHUB_TOKEN`(对该 tap 仓库有 contents 写权限的 PAT;
workflow 自带的 `GITHUB_TOKEN` 只能写当前仓库)。未配置时 `skip_upload` 的模板
会跳过 cask,不让一个可选渠道拖垮归档与镜像的发布。
二进制没有 Apple 签名与公证,cask 用 postflight 钩子摘 quarantine 属性,
否则用户第一次运行就被 Gatekeeper 拦下——这一点在 caveats 里对用户如实写明了。
同一个 workflow 并行构建并推送 `ghcr.io/hh-io/refurb-sentry` 的 amd64/arm64 镜像
(根目录 `Dockerfile`,同样注入版本号)。镜像**在构建机的原生架构上交叉编译**而不是靠
QEMU 模拟目标架构——纯 Go 关掉 CGO 就能直接出目标架构的静态二进制,快一个数量级。
容器以 **uid 1000** 运行,`deploy/docker-compose.yml` 因此默认用命名卷存状态:
Docker 会按镜像里 `/app/data` 的属主初始化命名卷,用户不必 chown。
换成 bind mount 就得自己 `chown 1000:1000` 宿主目录,否则状态写不进去、每轮基线回滚。

`assets/icon.png`(推送通知图标)采用 macOSicons 上的 Apple Store 图标(https://macosicons.com/?icon=ijSPtRVRMC),
规格为原始 1024x1024 PNG。`assets/make-icon.py` 为此前纯代码生成的自制图标脚本,保留供参考。

新增地区:在 `internal/apple/regions.go` 的表里加一行即可,启动校验会验证可用性。
同时要同步 `.github/ISSUE_TEMPLATE/spec_parse.yml` 的地区下拉——那是个手抄的清单,漏了不会报错,只会让新地区的用户提不了规格解析 issue。
新货币记得同时在 `internal/apple/model.go` 的符号表里补一项,否则会退化成 "XXX 999" 的展示。
实测 MX、IN 没有翻新店(返回 404),不要加。

README 是中英两版(`README.md` 英文、`README.zh-CN.md` 中文),内容对等、顶部互链。
改动其中一版的事实性内容(参数、规则字段、地区/分类、行为约束)必须同步另一版——
只改一版会留下一份静默过期的文档,而这个项目的可信度正建立在"这些都是实测结论"上。
推送文案按语言集中在 `internal/notify/lang.go` 的 `phrases` 表里,由 `notify.lang` 选择(zh-CN / en)。
**日志与错误信息一律保持中文**,不要顺手翻译:那是给运维和开发者看的,译了没有收益、维护成本翻倍。
加语言时只需往表里加一项。注意漏填字段**不会**编译报错,只会得到空串并静默推出残缺文案——
`TestAllLanguagesDefineEveryPhrase` 与 `TestEveryLanguageRendersAllKinds` 专门防这个。
标点也属于文案的一部分,但**只对推送文案而言**:中文用全角、英文用半角。
只换词不换标点会让英文输出很别扭;中文这边半角逗号更要命——它和价格里的千分位
逗号是同一个字符,「RMB 600,15.0%」第一眼会被读成一个数。
`TestChinesePhrasesUseFullWidthPunctuation` 用反射遍历 phrases 防这个,
新加的文案会自动纳入,不必手动登记。
**日志、`consoleHeader` 这类终端输出以及代码注释一律保持半角**,
那是等宽文本里的常规写法,不要顺手一起改成全角。
`EventKind` 只保留语言中立的 `listed` / `price_drop` / `delisted`;
展示用的 label 属于 notify 层,不要再往 state 里塞 `Label()` 之类的方法。
