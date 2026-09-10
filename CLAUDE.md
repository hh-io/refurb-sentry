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
5. **一轮内所有渠道都推送失败时不推进基线**,让这批变动下一轮重新产生并重试,
   而不是被永久吞掉。`Multi.Send` 为此返回成功渠道数。

## 设计取舍

- 规则过滤发生在 **diff 之后、推送之前**。状态库始终记录全部商品,
  这样以后放宽规则时,早已在架的商品不会被误报成新上架。
- 依赖只有 `gopkg.in/yaml.v3` 和 `golang.org/x/net`(SOCKS5),其余全标准库。
  加新依赖前先确认标准库真的做不到。
- 配置在 **YAML 节点层**展开环境变量,不是文本替换——否则注释里的 `${VAR}` 会被误当引用。
  且 `enabled: false` 的渠道整棵子树跳过展开:示例配置里禁用的 telegram 渠道
  不该逼用户去设 `TELEGRAM_BOT_TOKEN`(这曾让 README 的快速开始必然失败)。
- 货币护栏只能发现跨币种的串站。**BE/DE/ES/FR/IE/IT/NL 同为 EUR**,
  代理落到错误的欧元区国家时它发现不了,README 已如实说明。

## 开发

```bash
go test ./... && go vet ./... && gofmt -l .
./refurb-sentry -config configs/config.yaml -list-dims   # 查当前可用的过滤维度
./refurb-sentry -config configs/config.yaml -once -dry-run
```

新增地区:在 `internal/apple/regions.go` 的表里加一行即可,启动校验会验证可用性。
新货币记得同时在 `internal/apple/model.go` 的符号表里补一项,否则会退化成 "XXX 999" 的展示。
实测 MX、IN 没有翻新店(返回 404),不要加。

README 是中英两版(`README.md` 英文、`README.zh-CN.md` 中文),内容对等、顶部互链。
改动其中一版的事实性内容(参数、规则字段、地区/分类、行为约束)必须同步另一版——
只改一版会留下一份静默过期的文档,而这个项目的可信度正建立在"这些都是实测结论"上。
通知正文与日志目前是硬编码中文,英文 README 已如实声明这一点;若将来做输出本地化,记得同步删掉那段声明。
