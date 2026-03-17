// Package gincache 提供高性能的 Gin HTTP 响应缓存中间件
// 基于 chenyahui/gin-cache 设计思路，使用最新依赖重写
package gincache

import (
	"bytes"
	"encoding/json"
	"gincache/persist"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

// ResponseCache 缓存的响应数据
type ResponseCache struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header"`
	Body   []byte            `json:"body"`
}

// Strategy 缓存策略
type Strategy struct {
	CacheKey      string             // 自定义缓存 Key
	CacheStore    persist.CacheStore // 可选：使用不同的存储后端
	CacheDuration time.Duration      // 可选：覆盖默认 TTL
}

// GetCacheStrategyByRequest 根据请求动态决定缓存策略
// 返回值：(是否缓存, 缓存策略)
type GetCacheStrategyByRequest func(c *gin.Context) (bool, Strategy)

// OnHitCacheCallback 缓存命中回调
type OnHitCacheCallback func(c *gin.Context)

// OnMissCacheCallback 缓存未命中回调
type OnMissCacheCallback func(c *gin.Context)

// OnShareSingleFlightCallback singleflight 共享结果回调
type OnShareSingleFlightCallback func(c *gin.Context)

// Logger 日志接口
type Logger interface {
	Errorf(format string, args ...any)
}

// config 内部配置
type config struct {
	getCacheStrategy          GetCacheStrategyByRequest
	hitCacheCallback          OnHitCacheCallback
	missCacheCallback         OnMissCacheCallback
	shareSingleFlightCallback OnShareSingleFlightCallback
	singleFlightForgetTimeout time.Duration
	ignoreQueryOrder          bool
	logger                    Logger
}

// Option 配置选项函数
type Option func(*config)

// WithCacheStrategyByRequest 设置自定义缓存策略
func WithCacheStrategyByRequest(fn GetCacheStrategyByRequest) Option {
	return func(c *config) {
		c.getCacheStrategy = fn
	}
}

// WithOnHitCache 设置缓存命中回调
func WithOnHitCache(cb OnHitCacheCallback) Option {
	return func(c *config) {
		c.hitCacheCallback = cb
	}
}

// WithOnMissCache 设置缓存未命中回调
func WithOnMissCache(cb OnMissCacheCallback) Option {
	return func(c *config) {
		c.missCacheCallback = cb
	}
}

// WithOnShareSingleFlight 设置 singleflight 共享结果回调
func WithOnShareSingleFlight(cb OnShareSingleFlightCallback) Option {
	return func(c *config) {
		c.shareSingleFlightCallback = cb
	}
}

// WithSingleFlightForgetTimeout 设置 singleflight 超时
// 超时后会调用 Forget，防止长尾请求阻塞后续请求
func WithSingleFlightForgetTimeout(timeout time.Duration) Option {
	return func(c *config) {
		c.singleFlightForgetTimeout = timeout
	}
}

// WithIgnoreQueryOrder 忽略 query 参数顺序
// 例如 ?a=1&b=2 和 ?b=2&a=1 会被视为相同的缓存 Key
func WithIgnoreQueryOrder() Option {
	return func(c *config) {
		c.ignoreQueryOrder = true
	}
}

// WithLogger 设置自定义日志
func WithLogger(logger Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}

// =========================================================================
// 缓存中间件
// =========================================================================

var sfGroup singleflight.Group

// Cache 通用缓存中间件，需要通过 WithCacheStrategyByRequest 设置缓存策略
func Cache(store persist.CacheStore, defaultExpire time.Duration, opts ...Option) gin.HandlerFunc {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.getCacheStrategy == nil {
		panic("gincache: WithCacheStrategyByRequest is required for Cache()")
	}

	return cacheHandler(store, defaultExpire, cfg)
}

// CacheByRequestURI 按完整 URI 缓存（包含 query string）
// 适用场景：列表接口带分页参数，如 /products?page=1&per_page=20
func CacheByRequestURI(store persist.CacheStore, expire time.Duration, opts ...Option) gin.HandlerFunc {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	cfg.getCacheStrategy = func(c *gin.Context) (bool, Strategy) {
		uri := c.Request.RequestURI
		if cfg.ignoreQueryOrder {
			uri = normalizeURI(c.Request.URL.Path, c.Request.URL.RawQuery)
		}
		return true, Strategy{CacheKey: uri}
	}

	return cacheHandler(store, expire, cfg)
}

// CacheByRequestPath 按路径缓存（忽略 query string）
// 适用场景：详情接口，如 /products/123
func CacheByRequestPath(store persist.CacheStore, expire time.Duration, opts ...Option) gin.HandlerFunc {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	cfg.getCacheStrategy = func(c *gin.Context) (bool, Strategy) {
		return true, Strategy{CacheKey: c.Request.URL.Path}
	}

	return cacheHandler(store, expire, cfg)
}

// cacheHandler 核心缓存处理逻辑
func cacheHandler(defaultStore persist.CacheStore, defaultExpire time.Duration, cfg *config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 获取缓存策略
		shouldCache, strategy := cfg.getCacheStrategy(c)
		if !shouldCache {
			c.Next()
			return
		}

		// 确定缓存参数
		cacheKey := strategy.CacheKey
		cacheStore := defaultStore
		cacheDuration := defaultExpire

		if strategy.CacheStore != nil {
			cacheStore = strategy.CacheStore
		}
		if strategy.CacheDuration > 0 {
			cacheDuration = strategy.CacheDuration
		}

		// 2. 尝试从缓存读取
		var cachedResp ResponseCache
		if err := cacheStore.Get(cacheKey, &cachedResp); err == nil {
			// 缓存命中
			if cfg.hitCacheCallback != nil {
				cfg.hitCacheCallback(c)
			}
			writeResponse(c, &cachedResp)
			c.Abort()
			return
		}

		// 3. 缓存未命中，使用 singleflight 防止击穿
		if cfg.missCacheCallback != nil {
			cfg.missCacheCallback(c)
		}

		// singleflight 超时控制
		if cfg.singleFlightForgetTimeout > 0 {
			time.AfterFunc(cfg.singleFlightForgetTimeout, func() {
				sfGroup.Forget(cacheKey)
			})
		}

		// 使用 DoChan 支持超时控制
		ch := sfGroup.DoChan(cacheKey, func() (any, error) {
			// Double-check：可能前一个请求已经填充了缓存
			var cached ResponseCache
			if err := cacheStore.Get(cacheKey, &cached); err == nil {
				return &cached, nil
			}

			// 执行真正的 Handler
			resp := executeHandler(c)

			// 只缓存 2xx 响应
			if resp.Status >= 200 && resp.Status < 300 {
				if err := cacheStore.Set(cacheKey, resp, cacheDuration); err != nil {
					if cfg.logger != nil {
						cfg.logger.Errorf("gincache: failed to set cache: %v", err)
					}
				}
			}

			return resp, nil
		})

		// 等待结果
		result := <-ch
		resp := result.Val.(*ResponseCache)

		// 如果是共享结果，触发回调
		if result.Shared && cfg.shareSingleFlightCallback != nil {
			cfg.shareSingleFlightCallback(c)
		}

		// 如果是共享结果，需要写入响应（因为当前请求的 Handler 没有执行）
		if result.Shared {
			writeResponse(c, resp)
			c.Abort()
		}
	}
}

// =========================================================================
// 辅助函数
// =========================================================================

// responseWriter 包装 gin.ResponseWriter 以捕获响应
type responseWriter struct {
	gin.ResponseWriter
	body   *bytes.Buffer
	status int
}

var writerPool = sync.Pool{
	New: func() any {
		return &responseWriter{
			body: bytes.NewBuffer(make([]byte, 0, 4096)),
		}
	},
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *responseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *responseWriter) reset(rw gin.ResponseWriter) {
	w.ResponseWriter = rw
	w.body.Reset()
	w.status = 0
}

// executeHandler 执行 Handler 并捕获响应
func executeHandler(c *gin.Context) *ResponseCache {
	writer := writerPool.Get().(*responseWriter)
	defer func() {
		writer.reset(nil)
		writerPool.Put(writer)
	}()

	writer.reset(c.Writer)
	c.Writer = writer

	c.Next()

	// 收集响应头
	header := make(map[string]string)
	for k, v := range writer.Header() {
		if len(v) > 0 {
			header[k] = v[0]
		}
	}

	return &ResponseCache{
		Status: writer.Status(),
		Header: header,
		Body:   writer.body.Bytes(),
	}
}

// writeResponse 写入缓存的响应
func writeResponse(c *gin.Context, resp *ResponseCache) {
	for k, v := range resp.Header {
		c.Header(k, v)
	}
	c.Header("X-Cache", "HIT")
	c.Data(resp.Status, resp.Header["Content-Type"], resp.Body)
}

// normalizeURI 规范化 URI（排序 query 参数）
func normalizeURI(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}

	params := strings.Split(rawQuery, "&")
	sort.Strings(params)
	return path + "?" + strings.Join(params, "&")
}

// GenerateCacheKey 生成缓存 Key（可用于外部主动失效）
func GenerateCacheKey(prefix string, parts ...string) string {
	h := fnv.New64a()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte(p))
	}
	return prefix + ":" + strconv.FormatUint(h.Sum64(), 36)
}

// =========================================================================
// 序列化辅助
// =========================================================================

// Serialize 序列化响应缓存
func Serialize(resp *ResponseCache) ([]byte, error) {
	return json.Marshal(resp)
}

// Deserialize 反序列化响应缓存
func Deserialize(data []byte, resp *ResponseCache) error {
	return json.Unmarshal(data, resp)
}
