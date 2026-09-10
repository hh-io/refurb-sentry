# refurb-sentry 项目约定

## 数据源的既定事实(改代码前先读,这些都是实测结论,不要凭直觉推翻)

- 翻新列表页是**公开静态页**,裸请求即 200,响应头 `cache-control: public, max-age=120, s-maxage=120`,
  命中 CDN 边缘缓存(约 50ms)。**没有风控**,不要引入 TLS 指纹伪装、UA 轮换或代理池。
- `s-maxage=120` 决定了轮询间隔的物理下限。任何"提高抓取频率以更早发现新品"的改动都是无效的。
- 商品数据全量内嵌在 `window.REFURB_GRID_BOOTSTRAP` 的 `tiles[]` 里,**一分类一请求拿全量,无分页**。
- 价格**必须**取 `price.currentPrice.raw_amount`(纯数字字符串)。
  `amount` 字段在部分分类(watch)里混有 HTML,如 `<span class="visuallyhidden">Now </span>$209.00`。
- `price.fullMrpPrice` 恒为空,拿不到官方原价;降价只能靠自己的历史快照比对。
- `filters.dimensions` 的 key 集合**随分类而变**(mac 有 `tsMemorySize`/`dimensionCapacity`,
  watch 是 `dimensionCaseSize`/`dimensionCaseMaterial`/`dimensionConnection`)。
  因此它是 `map[string]string`,规则匹配也必须是通用 key-value,不能定义成固定字段。
- **芯片型号与 CPU/GPU 核心数不在 dimensions 里,只在 `title` 字符串**,且各语言语序完全不同:
  FR「puce Apple M4, CPU 10 cœurs」型号在指示词之后、数字在量词之前;
  IT/ES「chip Apple M2, CPU 8-core」;NL「Apple M4-chip」用连字符连写;
  KR「Apple M5 Pro 칩(15코어 CPU)」;CN「芯片/核中央处理器」;HK/TW「晶片/核心 CPU」;JP「チップ/コアCPU」。
  因此正则**把芯片指示词与型号解耦**、并同时认两种语序,不按地区分派。
  这是全项目唯一的脆弱解析点,见 `internal/filter/chip.go`。
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

- 规则过滤发生在 **diff 之后、推送之前**。状态库始终记录全部商品,
  这样以后放宽规则时,早已在架的商品不会被误报成新上架。
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
  示例配置里的 body 模板写错既不编译报错也碰不到其它测试,只会在真收到事件那天静默 400,
  `TestExampleConfigWebhookTemplatesRender` 为此把每份模板真发一遍到本地服务器,
  按 Content-Type 校验载荷是合法 JSON / 表单,且商品标题确实落进了载荷。
- 货币护栏只能发现跨币种的串站。**BE/DE/ES/FR/IE/IT/NL 同为 EUR**,
  代理落到错误的欧元区国家时它发现不了,README 已如实说明。

## 开发

```bash
go test ./... && go vet ./... && gofmt -l .
./refurb-sentry -config configs/config.yaml -list-dims   # 查当前可用的过滤维度
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
