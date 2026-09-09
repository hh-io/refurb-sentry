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
- **芯片型号与 CPU/GPU 核心数不在 dimensions 里,只在 `title` 字符串**,且各地区文案完全不同
  (CN「芯片/核中央处理器」、HK「晶片/核心 CPU」、JP「チップ/コアCPU」、DE 用 U+2011 连字符)。
  这是全项目唯一的脆弱解析点,见 `internal/filter/chip.go`。
- 数据源**没有库存数量**。事件只能是上架/降价/下架。
- 地区×分类矩阵是稀疏的:CN/HK 的 iphone、appletv 返回 **404**;
  US 的 appletv/airpods/homepod 返回 200 但无 bootstrap。这三种情况必须分开处理。

## 三条不能破坏的正确性约束

1. **冷启动必须静默**。首次运行数百件在架商品全是"新增",直接推送会刷屏。
   由 `State.Bootstrapped` 控制,见 `internal/state/state.go`。
2. **抓取失败绝不能触发下架**。任何 error / `ErrNoBootstrap` 都必须跳过该分类而不调用 `State.Apply`。
   连续空结果要攒够 `emptyStreakThreshold` 轮才认定真空。
3. **首轮遇到不可用分类必须在写入任何状态前退出**。`RunOnce` 为此刻意分成
   「先抓全、再统一比对」两阶段,否则会留下只覆盖部分范围的残缺基线。

## 设计取舍

- 规则过滤发生在 **diff 之后、推送之前**。状态库始终记录全部商品,
  这样以后放宽规则时,早已在架的商品不会被误报成新上架。
- 依赖只有 `gopkg.in/yaml.v3` 和 `golang.org/x/net`(SOCKS5),其余全标准库。
  加新依赖前先确认标准库真的做不到。
- 配置在 **YAML 节点层**展开环境变量,不是文本替换——否则注释里的 `${VAR}` 会被误当引用。

## 开发

```bash
go test ./... && go vet ./... && gofmt -l .
./refurb-sentry -config configs/config.yaml -list-dims   # 查当前可用的过滤维度
./refurb-sentry -config configs/config.yaml -once -dry-run
```

新增地区:在 `internal/apple/regions.go` 的表里加一行即可,启动校验会验证可用性。
