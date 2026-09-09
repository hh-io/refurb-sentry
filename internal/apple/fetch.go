package apple

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// ErrCategoryNotAvailable 表示该地区不提供此分类(实测 CN/HK 的 iphone、appletv 返回 404)。
// 属配置问题而非运行故障,由启动校验一次性暴露。
var ErrCategoryNotAvailable = errors.New("该地区不提供此分类")

// defaultUserAgent 固定为一个真实浏览器标识,且刻意不做轮换——
// 稳定的 UA 比随机变化的 UA 更不像自动化流量。
const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

type ClientOptions struct {
	// Proxy 支持 http://、https://、socks5://。用途是修正出口地区,而非隐藏身份。
	Proxy      string
	UserAgent  string
	Timeout    time.Duration
	MaxRetries int
	Logger     *slog.Logger
}

type Client struct {
	hc         *http.Client
	ua         string
	maxRetries int
	log        *slog.Logger
	rnd        *rand.Rand
}

func NewClient(opt ClientOptions) (*Client, error) {
	if opt.Timeout <= 0 {
		opt.Timeout = 20 * time.Second
	}
	if opt.MaxRetries <= 0 {
		opt.MaxRetries = 3
	}
	if opt.UserAgent == "" {
		opt.UserAgent = defaultUserAgent
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}

	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		// 列表页每 2 分钟才抓一轮,空闲连接留久一点以复用 TLS 握手结果。
		IdleConnTimeout:       5 * time.Minute,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	if opt.Proxy != "" {
		u, err := url.Parse(opt.Proxy)
		if err != nil {
			return nil, fmt.Errorf("解析代理地址 %q: %w", opt.Proxy, err)
		}
		switch u.Scheme {
		case "socks5", "socks5h":
			d, err := proxy.FromURL(u, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("初始化 SOCKS5 代理: %w", err)
			}
			cd, ok := d.(proxy.ContextDialer)
			if !ok {
				return nil, fmt.Errorf("SOCKS5 代理不支持带 context 的拨号")
			}
			tr.Proxy = nil
			tr.DialContext = cd.DialContext
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		default:
			return nil, fmt.Errorf("不支持的代理协议 %q,仅支持 http/https/socks5", u.Scheme)
		}
	}

	return &Client{
		hc:         &http.Client{Transport: tr, Timeout: opt.Timeout},
		ua:         opt.UserAgent,
		maxRetries: opt.MaxRetries,
		log:        opt.Logger,
		rnd:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// FetchGrid 抓取并解析某地区某分类的翻新列表页。
func (c *Client) FetchGrid(ctx context.Context, region Region, category string) (*Grid, error) {
	body, err := c.get(ctx, region, region.GridURL(category))
	if err != nil {
		return nil, err
	}
	g, err := ParseGrid(body, region, category)
	if err != nil {
		return nil, err
	}
	// 货币不符说明请求被重定向到了别的地区站(常见于代理出口地区与目标不匹配)。
	// 此时数据是「另一个地区的」,若继续参与 diff 会造成大面积误报。
	if len(g.Products) > 0 && g.Products[0].Currency != region.Currency {
		return nil, fmt.Errorf("地区 %s 期望货币 %s,实际拿到 %s:请求可能被重定向到其他地区站点",
			region.Code, region.Currency, g.Products[0].Currency)
	}
	return g, nil
}

func (c *Client) get(ctx context.Context, region Region, u string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			d := c.backoff(attempt, lastErr)
			c.log.Warn("请求失败,退避后重试",
				"url", u, "attempt", attempt, "backoff", d.String(), "err", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
		}

		body, err := c.do(ctx, region, u)
		if err == nil {
			return body, nil
		}
		// 该地区没有这个分类,重试多少次都是 404。
		if errors.Is(err, ErrCategoryNotAvailable) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var he *httpError
		if errors.As(err, &he) && !he.retryable() {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", c.maxRetries, lastErr)
}

func (c *Client) do(ctx context.Context, region Region, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", region.AcceptLanguage)
	// 不手动设置 Accept-Encoding:交给 net/http 自动协商并透明解压 gzip。

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%s: %w", u, ErrCategoryNotAvailable)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &httpError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), URL: u}
	}

	// 列表页实测 0.1~1.3 MB,留足上限同时防止异常响应吃光内存。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应体 %s: %w", u, err)
	}
	return body, nil
}

// backoff 计算退避时长:指数增长 + 抖动,上限 60s;若服务端给了 Retry-After 则优先遵从。
func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	var he *httpError
	if errors.As(lastErr, &he) && he.RetryAfter > 0 {
		return min(he.RetryAfter, 5*time.Minute)
	}
	d := time.Duration(1<<uint(attempt)) * time.Second
	d = min(d, 60*time.Second)
	return d + time.Duration(c.rnd.Int63n(int64(time.Second)))
}

type httpError struct {
	Status     int
	RetryAfter time.Duration
	URL        string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("请求 %s 返回 HTTP %d", e.URL, e.Status)
}

// retryable:5xx 与 429 属临时故障值得重试;其余 4xx 重试无意义。
func (e *httpError) retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Jitter 返回 [base, base+spread) 内的随机时长,用于打散同一轮内各请求的发出时刻。
func (c *Client) Jitter(base, spread time.Duration) time.Duration {
	if spread <= 0 {
		return base
	}
	return base + time.Duration(c.rnd.Int63n(int64(spread)))
}
