// Package gincache 提供生产级的 Gin HTTP 响应缓存中间件
package gincache

import (
	"bytes"
	"context"
	"errors"
	"hash/maphash"
	"math"
	"net/http"
	"reflect"
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
	// ResponseTime 是本包收到这份响应的 Unix 纳秒，即 RFC 9111 §4.2.3 的
	// response_time。回放时用它加上条目里的 Age 得出当前年龄——RFC 9111 §5.1
	// 要求缓存在复用响应时给出当前年龄，否则下游缓存会低估年龄而超期复用；
	// 新鲜度判定也要用它，否则存储 TTL 比声明的新鲜期长时条目会活过自己的新鲜期。
	// 本包此前写入的条目没有这个字段，其零值表示"无法估算"，那类条目不再回放。
	ResponseTime int64 `json:"t,omitempty"`
	// InitialAge 是这份响应到达本包时已有的年龄（纳秒），即 RFC 9111 §4.2.3 的
	// corrected_initial_age。回放时的当前年龄就是它加上驻留时长。
	//
	// 两个字段都用纳秒而不是秒：截成整秒会让 InitialAge 偏小最多一秒，
	// 短新鲜期的条目因此能多活将近一个 max-age。
	//
	// 用数值字段而不是把它写回条目的 Age 头：多一个 header 意味着每次命中的
	// 反序列化都要多分配一个 map 条目、一个切片和一个字符串，实测每次命中多
	// 6 次分配；数值字段几乎不要钱。
	InitialAge int64 `json:"a,omitempty"`

	tooLarge bool `json:"-"`
	// responseDelay 是 handler 的全程耗时，用作 RFC 9111 §4.2.3 的 response_delay。
	// 只在写入判定时有意义，不随条目持久化。
	responseDelay time.Duration `json:"-"`
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

// responseFreshness 返回响应写入缓存那一刻的剩余新鲜期，即声明的新鲜期减去
// 它到达本包时已有的年龄。回放侧不用它——那时的年龄要算上驻留时间，见 replayable。
//
// 第二个返回值表示响应是否声明过新鲜期；未声明时调用方沿用配置的 TTL。
func responseFreshness(header http.Header, responseDelay time.Duration) (time.Duration, bool) {
	lifetime, declared := declaredLifetime(header, time.Now())
	if !declared {
		return 0, false
	}

	age := headerAge(header, responseDelay)

	// 先比较再相减。两者都可能是 Time.Sub 的饱和值（超过约 292 年就会顶到
	// MinInt64 / MaxInt64），直接相减会整数回绕：MinInt64 - MaxInt64 恰好绕成
	// +1ns，几百年前就过期的响应反而被判成"还新鲜 1 纳秒"。
	if lifetime <= age {
		return 0, true
	}

	return lifetime - age, true
}

// declaredLifetime 返回响应声明的新鲜期本身，不扣任何年龄。
//
// 优先级按 RFC 9111 §4.2.1：s-maxage（只对共享缓存生效）> max-age > Expires - Date。
// 存在 Expires 但无法解析时按已过期处理——RFC 9111 §5.3 要求把非法日期（尤其是
// 常见的 "Expires: 0"）当作过去的时间。
//
// 第二个返回值表示响应是否声明过新鲜期；未声明时调用方沿用配置的 TTL。
func declaredLifetime(header http.Header, receivedAt time.Time) (time.Duration, bool) {
	controlValues, expiresValues, dateValues, _ := freshnessFields(header)

	control := parseCacheControl(controlValues)
	switch {
	case control.hasFreshness:
		return control.freshness, true
	// Expires 是 singleton 字段，出现多个值属于畸形，按已过期处理。
	case len(expiresValues) > 1:
		return 0, true
	case len(expiresValues) == 0:
		return 0, false
	}

	expiresAt, err := http.ParseTime(expiresValues[0])
	if err != nil {
		return 0, true
	}

	// Date 缺失时以"收到这份响应的时刻"为准（RFC 9111 §4.2.1）。回放侧必须传
	// 条目自己的 response_time，不能拿当下顶替——那样算出来的已经是剩余时间，
	// 再和含驻留时长的年龄一比，驻留时间就被扣了两遍，条目在半途就失效。
	date, ok := headerDate(dateValues)
	if !ok {
		date = receivedAt
	}
	return expiresAt.Sub(date), true
}

// headerAge 计算响应到达本包时已有的年龄，即 RFC 9111 §4.2.3 的
// corrected_initial_age：apparent_age 与 age_value + response_delay 取大者。
//
// 本地 handler 产生的响应没有 Age、Date 就是当下，这一项等于 handler 的耗时；
// handler 在做上游代理时，上游的 "Age: 60" 与这段耗时都会算进来。
func headerAge(header http.Header, responseDelay time.Duration) time.Duration {
	_, _, dateValues, ageValues := freshnessFields(header)

	var apparent time.Duration
	if date, ok := headerDate(dateValues); ok {
		apparent = max(time.Since(date), 0)
	}

	// response_delay 与 Age 是否存在无关：没有 Age 时 age_value 视作 0，
	// 慢响应本身消耗掉的时间照样要算进年龄。
	corrected := responseDelay
	if age, ok := maxAgeValue(ageValues); ok {
		corrected = saturatingAdd(age, responseDelay)
	}

	return max(apparent, corrected)
}

// freshnessFields 一趟扫出四个与新鲜度相关的字段。
//
// 合并收集而不是逐个赋值：大小写不同的同名键在 HTTP 语义上是同一个字段，
// 赋值会让后遍历到的那个覆盖前一个，而 Go 的 map 遍历顺序是随机的。
func freshnessFields(header http.Header) (control, expires, date, age []string) {
	for key, values := range header {
		switch {
		case strings.EqualFold(key, "Cache-Control"):
			control = mergeValues(control, values)
		case strings.EqualFold(key, "Expires"):
			expires = mergeValues(expires, values)
		case strings.EqualFold(key, "Date"):
			date = mergeValues(date, values)
		case strings.EqualFold(key, "Age"):
			age = mergeValues(age, values)
		}
	}
	return control, expires, date, age
}

// headerDate 解析 Date。第二个返回值表示是否真的解析到——缺失或畸形时按
// RFC 9111 §4.2.1 以"收到响应的时刻"为准，而那一刻的 apparent_age 恰好是 0，
// 由调用方直接取 0，不要再拿两次 time.Now() 相减凑出一个几百纳秒的假年龄。
//
// 先判空再解析：http.ParseTime 会依次尝试三种格式，每次失败都分配一个
// *time.ParseError——本地 handler 的响应通常没有 Date，不该在热路径上白付。
func headerDate(dateValues []string) (time.Time, bool) {
	if raw := singletonValue(dateValues); raw != "" {
		if parsed, err := http.ParseTime(raw); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// parseAgeValue 解析单个 Age 值。Age 是 singleton，出现列表形式时按 RFC 9110 §5.5
// 取第一个成员——整体判为非法会把年龄算小，方向是 fail-open。
func parseAgeValue(raw string) (time.Duration, bool) {
	first, _, _ := strings.Cut(raw, ",")
	return parseDeltaSeconds(first)
}

// maxAgeValue 返回所有 Age 值中最大的一个。
//
// Age 是 singleton 字段，出现多个字段行属于畸形。RFC 9110 §5.5 允许取首个成员，
// 但合并大小写变体之后"首个"取决于 map 的遍历顺序；取最大值既确定又保守——
// 年龄算大即新鲜期算短。整个忽略则会把年龄算成 0，方向是 fail-open。
func maxAgeValue(values []string) (time.Duration, bool) {
	var (
		largest time.Duration
		found   bool
	)
	for _, value := range values {
		if age, ok := parseAgeValue(value); ok {
			largest, found = max(largest, age), true
		}
	}
	return largest, found
}

// mergeValues 把同一字段的又一批值并进已有结果。
//
// 首批直接复用原切片：绝大多数 header 只有一个键，这样最常见的情形不必付一次
// 分配。后续批次先 Clip 再 append，保证不写进 header 自己的底层数组。
// 返回的切片可能与 header 共享底层数组，调用方只读。
func mergeValues(current, values []string) []string {
	if current == nil {
		return values
	}
	return append(slices.Clip(current), values...)
}

// singletonValue 返回 singleton 字段的唯一值；出现零个或多个时返回空串。
// 多值属于畸形，取其中任何一个都会让结果依赖 map 的遍历顺序。
func singletonValue(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return values[0]
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

	// delta-seconds 的语法是 1*DIGIT。先自己卡一道纯数字：strconv 会接受 "+600"
	// 和 "-1"，两者按 RFC 都不合法。卡完之后 ParseInt 只可能因超范围而失败。
	if !isDigits(value) {
		return 0, false
	}

	seconds, err := strconv.ParseInt(value, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange), seconds > maxDeltaSeconds:
		return time.Duration(math.MaxInt64), true
	case err != nil:
		return 0, false
	}

	return time.Duration(seconds) * time.Second, true
}

// isDigits 报告 s 是否为非空的纯数字串。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
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
	// noStore 单独记录 no-store。响应侧它只是 blocked 的一员，请求侧却有独立
	// 语义——RFC 9111 §5.2.1.5 要求带它的请求所对应的响应不得被存储。
	noStore bool
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
			case "no-store":
				view.blocked, view.noStore = true, true
			case "private", "no-cache":
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
// RFC 9111 §4.2.1 对重复指令允许"取首个或按陈旧处理"。取值冲突的重复按陈旧
// 处理——那是两者中更保守的一个，而取首个会让 "max-age=600, max-age=0" 仍然
// 缓存 600 秒，同一份响应只因指令先后不同就差出十分钟。
//
// 取值相同的重复不算冲突，取该值即可：RFC 针对的是相互矛盾的重复，把
// "max-age=60, max-age=60" 也判成陈旧过苛。
//
// 值非法时同样按陈旧处理（RFC 9111 §1.2.2），而不是把整条指令当作没出现——
// 后者会退回配置的 TTL，等于让一个写错的 max-age 反而放宽了缓存。
func mergeFreshness(current time.Duration, has bool, param string) (time.Duration, bool) {
	parsed, ok := parseDeltaSeconds(param)
	switch {
	case !ok:
		return 0, true
	case !has:
		return parsed, true
	case parsed != current:
		return 0, true
	}

	return current, true
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
	// flightSeq 给拿不到实例身份的存储发放独占的 flight 编号，见 flightKey。
	flightSeq atomic.Uint64
	// writeGuards 按 flight key 分片，序列化同一 flight 的缓存写入，见 cacheResponseFenced。
	writeGuards [cacheWriteShards]sync.Mutex
}

// cacheWriteShards 是缓存写入互斥的分片数。定长数组，内存固定，不随 key 数量增长。
const cacheWriteShards = 256

// cacheWriteShardSeed 只用于算分片下标，与安全无关。
var cacheWriteShardSeed = maphash.MakeSeed()

// writeGuard 返回 flight 所属分片的写入互斥。
//
// 分片键用 flight key 而不是 CacheKey：flight key 已经把存储身份并进去了（见
// flightKey），按 CacheKey 分片会让不同存储的同名 key 互相争用。
func (m *Middleware) writeGuard(flight string) *sync.Mutex {
	return &m.writeGuards[maphash.String(cacheWriteShardSeed, flight)%cacheWriteShards]
}

// New 创建缓存中间件实例.
//
// defaultExpire 是**缓存条目的 TTL 上限**：响应自己声明了更短的新鲜期
// （Cache-Control 的 s-maxage / max-age，或 Expires）时以声明为准。
//
// defaultExpire 传 0 表示"交由存储决定"，此时中间件不再约束 TTL，只保留
// "响应已经陈旧就不缓存"这一道闸门——想让响应声明的新鲜期真正生效，
// 必须给出一个显式的上限。
//
// 配置错误在这里就地 panic，而不是拖到第一个请求进来时才以 nil 解引用的形式
// 暴露——那时的报错信息与配置错误毫无关联。会 panic 的情况：store 为 nil，
// 或 defaultExpire 为负数。opts 中的 nil 元素被跳过。
func New(store persist.CacheStore, defaultExpire time.Duration, opts ...Option) *Middleware {
	if isNilStore(store) {
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
		// typed-nil 是调用方的 bug。这里不能悄悄退回默认存储——按存储做租户
		// 隔离时，那等于把这次请求写进别人的库。宁可不缓存。
		if isNilStore(strategy.CacheStore) {
			if m.cfg.logger != nil {
				m.cfg.logger.Errorf("gincache: Strategy.CacheStore is a typed-nil, skip cache")
			}
			c.Next()
			return
		}
		cacheStore = strategy.CacheStore
	}
	if strategy.CacheDuration > 0 {
		cacheDuration = strategy.CacheDuration
	}

	m.handleWithParams(c, cacheRequest{
		key:      cacheKey,
		store:    cacheStore,
		custom:   strategy.CacheStore != nil,
		duration: cacheDuration,
	})
}

// cacheRequest 是一次请求走缓存路径时要带的全部上下文。
//
// 这几个值以前是逐个往下传的位置参数，随着规则增加已经到了看不清调用点的程度；
// 收成一个结构体之后，新增一项不再牵动每个签名。
type cacheRequest struct {
	key   string
	store persist.CacheStore
	// custom 表示 store 来自 Strategy.CacheStore 而不是默认存储。
	// 由调用点直接告知，不靠比较存储实例——值类型存储既比较不了也取不到地址。
	custom   bool
	duration time.Duration
	// noStore 表示请求声明了 Cache-Control: no-store。
	noStore bool
}

func (m *Middleware) handleWithKey(c *gin.Context, cacheKey string) {
	m.handleWithParams(c, cacheRequest{key: cacheKey, store: m.store, duration: m.defaultExpire})
}

func (m *Middleware) handleWithParams(c *gin.Context, req cacheRequest) {
	// 0. 范围请求整体绕过缓存。
	// 缓存键不含 Range / If-Range，两个方向都必然错：读会把完整响应当成范围响应
	// 发出，写会把某一段字节固化成该 key 的全部内容。把范围条件纳入键也不行——
	// 键空间会被客户端任意撑爆。
	if hasHeaderFold(c.Request.Header, "Range") {
		c.Next()
		return
	}

	// 1. 尝试从缓存读取
	var cached ResponseCache
	if err := req.store.Get(req.key, &cached); err == nil {
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

	// 3. 请求带 Cache-Control: no-store 时不得存储本次响应（RFC 9111 §5.2.1.5）。
	//
	// 与请求端的 no-cache 不同层级：no-cache 管的是"能不能复用"，遵守它等于放开
	// 一个人人可用的缓存击穿入口；no-store 只管"能不能写"，已有条目照常命中，
	// 攻击者拿 no-store 刷也只是让自己的响应不入库，制造不出别人的 miss。
	req.noStore = requestNoStore(c.Request)

	// 4. 是否使用 singleflight
	if m.cfg.disableSingleFlight {
		m.executeAndCache(c, req)
		return
	}

	// 5. 使用 singleflight 防止缓存击穿
	m.executeWithSingleFlightSafe(c, req)
}

// requestNoStore 报告请求的 Cache-Control 中是否出现 no-store 指令。
func requestNoStore(r *http.Request) bool {
	for key, values := range r.Header {
		if !strings.EqualFold(key, "Cache-Control") {
			continue
		}
		if parseCacheControl(values).noStore {
			return true
		}
	}
	return false
}

func (m *Middleware) executeAndCache(c *gin.Context, req cacheRequest) {
	resp := m.executeHandler(c)
	if !req.noStore {
		m.cacheResponse(req.key, resp, req.store, req.duration)
	}
}

// flightKey 把存储身份并进 singleflight 的合并身份。
//
// Strategy.CacheStore 允许逐请求换存储（典型用法是按租户分库），而 flight 只按
// CacheKey 合并的话，两个键相同但存储不同的并发请求会共享 leader 的响应——
// 按存储做的隔离就被从背后打穿了。
//
// 三类身份各带一个前缀，跨类不可能相撞：调用方能自由决定 CacheKey，不加前缀的话
// 一个精心构造的键就能伪装成另一类的 flight 身份（实测可复现跨存储串用）。
//
// "是不是默认存储"由调用点告知而不是比较实例：值类型存储既比较不了（不可比较的
// 动态类型会 panic）也取不到地址，靠地址判断会让值类型的默认存储彻底失去合并。
func (m *Middleware) flightKey(req cacheRequest) string {
	if !req.custom {
		return "d\x00" + req.key
	}

	if id := storeIdentity(req.store); id != 0 {
		return "p" + strconv.FormatUint(uint64(id), 36) + "\x00" + req.key
	}

	// 以值类型实现接口的自定义存储拿不到稳定的实例身份，无从判断两个请求用的是
	// 不是同一个。宁可不合并也不能合错：给一个本次请求独占的 flight 身份。
	return "u" + strconv.FormatUint(m.flightSeq.Add(1), 36) + "\x00" + req.key
}

// storeIdentity 返回存储实例的稳定身份；指针一类之外的实现拿不到，返回 0。
func storeIdentity(store persist.CacheStore) uintptr {
	switch value := reflect.ValueOf(store); value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.UnsafePointer:
		return value.Pointer()
	default:
		return 0
	}
}

// isNilStore 报告存储是否为 nil，包括装进接口的 typed-nil。
//
// `var store *persist.MemoryStore` 装进接口之后接口本身非 nil，只查 store == nil
// 会放它过去，错误要拖到第一次读写才以 nil 解引用的形式炸出来。
func isNilStore(store persist.CacheStore) bool {
	if store == nil {
		return true
	}

	switch value := reflect.ValueOf(store); value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice, reflect.UnsafePointer:
		return value.IsNil()
	default:
		return false
	}
}

func (m *Middleware) executeWithSingleFlightSafe(c *gin.Context, req cacheRequest) {
	var executedHandler atomic.Bool

	// 释放定时器必须用同一个 flight key，否则 Forget 删的是另一条 flight。
	flight := m.flightKey(req)

	result, err, shared := m.sfGroup.Do(flight, func() (any, error) {
		// 定时器建在 flight 内并在结束时停止，与 persist/twolevel.go 的写法一致。
		// 建在 Do 之外的话每个等待者也会建一个、而且没有一个会被停止：早已结束的
		// 请求，其定时器会在毫不相干的后续 flight 期间 Forget，同一个 key 于是
		// 出现多个 leader，防击穿失效；高 miss 流量下还会持续积累无效定时器。
		superseded, stopForget := m.scheduleForget(flight)
		defer stopForget()

		var cached ResponseCache
		if err := req.store.Get(req.key, &cached); err == nil {
			// 这里也要复检：不合规的历史条目若被放出去，leader 和所有等待者会
			// 一起 fall through 去执行 handler，singleflight 就被击穿了。
			if _, ok := m.replayable(&cached); ok {
				return &cached, nil
			}
		}

		executedHandler.Store(true)
		resp := m.executeHandler(c)

		if !req.noStore {
			m.cacheResponseFenced(flight, superseded, req, resp)
		}

		if reason, ok := unshareable(req, resp); !ok {
			if m.cfg.logger != nil {
				m.cfg.logger.Debugf("gincache: leader response not shareable (%s), waiters fall through", reason)
			}
			return nil, nil
		}

		return resp, nil
	})
	if err != nil {
		if m.cfg.logger != nil {
			m.cfg.logger.Errorf("gincache: singleflight error: %v", err)
		}
		c.Next()
		return
	}

	// 先判 leader 再判类型断言。leader 的响应已经通过包装器写出去了，
	// 若让它落进下面的 c.Next()，整条 handler 链会被二次执行。
	if executedHandler.Load() {
		if shared && m.cfg.shareSingleFlightCallback != nil {
			m.cfg.shareSingleFlightCallback(c)
		}
		c.Abort()
		return
	}

	// 走到这里说明响应不是本请求产生的：要么来自 store，要么是 leader 刚跑出来的。
	// leader 判定过不可复用时返回的是空，等待者据此各自回源。
	resp, ok := result.(*ResponseCache)
	if !ok {
		c.Next()
		return
	}

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
}

// unshareable 判定 leader 刚产生的这份响应能不能交给并发等待者，不能时给出原因。
//
// "能不能写库"和"能不能共享"是两件事，但都取决于同一组事实。以前只在写库路径
// 上拦，等待者照拿不误——实测过三种：no-store 请求的响应被原样发给普通请求；
// 超限被丢弃 Body 的响应回放成空 Body；Hijack 之后包装器什么都没观察到，
// 等待者却拿到一个编出来的空 200。
func unshareable(req cacheRequest, resp *ResponseCache) (string, bool) {
	switch {
	case req.noStore:
		return "request declared no-store", false
	case !resp.written:
		return "handler wrote nothing through the wrapper", false
	case resp.tooLarge:
		return "body exceeded max cache size", false
	}
	return "", true
}

// scheduleForget 为一次 flight 安排释放定时器。入参是 flight key（可能带存储
// 命名空间前缀），不是原始的 cacheKey。
//
// 返回的 superseded 在定时器触发后为真，表示这条 flight 已被释放、可能已有新
// leader 在跑。stop 在 flight 结束时停止定时器。
//
// Forget 只是解除 key 与本 flight 的关联，并不会取消本 flight。被释放之后，新
// leader 可能先跑完并写入较新的结果，而旧 leader 随后完成时若照样写缓存，就把新
// 结果覆盖成了旧的——实测请求 C 拿到旧响应且 X-Cache: HIT。响应自带的年龄机制
// 兜不住：没有 Cache-Control 时新鲜期未声明，回放不会被拒。因此需要这个标记。
func (m *Middleware) scheduleForget(flight string) (superseded *atomic.Bool, stop func()) {
	superseded = new(atomic.Bool)
	if m.cfg.singleFlightForgetTimeout <= 0 {
		return superseded, func() {}
	}

	timer := time.AfterFunc(m.cfg.singleFlightForgetTimeout, func() {
		// 先置位再 Forget：反过来的话，会出现"新 leader 已经可以产生、而旧
		// leader 还没被标记"的瞬间。
		superseded.Store(true)
		m.sfGroup.Forget(flight)
	})
	return superseded, func() { timer.Stop() }
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

	// 执行后续 Handler。顺带量出耗时：handler 在做上游代理时，这段时间也算进
	// 响应的年龄（RFC 9111 §4.2.3 的 response_delay）。
	start := time.Now()
	c.Next()
	responseDelay := time.Since(start)

	// response_time 就是此刻。年龄相关的两个字段都在这里落定而不是写入缓存时：
	// 它们是响应的固有属性，而"有没有被写进缓存"是另一回事——新鲜度不合格的
	// 响应会在写库路径提前返回，那时再算就永远算不上，等待者据此把陈旧响应
	// 当成新鲜的收下。
	responseTime := time.Now()

	headers := cloneCachedHeaders(cw.Header())

	// 复制 Body（重要：必须复制，因为 buffer 会被重用）
	body := make([]byte, cw.body.Len())
	copy(body, cw.body.Bytes())

	return &ResponseCache{
		Status:        cw.Status(),
		Header:        flattenLegacyHeaders(headers),
		Headers:       headers,
		Body:          body,
		ResponseTime:  responseTime.UnixNano(),
		InitialAge:    int64(max(headerAge(headers, responseDelay), 0)),
		tooLarge:      cw.overflowed,
		written:       cw.written,
		responseDelay: responseDelay,
	}
}

// cacheResponseFenced 在分片临界区内确认这条 flight 未被淘汰，然后写缓存。
//
// 校验与写入必须在同一临界区。只在写入前查一次不够：定时器可以在查过之后触发，
// 新 leader 随即产生并写入更新的响应，而这份较旧的结果最后落地就把它盖掉了。
// 放进同一把锁之后两种交错都以新值收尾——superseded 在 Forget 之前置位，而新 leader
// 只可能在 Forget 之后产生，因此旧 leader 若在新 leader 写入之后拿到锁，必然看到
// 标记已置位；在之前拿到，它写的旧值会被随后同样要拿锁的新 leader 覆盖。
//
// 锁横跨一次 store 写入（网络往返）。按 flight key 分 256 片，只在同分片写入之间
// 生效。代价是同分片的另一条 flight 若在写库时被拖慢（Redis 卡顿），这条 flight 的
// 等待者也会多等一会儿——写缓存发生在 singleflight 的回调内，回调返回才唤醒等待者。
// 上限是 writeCacheEntry 里那次写入的超时，前提是存储实现尊重 ctx（见
// persist.CacheStore 的接口文档）。
//
// 只 fence 写缓存这一处：响应仍要返给本请求的调用方，也仍要交给早已加入的等待者
// （他们一直在等这一份，改判会让他们凭空落空）。
func (m *Middleware) cacheResponseFenced(flight string, superseded *atomic.Bool, req cacheRequest, resp *ResponseCache) {
	// 判据先在锁外求值完毕。它包含调用方提供的回调（WithCacheableResponse），
	// 在临界区内跑调用方代码有两个后果：一次慢回调会堵住整个分片；回调若再触发
	// 同分片 key 的缓存写入会直接自锁死（sync.Mutex 不可重入）。
	final, ok := m.cacheDecision(resp, req.duration)
	if !ok {
		return
	}

	// 锁只覆盖"确认未被淘汰 + 写入"这两步。
	guard := m.writeGuard(flight)
	guard.Lock()
	defer guard.Unlock()

	if superseded.Load() {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: flight superseded after forget timeout, skip cache write")
		}
		return
	}

	m.writeCacheEntry(req.key, resp, req.store, final)
}

// writeCacheEntry 把条目写入存储。
func (m *Middleware) writeCacheEntry(key string, resp *ResponseCache, store persist.CacheStore, duration time.Duration) {
	// 使用独立于请求的 context，避免客户端取消导致缓存回填中断。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := store.SetWithContext(ctx, key, resp, duration); err != nil {
		if m.cfg.logger != nil {
			m.cfg.logger.Errorf("gincache: failed to set cache: %v", err)
		}
	}
}

func (m *Middleware) cacheResponse(key string, resp *ResponseCache, store persist.CacheStore, duration time.Duration) {
	final, ok := m.cacheDecision(resp, duration)
	if !ok {
		return
	}
	m.writeCacheEntry(key, resp, store, final)
}

// cacheDecision 判定这份响应能否入库，并给出最终 TTL。
// 纯判定，不写入、不持锁——判据里有调用方回调，不能在临界区内求值。
func (m *Middleware) cacheDecision(resp *ResponseCache, duration time.Duration) (time.Duration, bool) {
	// 包装器没观察到任何写入：handler 要么 Hijack 接管了连接（WebSocket 升级等），
	// 要么根本没产生响应。此时 Status 和 Body 都是包装器编出来的默认值，缓存它
	// 等于用一个空 200 顶掉后续所有请求。
	if !resp.written {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: handler wrote nothing through the wrapper (hijacked?), skip cache")
		}
		return 0, false
	}

	// 检查是否应该缓存
	if !m.shouldCache(resp) {
		return 0, false
	}

	if resp.tooLarge {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: body exceeded max cache size, skip cache")
		}
		return 0, false
	}

	// 检查 Body 大小限制
	if m.cfg.maxBodySize > 0 && int64(len(resp.Body)) > m.cfg.maxBodySize {
		if m.cfg.logger != nil {
			m.cfg.logger.Debugf("gincache: body too large, skip cache: %d > %d", len(resp.Body), m.cfg.maxBodySize)
		}
		return 0, false
	}

	// 响应自己声明的新鲜期约束回放时长：声明已经过期就不该进缓存，声明比配置短
	// 就以声明为准。配置 TTL 因此是上限，而不是最终值。
	if declared, ok := responseFreshness(resp.Headers, resp.responseDelay); ok {
		if declared <= 0 {
			if m.cfg.logger != nil {
				m.cfg.logger.Debugf("gincache: response declares no freshness lifetime, skip cache")
			}
			return 0, false
		}
		// duration 为 0 表示"交由存储决定"，那是调用方给出的契约，声明的新鲜期
		// 不该反过来把它顶开：一个完全合法的 max-age=31536000 会把存储的一分钟
		// 默认变成一年。想要响应驱动的 TTL，就给一个显式的上限。
		if duration > 0 && declared < duration {
			duration = declared
		}
	}

	return duration, true
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

	// 估算不出年龄的条目不回放。RFC 要求复用存储的响应时给出当前 Age，给不出
	// 就不该复用。这类条目只可能来自本版本之前的写入：handler 会重新产生响应
	// 并带上入库时刻回填，因此每个 key 只需一次回源，而且被 singleflight 合并。
	age, ok := replayAge(header, resp.ResponseTime, resp.InitialAge)
	if !ok {
		return nil, false
	}

	// 新鲜度按 RFC 9111 §4.2 判定：freshness_lifetime > current_age。
	//
	// current_age 必须含驻留时间。只靠存储 TTL 兜不住：defaultExpire 传 0 时
	// TTL 由存储决定，存储默认值比声明的新鲜期长，条目就会活过自己的新鲜期
	// 还被当成新鲜的回放（实测 max-age=1 的条目 1.2 秒后仍然命中）。
	receivedAt := time.Now()
	if resp.ResponseTime > 0 {
		receivedAt = time.Unix(0, resp.ResponseTime)
	}
	if lifetime, declared := declaredLifetime(header, receivedAt); declared && lifetime <= age {
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

	// 复用缓存条目时必须给出当前年龄（RFC 9111 §5.1）。只扣本级 TTL 不够——
	// 下游的 CDN 或浏览器看到偏小的 Age 会把这份响应再多留一会儿。
	// replayable 已经保证年龄可估算，估不出的条目根本走不到回放。
	if age, ok := replayAge(header, resp.ResponseTime, resp.InitialAge); ok {
		c.Header("Age", strconv.FormatInt(int64(age/time.Second), 10))
	}

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

// replayAge 估算回放这个条目时的当前年龄（RFC 9111 §4.2.3 的 current_age）。
//
// 新格式条目（有 ResponseTime）用"记下的初始年龄 + 驻留时长"，那个初始年龄已经
// 折进了上游的 Age 与写入时的 response_delay，因此不再重复读条目里的 Age 头。
// 旧格式条目只能从响应头估算：Date 给出自源站生成以来的总时长，条目里的 Age
// 是写入那一刻的年龄（不含驻留时长），两者取大者。
//
// 都估算不出来时返回 false——宁可判为不可回放，也不写一个编出来的年龄。
func replayAge(header http.Header, responseTime, initialAge int64) (time.Duration, bool) {
	var (
		age   time.Duration
		known bool
	)

	if dateRaw := header.Get("Date"); dateRaw != "" {
		if date, err := http.ParseTime(dateRaw); err == nil {
			age, known = max(time.Since(date), 0), true
		}
	}

	if responseTime > 0 {
		resident := max(time.Since(time.Unix(0, responseTime)), 0)
		return max(age, saturatingAdd(resident, nonNegative(initialAge))), true
	}

	// 旧格式条目：条目里的 Age 是写入那一刻的年龄，不含驻留时长，只能用来把
	// 估算往大了抬，不能单独当作"年龄可估"——没有任何时间锚点时驻留时长无从
	// 得知，宁可判为不可回放。
	if known {
		if stored, ok := maxAgeValue(header.Values("Age")); ok {
			age = max(age, stored)
		}
	}

	return age, known
}

// nonNegative 把持久化的纳秒数转成 Duration，负值当 0——手写或损坏的条目可能带
// 一个负的初始年龄，直接用会把当前年龄算小。
func nonNegative(nanos int64) time.Duration {
	if nanos <= 0 {
		return 0
	}
	return time.Duration(nanos)
}

// saturatingAdd 相加并在溢出时钳到最大值，入参须为非负。
//
// 入库时的 Age 可能是 parseDeltaSeconds 钳出来的 MaxInt64，再加任意驻留时长
// 就会回绕成负数，被随后的 max 吃掉后输出 Age: 0——把"极老"报成"刚出炉"，
// 方向正好反了。
func saturatingAdd(a, b time.Duration) time.Duration {
	if sum := a + b; sum >= a {
		return sum
	}
	return time.Duration(math.MaxInt64)
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
	// 一趟扫描取两个条件请求头。逐个查找在头不存在时要各扫一遍整张表，而绝大多数
	// 请求两个都不带；一趟扫描顺带对非规范键天然正确——程序化构造的请求或前置
	// 中间件可能留下 "if-none-match" 这样的键，只按规范键查会整个漏掉。
	var (
		ifNoneMatch, ifModifiedSince []string
		hasIfNoneMatch               bool
	)
	for key, values := range r.Header {
		switch {
		case strings.EqualFold(key, "If-None-Match"):
			ifNoneMatch = mergeValues(ifNoneMatch, values)
			hasIfNoneMatch = true
		case strings.EqualFold(key, "If-Modified-Since"):
			ifModifiedSince = mergeValues(ifModifiedSince, values)
		}
	}

	// 只要 If-None-Match **存在**就压制 If-Modified-Since，哪怕它的值为空或不匹配
	// （RFC 9110 §13.2.2）。多个字段行等价于逗号连接的一个列表，因此整批送去比对。
	if hasIfNoneMatch {
		return etagMatches(ifNoneMatch, header.Get("ETag"))
	}

	// If-Modified-Since 是单个 Date 而不是列表，出现多行属于畸形请求，整体忽略。
	// 先判空再解析还有一层考虑：http.ParseTime 会依次尝试三种格式，每次失败都
	// 分配一个 *time.ParseError——绝大多数请求不带这个头，不该在命中热路径上白付。
	if len(ifModifiedSince) != 1 || ifModifiedSince[0] == "" {
		return false
	}

	since, err := http.ParseTime(ifModifiedSince[0])
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
	"Proxy-Authentication-Info",
	"Proxy-Authorization",
}

// needsHeaderCleanup 报告 header 是否需要规范化键或剔除逐跳字段。
// 两项都是无分配的扫描，用来在命中回放的热路径上判断能否跳过整表克隆。
func needsHeaderCleanup(header http.Header) bool {
	for key := range header {
		if http.CanonicalHeaderKey(key) != key || strings.HasPrefix(key, http.TrailerPrefix) {
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
	// Connection 与 Trailer 都会"指向"另一批字段，被指到的同样不能进缓存：
	// 删掉 Trailer 声明却留下它列出的字段，等于把 trailer 当成普通响应头缓存。
	// 必须先读再删——这两个头自己都在待删清单里。
	for _, list := range [2][]string{header["Connection"], header["Trailer"]} {
		for _, value := range list {
			for name := range strings.SplitSeq(value, ",") {
				if name = strings.TrimSpace(name); name != "" {
					// 这些名字来自响应方写下的任意字符串，必须走规范化。
					header.Del(name)
				}
			}
		}
	}

	for _, name := range hopByHopHeaders {
		delete(header, name)
	}

	// net/http 用 "Trailer:" 前缀承载运行期追加的 trailer。这类键含冒号，
	// 规范化会原样返回，因此既躲过了规范键检查也不在固定清单里——缓存下来
	// 就会被当成普通响应头回放。
	for key := range header {
		if strings.HasPrefix(key, http.TrailerPrefix) {
			delete(header, key)
		}
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
