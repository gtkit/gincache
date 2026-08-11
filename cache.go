// Package gincache 提供生产级的 Gin HTTP 响应缓存中间件
package gincache

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/json"
	"golang.org/x/sync/singleflight"
)

// =========================================================================
// 类型定义
// =========================================================================

// ResponseCache 缓存的响应数据.
type ResponseCache struct {
	Status int `json:"s"`
	// Header 保留旧的单值 header 视图，用于兼容历史数据和现有调用方。
	Header map[string]string `json:"h"`
	// Headers 保存完整的 header 集合，缓存命中时优先用它回放响应。
	Headers http.Header `json:"hv,omitempty"`
	Body    []byte      `json:"b"`

	tooLarge bool `json:"-"`
	// written 记录包装器是否观察到过写入。handler 走 Hijack 接管连接、或根本没
	// 产生响应时它为 false，此时 Status/Body 都是包装器编出来的默认值。
	written bool `json:"-"`
}

// Strategy 缓存策略.
type Strategy struct {
	CacheKey      string             // 自定义缓存 Key
	CacheStore    persist.CacheStore // 可选：使用不同的存储后端
	CacheDuration time.Duration      // 可选：覆盖默认 TTL
}

// GetCacheStrategyByRequest 根据请求动态决定缓存策略.
// 返回值：(是否缓存, 缓存策略)
type GetCacheStrategyByRequest func(c *gin.Context) (bool, Strategy)

// Callback 类型定义.
type (
	OnHitCacheCallback          func(c *gin.Context)
	OnMissCacheCallback         func(c *gin.Context)
	OnShareSingleFlightCallback func(c *gin.Context)
)

// Logger 日志接口.
type Logger interface {
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// =========================================================================
// 配置
// =========================================================================

// Config 缓存配置.
type Config struct {
	getCacheStrategy          GetCacheStrategyByRequest
	hitCacheCallback          OnHitCacheCallback
	missCacheCallback         OnMissCacheCallback
	shareSingleFlightCallback OnShareSingleFlightCallback
	singleFlightForgetTimeout time.Duration
	ignoreQueryOrder          bool
	logger                    Logger
	disableSingleFlight       bool         // 禁用 singleflight
	maxBodySize               int64        // 最大缓存 Body 大小，0 表示不限制
	cacheableStatusCodes      map[int]bool // 可缓存的状态码，nil 表示只缓存 2xx
	cacheableResponse         CacheableResponse
}

// Option 配置选项.
type Option func(*Config)

// CacheableResponse 判定一个**已经产生的响应**能否被共享——写进缓存，或从缓存回放。
//
// 为什么需要它：WithCacheStrategyByRequest 是请求期判据，那时响应头还不存在，
// 因此无法表达"这个响应不该被共享缓存"。而本包会把响应头连同 Body 一起缓存
// 并在命中时回放，于是以下三类响应一旦被共享就是数据泄漏或语义错误：
//
//	Set-Cookie —— 回放给后续所有请求，等于把第一个用户的会话发给别人；
//	Cache-Control: private / no-store / no-cache —— handler 明确声明不可共享缓存；
//	Vary —— 缓存键不含它列出的请求头，不同 Accept-* 会拿到同一份响应。
//
// 前两类由内置基线 DefaultCacheableResponse 默认挡住，第三类只挡 Vary: *——
// 它按 RFC 9111 §4.1 永远匹配失败，条目不可能被合法复用，拒绝零误伤。**具名
// Vary 不在基线内**：Vary: Origin 来自任何 CORS 中间件、Vary: Accept-Encoding
// 来自任何压缩中间件，默认拒绝会把绝大多数调用方的命中率打到 0；而它是否真的
// 出错取决于压缩中间件挂在本中间件的外侧还是内侧——本包看不到这个信息。
// 需要挡具名 Vary 的调用方自己写判据，或把对应请求头纳入缓存键。
//
// 判据在三个时机被调用，返回 false 一律表示"不共享"：
//
//	写入缓存前 —— 跳过缓存，本次响应照常发给客户端；
//	缓存命中回放前 —— 视为未命中，继续执行后续 handler；
//	singleflight 把 leader 的响应交给并发等待者前 —— 等待者改为执行自己的 handler。
//
// 因此判据应保持廉价且无副作用，尤其不要在里面计数或打点：同一次请求可能调用两次。
//
// **传入的是 header 的副本，且保证非 nil**：http.Header 是 map，直接把内部 header
// 交出去等于允许判据改写随后写入 store 或即将回放的内容——一个只该"判断"的回调
// 却能改数据，而调用方不会意识到自己有这个能力。非 nil 是另一重保证：完全没有
// 响应头的响应会让内部 header 为 nil，而判据往 nil map 上写一个字段就是 panic。
type CacheableResponse func(status int, header http.Header) bool

// DefaultCacheableResponse 是未设置 WithCacheableResponse 时生效的内置准入基线。
//
// 它拒绝这些响应：携带任意 Set-Cookie 的响应；Cache-Control 指令中出现
// no-store、private 或 no-cache 的响应；Vary 中出现 * 的响应。它们在 RFC 9111 中
// 对共享缓存是明确禁止或等价禁止的，且几乎不存在合法的共享缓存用途，误伤面接近零。
//
// Vary: * 按 RFC 9111 §4.1 永远匹配失败，条目不可能被合法复用。**具名 Vary 不在
// 拒绝清单内**，理由见 CacheableResponse 的说明。
//
// 判定大小写无关，也不会修改传入的 header——本函数是导出的组合原语，调用方
// 可能传进直接写 map 得到的非规范键，而那份 header 往往还要接着用。
//
// no-cache 的语义是"未经重新验证不得复用"而非"不得存储"。本包没有 revalidation
// 机制，存了就必然无验证复用——"允许存储"这一半没有任何可利用的余地，因此对
// 本包而言 no-cache 等价于不可缓存。
//
// 指令按逗号切分后逐个比对、大小写不敏感、不做子串匹配——否则 no-store-hint
// 之类的自定义指令会被误判。
//
// 设置 WithCacheableResponse 后本基线不再自动生效。需要"基线加上自己的条件"时，
// 在自定义判据里显式调用它：
//
//	gincache.WithCacheableResponse(func(status int, header http.Header) bool {
//		return gincache.DefaultCacheableResponse(status, header) && myCheck(status, header)
//	})
func DefaultCacheableResponse(_ int, header http.Header) bool {
	// 一趟扫描而不是逐个 header 做 map 查找：本函数要看三个头，而最常见的情况是
	// 三个都不存在——"先查规范键、查不到再全表扫描"反而让常见情况每次都扫全表。
	// 一趟扫描顺带对非规范键天然正确（本函数是导出的组合原语，调用方可能传进
	// 直接写 map 得到的键）。
	for key, values := range header {
		switch {
		case strings.EqualFold(key, "Set-Cookie"):
			if len(values) > 0 {
				return false
			}
		case strings.EqualFold(key, "Vary"):
			if hasVaryStar(values) {
				return false
			}
		case strings.EqualFold(key, "Cache-Control"):
			if parseCacheControl(values).blocked {
				return false
			}
		}
	}

	return true
}

// responseFreshness 解析响应自己声明的新鲜期。
//
// 优先级按 RFC 9111 §4.2.1：s-maxage（只对共享缓存生效）> max-age > Expires - Date。
// 存在 Expires 但无法解析时按已过期处理——RFC 9111 §5.3 要求把非法日期（尤其是
// 常见的 "Expires: 0"）当作过去的时间。
//
// 第二个返回值表示响应是否声明过新鲜期；未声明时调用方沿用配置的 TTL。
func responseFreshness(header http.Header) (time.Duration, bool) {
	var (
		control                     cacheControlView
		expiresRaw, dateRaw, ageRaw string
	)

	for key, values := range header {
		switch {
		case strings.EqualFold(key, "Cache-Control"):
			control = parseCacheControl(values)
		case strings.EqualFold(key, "Expires"):
			if len(values) > 0 {
				expiresRaw = values[0]
			}
		case strings.EqualFold(key, "Date"):
			if len(values) > 0 {
				dateRaw = values[0]
			}
		case strings.EqualFold(key, "Age"):
			if len(values) > 0 {
				ageRaw = values[0]
			}
		}
	}

	// Date 缺失时以"收到响应的时刻"为准，也就是现在（RFC 9111 §4.2.1）。
	//
	// 先判空再解析：http.ParseTime 会依次尝试三种格式，每次失败都分配一个
	// *time.ParseError——本地 handler 的响应通常没有 Date，不该在命中热路径上白付。
	now := time.Now()
	date := now
	if dateRaw != "" {
		if parsed, err := http.ParseTime(dateRaw); err == nil {
			date = parsed
		}
	}

	var lifetime time.Duration
	switch {
	case control.hasFreshness:
		lifetime = control.freshness
	case expiresRaw == "":
		return 0, false
	default:
		expiresAt, err := http.ParseTime(expiresRaw)
		if err != nil {
			return -1, true
		}
		lifetime = expiresAt.Sub(date)
	}

	// 扣掉响应已经消耗掉的新鲜期（RFC 9111 §4.2.3 的 current_age）。
	//
	// 本地 handler 产生的响应没有 Age、Date 就是当下，这一项为 0，行为不受影响；
	// 只有 handler 在做上游代理并透传了这两个头时才起作用——那时上游的
	// "max-age=60, Age: 60" 已经是陈旧响应，不扣就等于又给它续了 60 秒。
	currentAge := max(now.Sub(date), 0)
	// 同样先判空：strconv.ParseInt 对空串会分配一个 *strconv.NumError。
	if ageRaw != "" {
		if age, ok := parseDeltaSeconds(ageRaw); ok {
			currentAge = max(currentAge, age)
		}
	}

	return lifetime - currentAge, true
}

// maxDeltaSeconds 是 time.Duration 还能表示的最大秒数。
const maxDeltaSeconds = int64(math.MaxInt64) / int64(time.Second)

// parseDeltaSeconds 解析 delta-seconds 参数（Cache-Control 的 max-age 系列与 Age）。
//
// 第二个返回值为 false 表示值非法：非数字、负数，或者带单边引号——delta-seconds
// 是非负整数，这些都不该被当成有效的新鲜期。
//
// 超出可表示范围时钳到最大值而不是判为非法，这是 RFC 9111 §1.2.2 的要求；
// 反正最终 TTL 还要与配置值取小，钳到多大都不会真的放宽缓存。
func parseDeltaSeconds(param string) (time.Duration, bool) {
	value := unquote(param)

	seconds, err := strconv.ParseInt(value, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(value, "-"):
		return time.Duration(math.MaxInt64), true
	case err != nil, seconds < 0:
		return 0, false
	case seconds > maxDeltaSeconds:
		return time.Duration(math.MaxInt64), true
	}

	return time.Duration(seconds) * time.Second, true
}

// unquote 去掉成对的双引号。单边引号保持原样，好让它在后续解析中被判为非法，
// 而不是被 strings.Trim 悄悄抹平成一个合法数字。
func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		return value[1 : len(value)-1]
	}
	return value
}

// hasVaryStar 报告 Vary 是否包含 "*"。
func hasVaryStar(values []string) bool {
	for _, value := range values {
		for field := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(field) == "*" {
				return true
			}
		}
	}
	return false
}

// cacheControlView 是一次 Cache-Control 扫描能得到的全部判定结果。
//
// 合成一个按值返回的结构体，而不是返回 iter.Seq2 迭代器：迭代器要分配闭包，
// 而准入判定与新鲜期解析都在缓存命中的热路径上，两处各一次分配是白付的。
type cacheControlView struct {
	// blocked 表示出现了 no-store / private / no-cache 之一。
	blocked bool
	// freshness 是声明的新鲜期，hasFreshness 表示是否声明过。
	freshness    time.Duration
	hasFreshness bool
}

// parseCacheControl 扫描 Cache-Control 的全部值，一趟得出准入与新鲜期两项判定。
func parseCacheControl(values []string) cacheControlView {
	var (
		view                  cacheControlView
		sMaxAge, maxAge       time.Duration
		hasSMaxAge, hasMaxAge bool
	)

	for _, value := range values {
		for directive := range strings.SplitSeq(value, ",") {
			name, param, _ := strings.Cut(directive, "=")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-store", "private", "no-cache":
				view.blocked = true
			case "s-maxage":
				sMaxAge, hasSMaxAge = mergeFreshness(sMaxAge, hasSMaxAge, param)
			case "max-age":
				maxAge, hasMaxAge = mergeFreshness(maxAge, hasMaxAge, param)
			}
		}
	}

	// s-maxage 只对共享缓存生效，优先级高于 max-age。
	switch {
	case hasSMaxAge:
		view.freshness, view.hasFreshness = sMaxAge, true
	case hasMaxAge:
		view.freshness, view.hasFreshness = maxAge, true
	}

	return view
}

// mergeFreshness 把同一个新鲜期指令的又一次出现并入已有结果，取更保守的那个。
//
// RFC 9111 §4.2.1 对重复指令允许"取首个或按陈旧处理"。这里取最小值：它对两种
// 出现顺序给出同样保守的结果，而取首个会让 "max-age=600, max-age=0" 仍然缓存
// 600 秒——同一份响应只因指令先后不同就差出十分钟，不是共享缓存该有的行为。
//
// 值非法时按陈旧处理（RFC 9111 §1.2.2），而不是把整条指令当作没出现——后者
// 会退回配置的 TTL，等于让一个写错的 max-age 反而放宽了缓存。
func mergeFreshness(current time.Duration, has bool, param string) (time.Duration, bool) {
	parsed, ok := parseDeltaSeconds(param)
	if !ok {
		return 0, true
	}
	if !has {
		return parsed, true
	}
	return min(current, parsed), true
}

// WithCacheableResponse 设置响应期缓存判据，**替换**内置基线 DefaultCacheableResponse。
//
// 这里是替换而不是叠加：叠加会堵死一个合法用法——缓存键里已经带了用户维度时，
// 缓存 Cache-Control: private 的响应是正确的，而隐式叠加让调用方既做不到、
// 也看不出为什么被拒。需要保留基线的，在自己的判据里显式调用它。
func WithCacheableResponse(fn CacheableResponse) Option {
	return func(c *Config) { c.cacheableResponse = fn }
}

// WithCacheStrategyByRequest 设置自定义缓存策略.
func WithCacheStrategyByRequest(fn GetCacheStrategyByRequest) Option {
	return func(c *Config) { c.getCacheStrategy = fn }
}

// WithOnHitCache 设置缓存命中回调.
func WithOnHitCache(cb OnHitCacheCallback) Option {
	return func(c *Config) { c.hitCacheCallback = cb }
}

// WithOnMissCache 设置缓存未命中回调.
func WithOnMissCache(cb OnMissCacheCallback) Option {
	return func(c *Config) { c.missCacheCallback = cb }
}

// WithOnShareSingleFlight 设置 singleflight 共享结果回调.
func WithOnShareSingleFlight(cb OnShareSingleFlightCallback) Option {
	return func(c *Config) { c.shareSingleFlightCallback = cb }
}

// WithSingleFlightForgetTimeout 设置 singleflight 超时.
// 超时后会调用 Forget，防止长尾请求阻塞后续请求。
// 传入 0 或负数表示不设释放定时器。
func WithSingleFlightForgetTimeout(timeout time.Duration) Option {
	return func(c *Config) {
		c.singleFlightForgetTimeout = max(timeout, 0)
	}
}

// WithIgnoreQueryOrder 忽略 query 参数顺序.
func WithIgnoreQueryOrder() Option {
	return func(c *Config) { c.ignoreQueryOrder = true }
}

// WithLogger 设置日志.
func WithLogger(logger Logger) Option {
	return func(c *Config) { c.logger = logger }
}

// WithDisableSingleFlight 禁用 singleflight（某些场景可能需要）.
func WithDisableSingleFlight() Option {
	return func(c *Config) { c.disableSingleFlight = true }
}

// WithMaxBodySize 设置最大缓存 Body 大小.
// 传入 0 或负数表示不限制。
func WithMaxBodySize(size int64) Option {
	return func(c *Config) {
		c.maxBodySize = max(size, 0)
	}
}

// WithCacheableStatusCodes 设置可缓存的状态码.
func WithCacheableStatusCodes(codes ...int) Option {
	return func(c *Config) {
		c.cacheableStatusCodes = make(map[int]bool, len(codes))
		for _, code := range codes {
			c.cacheableStatusCodes[code] = true
		}
	}
}

// =========================================================================
// 缓存中间件
// =========================================================================

// Middleware 缓存中间件实例
type Middleware struct {
	store         persist.CacheStore
	defaultExpire time.Duration
	cfg           *Config
	sfGroup       singleflight.Group
}

// New 创建缓存中间件实例.
//
// 配置错误在这里就地 panic，而不是拖到第一个请求进来时才以 nil 解引用的形式
// 暴露——那时的报错信息与配置错误毫无关联。会 panic 的情况：store 为 nil，
// 或 defaultExpire 为负数。opts 中的 nil 元素被跳过。
func New(store persist.CacheStore, defaultExpire time.Duration, opts ...Option) *Middleware {
	if store == nil {
		panic("gincache: store must not be nil")
	}
	if defaultExpire < 0 {
		panic("gincache: defaultExpire must not be negative")
	}

	cfg := &Config{
		singleFlightForgetTimeout: 10 * time.Second, // 默认 10 秒
	}
	for _, opt := range opts {
		// 跳过而不是 panic：`var opt Option; if cond { opt = WithX() }`
		// 是常见的条件构造写法。
		if opt != nil {
			opt(cfg)
		}
	}

	return &Middleware{
		store:         store,
		defaultExpire: defaultExpire,
		cfg:           cfg,
	}
}

// Handler 返回 Gin 中间件处理函数.
func (m *Middleware) Handler() gin.HandlerFunc {
	if m.cfg.getCacheStrategy == nil {
		panic("gincache: WithCacheStrategyByRequest is required")
	}
	return m.handle
}

// CacheByURI 按 HTTP method 与完整 URI 缓存的中间件.
//
// 缓存键为 "<method> <uri>"，GET 与 HEAD 各用各的键，不同 method 的响应互不复用。
//
// 只处理 GET 与 HEAD，也只处理不带 Authorization 的请求，其余一律直接放行不缓存，
// 原因见 cacheableRequest。需要缓存非安全方法或带凭据的响应，改用 Cache 配合
// WithCacheStrategyByRequest，在 Strategy.CacheKey 里自行拼入所需维度。
//
// 键中**不含任何请求头**。响应内容随 Cookie、Accept-* 等请求头变化的接口
// （典型信号是响应带 Vary）同样必须走 WithCacheStrategyByRequest 自行拼键。
func (m *Middleware) CacheByURI() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !cacheableRequest(c.Request) {
			c.Next()
			return
		}

		uri := c.Request.RequestURI
		if m.cfg.ignoreQueryOrder {
			uri = normalizeURI(c.Request.URL.Path, c.Request.URL.RawQuery)
		}
		m.handleWithKey(c, requestKey(c.Request.Method, uri))
	}
}

// CacheByPath 按 HTTP method 与路径缓存的中间件.
//
// 缓存键为 "<method> <path>"，请求维度与请求头维度的约束与 CacheByURI 相同。
func (m *Middleware) CacheByPath() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !cacheableRequest(c.Request) {
			c.Next()
			return
		}

		m.handleWithKey(c, requestKey(c.Request.Method, c.Request.URL.Path))
	}
}

// cacheableRequest 报告内置中间件能否让这个请求参与缓存。
//
// 只放行 GET 与 HEAD：缓存一次 POST / PUT / DELETE 的响应并回放，等于让后续同键
// 请求跳过业务处理——副作用不会发生，调用方却收到成功响应。
//
// 带 Authorization 的请求整体绕过：内置键只由 method 与 URI 或 Path 构成，不含
// 任何请求头，无从区分不同凭据的用户；RFC 9111 §3.5 也规定共享缓存不得复用带
// Authorization 请求的响应。
//
// 两条限制都只作用于内置中间件。走 Cache + WithCacheStrategyByRequest 的调用方
// 已经显式表达了"缓存这个请求"，那是他们的判断，本包不代为否决。
func cacheableRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return !hasHeaderFold(r.Header, "Authorization")
}

// hasHeaderFold 大小写无关地报告 header 中是否存在某个字段。
//
// 真实网络流量经 net/http 解析后键必为规范形式，但程序化构造的请求（测试、
// 内部中间件、网关适配层）可能留下非规范键——只按规范键查找会让门禁被绕过。
func hasHeaderFold(header http.Header, name string) bool {
	for key, values := range header {
		if len(values) > 0 && strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// requestKey 把 method 拼进缓存键。不含 method 的键会让挂在 r.Any() 或混方法
// 路由组下的 POST 复用 GET 的响应。
//
// HEAD 不归一为 GET：归一只顾了读方向，写方向上 HEAD 首次未命中会把 HEAD 专属
// 响应（常见 handler 在 HEAD 分支不产出 Body）写进 GET 键，随后的 GET 命中一个
// 空条目；singleflight 下 HEAD 做 leader 时，同键的 GET 等待者也会拿到 HEAD 的
// 响应。分开键之后这类污染在结构上就不可能发生。
func requestKey(method, uri string) string {
	return method + " " + uri
}

// =========================================================================
// 快捷函数（向后兼容）
// =========================================================================

// Cache 通用缓存中间件.
func Cache(store persist.CacheStore, expire time.Duration, opts ...Option) gin.HandlerFunc {
	m := New(store, expire, opts...)
	return m.Handler()
}

// CacheByRequestURI 按 URI 缓存.
func CacheByRequestURI(store persist.CacheStore, expire time.Duration, opts ...Option) gin.HandlerFunc {
	m := New(store, expire, opts...)
	return m.CacheByURI()
}

// CacheByRequestPath 按路径缓存.
func CacheByRequestPath(store persist.CacheStore, expire time.Duration, opts ...Option) gin.HandlerFunc {
	m := New(store, expire, opts...)
	return m.CacheByPath()
}

// =========================================================================
// 核心处理逻辑
// =========================================================================

func (m *Middleware) handle(c *gin.Context) {
	shouldCache, strategy := m.cfg.getCacheStrategy(c)
	if !shouldCache {
		c.Next()
		return
	}

	cacheKey := strategy.CacheKey
	cacheStore := m.store
	cacheDuration := m.defaultExpire

	if strategy.CacheStore != nil {
		cacheStore = strategy.CacheStore
	}
	if strategy.CacheDuration > 0 {
		cacheDuration = strategy.CacheDuration
	}

	m.handleWithParams(c, cacheKey, cacheStore, cacheDuration)
}

func (m *Middleware) handleWithKey(c *gin.Context, cacheKey string) {
	m.handleWithParams(c, cacheKey, m.store, m.defaultExpire)
}

func (m *Middleware) handleWithParams(c *gin.Context, cacheKey string, store persist.CacheStore, duration time.Duration) {
	// 0. 范围请求整体绕过缓存。
	// 缓存键不含 Range / If-Range，两个方向都必然错：读会把完整响应当成范围响应
	// 发出，写会把某一段字节固化成该 key 的全部内容。把范围条件纳入键也不行——
	// 键空间会被客户端任意撑爆。
	if c.Request.Header.Get("Range") != "" {
		c.Next()
		return
	}

	// 1. 尝试从缓存读取
	var cached ResponseCache
	if err := store.Get(cacheKey, &cached); err == nil {
		// 条目可能是本次准入判据生效之前写进去的，回放前按当前判据复检；
		// 不通过就当未命中，让 handler 重新产生响应。
		if header, ok := m.replayable(&cached); ok {
			if m.cfg.hitCacheCallback != nil {
				m.cfg.hitCacheCallback(c)
			}
			m.writeResponse(c, &cached, header)
			c.Abort()
			return
		}
	}

	// 2. 缓存未命中
	if m.cfg.missCacheCallback != nil {
		m.cfg.missCacheCallback(c)
	}

	// 3. 是否使用 singleflight
	if m.cfg.disableSingleFlight {
		m.executeAndCache(c, cacheKey, store, duration)
		return
	}

	// 4. 使用 singleflight 防止缓存击穿
	m.executeWithSingleFlightSafe(c, cacheKey, store, duration)
}

func (m *Middleware) executeAndCache(c *gin.Context, cacheKey string, store persist.CacheStore, duration time.Duration) {
	resp := m.executeHandler(c)
	m.cacheResponse(cacheKey, resp, store, duration)
}

func (m *Middleware) executeWithSingleFlightSafe(c *gin.Context, cacheKey string, store persist.CacheStore, duration time.Duration) {
	var executedHandler atomic.Bool

	result, err, shared := m.sfGroup.Do(cacheKey, func() (any, error) {
		// 定时器建在 flight 内并在结束时停止，与 persist/twolevel.go 的写法一致。
		// 建在 Do 之外的话每个等待者也会建一个、而且没有一个会被停止：早已结束的
		// 请求，其定时器会在毫不相干的后续 flight 期间 Forget，同一个 key 于是
		// 出现多个 leader，防击穿失效；高 miss 流量下还会持续积累无效定时器。
		stopForget := m.scheduleForget(cacheKey)
		defer stopForget()

		var cached ResponseCache
		if err := store.Get(cacheKey, &cached); err == nil {
			// 这里也要复检：不合规的历史条目若被放出去，leader 和所有等待者会
			// 一起 fall through 去执行 handler，singleflight 就被击穿了。
			if _, ok := m.replayable(&cached); ok {
				return &cached, nil
			}
		}

		executedHandler.Store(true)
		resp := m.executeHandler(c)
		m.cacheResponse(cacheKey, resp, store, duration)

		return resp, nil
	})
	if err != nil {
		if m.cfg.logger != nil {
			m.cfg.logger.Errorf("gincache: singleflight error: %v", err)
		}
		c.Next()
		return
	}

	resp, ok := result.(*ResponseCache)
	if !ok {
		c.Next()
		return
	}

	if !executedHandler.Load() {
		// 走到这里说明响应不是本请求产生的：要么来自 store，要么是 leader 刚跑出来的。
		// 后一种绕过了 store，此前完全不受判据保护——leader 的 Set-Cookie 会被原样
		// 发给所有同 key 的并发等待者，和缓存泄漏是同一类会话串号。
		header, ok := m.replayable(resp)
		if !ok {
			c.Next()
			return
		}

		if shared && m.cfg.shareSingleFlightCallback != nil {
			m.cfg.shareSingleFlightCallback(c)
		}
		m.writeResponse(c, resp, header)
		c.Abort()
		return
	}

	if shared && m.cfg.shareSingleFlightCallback != nil {
		m.cfg.shareSingleFlightCallback(c)
	}
	c.Abort()
}

// scheduleForget 为一次 flight 安排释放定时器，返回的函数在 flight 结束时停止它。
func (m *Middleware) scheduleForget(cacheKey string) func() {
	if m.cfg.singleFlightForgetTimeout <= 0 {
		return func() {}
	}

	timer := time.AfterFunc(m.cfg.singleFlightForgetTimeout, func() {
		m.sfGroup.Forget(cacheKey)
	})
	return func() { timer.Stop() }
}

// =========================================================================
// ResponseWriter 包装
// =========================================================================

type cachedWriter struct {
	gin.ResponseWriter
	body        *bytes.Buffer
	statusCode  int
	written     bool
	maxBodySize int64
	overflowed  bool
}

var writerPool = sync.Pool{
	New: func() any {
		return &cachedWriter{
			body: bytes.NewBuffer(make([]byte, 0, 4096)),
		}
	},
}

func getWriter(w gin.ResponseWriter, maxBodySize int64) *cachedWriter {
	cw := writerPool.Get().(*cachedWriter)
	cw.ResponseWriter = w
	cw.body.Reset()
	cw.statusCode = 0
	cw.written = false
	cw.maxBodySize = maxBodySize
	cw.overflowed = false
	return cw
}

func putWriter(cw *cachedWriter) {
	cw.ResponseWriter = nil
	cw.body.Reset()
	cw.maxBodySize = 0
	cw.overflowed = false
	writerPool.Put(cw)
}

func (w *cachedWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cachedWriter) Write(data []byte) (int, error) {
	if !w.written {
		w.statusCode = http.StatusOK
		w.written = true
	}
	if w.shouldBuffer(len(data)) {
		if _, err := w.body.Write(data); err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *cachedWriter) WriteString(s string) (int, error) {
	if !w.written {
		w.statusCode = http.StatusOK
		w.written = true
	}
	if w.shouldBuffer(len(s)) {
		if _, err := w.body.WriteString(s); err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.WriteString(s)
}

func (w *cachedWriter) Status() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *cachedWriter) shouldBuffer(nextWriteSize int) bool {
	if w.overflowed {
		return false
	}
	if w.maxBodySize <= 0 {
		return true
	}
	if int64(w.body.Len()+nextWriteSize) <= w.maxBodySize {
		return true
	}

	// Stop buffering entirely once the response grows past the configured cache limit.
	w.body.Reset()
	w.overflowed = true
	return false
}

// =========================================================================
// 辅助方法
// =========================================================================

func (m *Middleware) executeHandler(c *gin.Context) *ResponseCache {
	cw := getWriter(c.Writer, m.cfg.maxBodySize)
	originalWriter := c.Writer
	c.Writer = cw

	// **恢复必须先于回收，且两者都必须在 defer 里。**
	//
	// 此前的写法是 `defer putWriter(cw)` + 函数末尾赋回 c.Writer：handler panic 时
	// 末尾那行永远执行不到，而 defer 已经把 cw 的内层 ResponseWriter 置为 nil
	// 并把它放回了池。于是 gin.Recovery 拿着仍指向 cw 的 c.Writer 写 500 ——
	// 二次 panic；更糟的是 cw 已回池，可能被另一个请求取走，两个请求共用一个
	// writer，响应互相串。
	//
	// defer 的执行顺序是后进先出，因此这里的单个 defer 内显式排序：
	// 先把 c.Writer 换回真实 writer（Recovery 从此刻起是安全的），再回收包装器。
	defer func() {
		c.Writer = originalWriter
		putWriter(cw)
	}()

	// 执行后续 Handler
	c.Next()

	headers := cloneCachedHeaders(cw.Header())

	// 复制 Body（重要：必须复制，因为 buffer 会被重用）
	body := make([]byte, cw.body.Len())
	copy(body, cw.body.Bytes())

	return &ResponseCache{
		Status:   cw.Status(),
		Header:   flattenLegacyHeaders(headers),
		Headers:  headers,
		Body:     body,
		tooLarge: cw.overflowed,
		written:  cw.written,
	}
}

func (m *Middleware) cacheResponse(key string, resp *ResponseCache, store persist.CacheStore, duration time.Duration) {
	// 包装器没观察到任何写入：handler 要么 Hijack 接管了连接（WebSocket 升级等），
	// 要么根本没产生响应。此时 Status 和 Body 都是包装器编出来的默认值，缓存它
	// 等于用一个空 200 顶掉后续所有请求。
	if !resp.written {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: handler wrote nothing through the wrapper (hijacked?), skip cache")
		}
		return
	}

	// 检查是否应该缓存
	if !m.shouldCache(resp) {
		return
	}

	if resp.tooLarge {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: body exceeded max cache size, skip cache")
		}
		return
	}

	// 检查 Body 大小限制
	if m.cfg.maxBodySize > 0 && int64(len(resp.Body)) > m.cfg.maxBodySize {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: body too large, skip cache: %d > %d", len(resp.Body), m.cfg.maxBodySize)
		}
		return
	}

	// 响应自己声明的新鲜期约束回放时长：声明已经过期就不该进缓存，声明比配置短
	// 就以声明为准。配置 TTL 因此是上限，而不是最终值。
	if declared, ok := responseFreshness(resp.Headers); ok {
		if declared <= 0 {
			if m.cfg.logger != nil {
				m.cfg.logger.Debugf("gincache: response declares no freshness lifetime, skip cache")
			}
			return
		}
		// duration 为 0 表示交由存储决定，此时直接用声明值——min(0, 声明) 会退化
		// 成 0 并被存储当成默认值，反而可能更长。
		if duration <= 0 || declared < duration {
			duration = declared
		}
	}

	// 写入缓存
	// 使用独立于请求的 context，避免客户端取消导致缓存回填中断。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := store.SetWithContext(ctx, key, resp, duration); err != nil {
		if m.cfg.logger != nil {
			m.cfg.logger.Errorf("gincache: failed to set cache: %v", err)
		}
	}
}

func (m *Middleware) shouldCache(resp *ResponseCache) bool {
	if !m.statusCacheable(resp.Status) {
		return false
	}

	// 响应期判据放在状态码之后：状态码不可缓存时无需再问判据，
	// 而判据要能看到完整响应头（Set-Cookie / Cache-Control / Vary）。
	if !m.responseCacheable(resp.Status, resp.Headers) {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: response rejected by CacheableResponse, skip cache")
		}
		return false
	}

	return true
}

// replayable 判定一个不是本请求产生的响应能否回放给当前请求，并返回清理后的
// header 视图。来源有两种：store 里的缓存条目（可能是旧判据下写入的），以及
// singleflight 中 leader 刚产生、准备交给等待者的响应。两者共用同一道判据——
// 能被共享写入的，才能被共享放出去。
func (m *Middleware) replayable(resp *ResponseCache) (http.Header, bool) {
	header := cachedHeaderView(resp)
	if !m.statusCacheable(resp.Status) || !m.responseCacheable(resp.Status, header) {
		return nil, false
	}

	// 升级前写入的条目可能声明了一出生就过期的新鲜期，回放前一并挡下。
	// 这里判定的是条目声明的新鲜期本身，条目是否已经到期由存储的 TTL 负责。
	if declared, ok := responseFreshness(header); ok && declared <= 0 {
		return nil, false
	}

	return header, true
}

// responseCacheable 用调用方判据或内置基线判定响应能否被共享。
func (m *Middleware) responseCacheable(status int, header http.Header) bool {
	if m.cfg.cacheableResponse == nil {
		return DefaultCacheableResponse(status, header)
	}
	// 只有交给调用方时才需要副本：判据只该判断，不该改写随后写入 store
	// 或即将回放的 header。内置基线是本包自己的只读实现，不必付这份拷贝。
	//
	// 副本还必须非 nil：完全没有响应头的响应（如只调 c.Status(204) 的 handler）
	// 会让 header 为 nil，而 http.Header.Clone() 对 nil 返回 nil——判据往 nil map
	// 上写一个字段就是 panic，且发生在 singleflight 内会连累所有等待者。
	// 判据本就不该改 header，但库不能因此把一条 panic 路径留在这里。
	cloned := header.Clone()
	if cloned == nil {
		cloned = make(http.Header)
	}
	return m.cfg.cacheableResponse(status, cloned)
}

func (m *Middleware) statusCacheable(status int) bool {
	if m.cfg.cacheableStatusCodes != nil {
		return m.cfg.cacheableStatusCodes[status]
	}
	// 默认缓存 2xx，但排除 206：缓存键不含 Range，回放会把某一段字节
	// 连同 Content-Range 当成完整响应发给所有人。
	return status >= 200 && status < 300 && status != http.StatusPartialContent
}

func (m *Middleware) writeResponse(c *gin.Context, resp *ResponseCache, header http.Header) {
	applyCachedHeaders(c.Writer.Header(), header)
	c.Header("X-Cache", "HIT")

	if notModified(c.Request, resp.Status, header) {
		writeNotModified(c)
		return
	}

	c.Status(resp.Status)
	if c.Request.Method == http.MethodHead || len(resp.Body) == 0 {
		c.Writer.WriteHeaderNow()
		return
	}
	_, _ = c.Writer.Write(resp.Body)
}

// notModified 报告缓存条目对本次条件请求可以答以 304。
//
// 只对 200 条目和 GET / HEAD 生效：其他状态码的 304 没有意义，其他方法的条件
// 请求语义是 412 而不是 304，本包不涉足。
func notModified(r *http.Request, status int, header http.Header) bool {
	if status != http.StatusOK {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	// If-None-Match 优先于 If-Modified-Since（RFC 9110 §13.2.2）：前者存在时，
	// 后者一律不再评估，哪怕前者不匹配。
	//
	// 用 Values 而不是 Get：多个字段行等价于逗号连接的一个列表，Get 只会给出
	// 第一行，后面几行里的 tag 就漏掉了。
	ifNoneMatch := r.Header.Values("If-None-Match")
	for _, line := range ifNoneMatch {
		if strings.TrimSpace(line) != "" {
			return etagMatches(ifNoneMatch, header.Get("ETag"))
		}
	}

	// 先判空再解析：http.ParseTime 会依次尝试三种格式，每次失败都分配一个
	// *time.ParseError——绝大多数请求不带这个头，不该在命中热路径上白付三次分配。
	ifModifiedSince := r.Header.Get("If-Modified-Since")
	if ifModifiedSince == "" {
		return false
	}

	since, err := http.ParseTime(ifModifiedSince)
	if err != nil {
		return false
	}
	modified, err := http.ParseTime(header.Get("Last-Modified"))
	if err != nil {
		return false
	}
	// HTTP 日期精度只到秒，不晚于即视为未变更。
	return !modified.Truncate(time.Second).After(since)
}

// etagMatches 按 RFC 9110 §13.1.2 的弱比较判断 If-None-Match 是否命中。
// 弱比较忽略 W/ 前缀；"*" 表示"只要存在表示就算命中"。
func etagMatches(ifNoneMatch []string, etag string) bool {
	want := strings.TrimPrefix(etag, "W/")

	for _, line := range ifNoneMatch {
		for rest := line; rest != ""; {
			var candidate string
			candidate, rest = nextEntityTag(rest)

			switch {
			case candidate == "":
				continue
			case candidate == "*":
				return true
			case etag != "" && strings.TrimPrefix(candidate, "W/") == want:
				return true
			}
		}
	}

	return false
}

// nextEntityTag 从 If-None-Match 列表里切出下一个 entity-tag 及剩余部分。
//
// 不能直接按逗号切分：合法的 opaque-tag 允许包含逗号（`"a,b"` 是一个 tag 而不是
// 两个），引号内的逗号不是分隔符。
func nextEntityTag(list string) (tag, rest string) {
	list = strings.TrimLeft(list, " \t,")
	if list == "" {
		return "", ""
	}

	inQuotes := false
	for i := range len(list) {
		switch list[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				return strings.TrimSpace(list[:i]), list[i+1:]
			}
		}
	}

	return strings.TrimSpace(list), ""
}

// writeNotModified 写出 304 并剔除与实体相关的 header，与 net/http 自己的
// writeNotModified 保持一致。
func writeNotModified(c *gin.Context) {
	header := c.Writer.Header()
	header.Del("Content-Type")
	header.Del("Content-Length")
	header.Del("Content-Encoding")
	if header.Get("ETag") != "" {
		header.Del("Last-Modified")
	}

	c.Status(http.StatusNotModified)
	c.Writer.WriteHeaderNow()
}

func cloneCachedHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}

	cloned := src.Clone()
	// 规范化必须排在两个删除之前：它们都按规范键查找，非规范键会直接漏过去。
	canonicalizeHeaderKeys(cloned)
	delete(cloned, "X-Cache")
	deleteHopByHopHeaders(cloned)
	return cloned
}

// canonicalizeHeaderKeys 把非规范的 header 键就地并到规范键上。
//
// http.Header 底层是 map：Header.Set 会规范化键，而直接写 map 不会。准入基线按
// 规范键查找、逐跳过滤按规范键索引 map，于是 handler 写一句
// c.Writer.Header()["set-cookie"] = ... 就能让 Set-Cookie 完整地进缓存再发出去。
// 从外部存储反序列化出的条目同理。
func canonicalizeHeaderKeys(header http.Header) {
	for key, values := range header {
		canonical := http.CanonicalHeaderKey(key)
		if canonical == key {
			continue
		}

		delete(header, key)
		// Clip 掉多余容量，避免 append 写进与其他键共享的底层数组。
		header[canonical] = append(slices.Clip(header[canonical]), values...)
	}
}

// cachedHeaderView 返回缓存条目的 header 视图：优先用完整集合，只有旧式单值
// 视图的历史条目由它就地构造，并统一剔除不该跨连接回放的 header——历史条目
// 是在写入侧过滤存在之前写进去的，只能在回放侧补掉。
func cachedHeaderView(resp *ResponseCache) http.Header {
	if len(resp.Headers) > 0 {
		// 命中回放是热路径，而本包写入的条目在写入侧已经规范化并剔除过逐跳 header，
		// 回放时必然走这条快路径。返回原表是安全的：两个调用方（回放写出、判据）
		// 都只读，交给调用方判据的还是另一份副本。
		if !needsHeaderCleanup(resp.Headers) {
			return resp.Headers
		}

		cloned := resp.Headers.Clone()
		canonicalizeHeaderKeys(cloned)
		deleteHopByHopHeaders(cloned)
		return cloned
	}

	if len(resp.Header) == 0 {
		return nil
	}

	header := make(http.Header, len(resp.Header))
	for key, value := range resp.Header {
		header.Set(key, value)
	}
	deleteHopByHopHeaders(header)
	return header
}

// hopByHopHeaders 是 RFC 9110 §7.6.1 定义的连接级 header，只对单个连接有意义，
// 跨连接回放会破坏协议语义。
//
// 这里存的是 http.Header 的规范形式（"Te" 而非 RFC 写法 "TE"），这样可以直接
// 按 map 键存取，不必每次调用都走一遍 CanonicalMIMEHeaderKey——"TE" 不是规范形式，
// canonical 化会在热路径上多分配一个字符串。
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	"Proxy-Authenticate",
	"Proxy-Authorization",
}

// needsHeaderCleanup 报告 header 是否需要规范化键或剔除逐跳字段。
// 两项都是无分配的扫描，用来在命中回放的热路径上判断能否跳过整表克隆。
func needsHeaderCleanup(header http.Header) bool {
	for key := range header {
		if http.CanonicalHeaderKey(key) != key {
			return true
		}
	}

	// 键已全部规范，Connection 缺席就意味着没有额外的逐跳字段名可派生，
	// 只查固定清单即可。
	for _, name := range hopByHopHeaders {
		if _, ok := header[name]; ok {
			return true
		}
	}
	return false
}

// deleteHopByHopHeaders 就地剔除连接级 header。必须先读 Connection 的值再删固定
// 清单——RFC 规定 Connection 头里列出的字段名同样是逐跳的，而 Connection 自己
// 就在待删清单里。
func deleteHopByHopHeaders(header http.Header) {
	for _, value := range header["Connection"] {
		for name := range strings.SplitSeq(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				// 这些名字来自响应方写下的任意字符串，必须走规范化。
				header.Del(name)
			}
		}
	}

	for _, name := range hopByHopHeaders {
		delete(header, name)
	}
}

func flattenLegacyHeaders(src http.Header) map[string]string {
	if len(src) == 0 {
		return nil
	}

	flat := make(map[string]string, len(src))
	for key, values := range src {
		if len(values) == 0 {
			flat[key] = ""
			continue
		}
		flat[key] = values[0]
	}
	return flat
}

func applyCachedHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

// =========================================================================
// 工具函数
// =========================================================================

// normalizeURI 规范化 URI（排序 query 参数）
func normalizeURI(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	params := strings.Split(rawQuery, "&")
	slices.Sort(params)
	return path + "?" + strings.Join(params, "&")
}

// =========================================================================
// 序列化
// =========================================================================

// Serialize 序列化响应缓存，供自定义存储或跨进程传输场景复用。
func Serialize(resp *ResponseCache) ([]byte, error) {
	return json.Marshal(resp)
}

// Deserialize 反序列化响应缓存，供自定义存储或跨进程传输场景复用。
func Deserialize(data []byte) (*ResponseCache, error) {
	var resp ResponseCache
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

var ErrCacheMiss = persist.ErrCacheMiss
