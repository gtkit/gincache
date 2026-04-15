// Package gincache 提供生产级的 Gin HTTP 响应缓存中间件
package gincache

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
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
}

// Option 配置选项.
type Option func(*Config)

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
// 超时后会调用 Forget，防止长尾请求阻塞后续请求
func WithSingleFlightForgetTimeout(timeout time.Duration) Option {
	return func(c *Config) { c.singleFlightForgetTimeout = timeout }
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
func WithMaxBodySize(size int64) Option {
	return func(c *Config) { c.maxBodySize = size }
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
func New(store persist.CacheStore, defaultExpire time.Duration, opts ...Option) *Middleware {
	cfg := &Config{
		singleFlightForgetTimeout: 10 * time.Second, // 默认 10 秒
	}
	for _, opt := range opts {
		opt(cfg)
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

// CacheByURI 按 URI 缓存的中间件.
func (m *Middleware) CacheByURI() gin.HandlerFunc {
	return func(c *gin.Context) {
		uri := c.Request.RequestURI
		if m.cfg.ignoreQueryOrder {
			uri = normalizeURI(c.Request.URL.Path, c.Request.URL.RawQuery)
		}
		m.handleWithKey(c, uri)
	}
}

// CacheByPath 按路径缓存的中间件.
func (m *Middleware) CacheByPath() gin.HandlerFunc {
	return func(c *gin.Context) {
		m.handleWithKey(c, c.Request.URL.Path)
	}
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
	// 1. 尝试从缓存读取
	var cached ResponseCache
	if err := store.Get(cacheKey, &cached); err == nil {
		if m.cfg.hitCacheCallback != nil {
			m.cfg.hitCacheCallback(c)
		}
		m.writeResponse(c, &cached)
		c.Abort()
		return
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
	if m.cfg.singleFlightForgetTimeout > 0 {
		time.AfterFunc(m.cfg.singleFlightForgetTimeout, func() {
			m.sfGroup.Forget(cacheKey)
		})
	}

	var executedHandler atomic.Bool

	result, err, shared := m.sfGroup.Do(cacheKey, func() (any, error) {
		var cached ResponseCache
		if err := store.Get(cacheKey, &cached); err == nil {
			return &cached, nil
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
		if shared && m.cfg.shareSingleFlightCallback != nil {
			m.cfg.shareSingleFlightCallback(c)
		}
		m.writeResponse(c, resp)
		c.Abort()
		return
	}

	if shared && m.cfg.shareSingleFlightCallback != nil {
		m.cfg.shareSingleFlightCallback(c)
	}
	c.Abort()
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
	defer putWriter(cw)

	originalWriter := c.Writer
	c.Writer = cw

	// 执行后续 Handler
	c.Next()

	// 恢复原始 Writer
	c.Writer = originalWriter

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
	}
}

func (m *Middleware) cacheResponse(key string, resp *ResponseCache, store persist.CacheStore, duration time.Duration) {
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
	if m.cfg.cacheableStatusCodes != nil {
		return m.cfg.cacheableStatusCodes[resp.Status]
	}
	// 默认只缓存 2xx
	return resp.Status >= 200 && resp.Status < 300
}

func (m *Middleware) writeResponse(c *gin.Context, resp *ResponseCache) {
	applyCachedHeaders(c.Writer.Header(), resp)
	c.Header("X-Cache", "HIT")

	c.Status(resp.Status)
	if c.Request.Method == http.MethodHead || len(resp.Body) == 0 {
		c.Writer.WriteHeaderNow()
		return
	}
	_, _ = c.Writer.Write(resp.Body)
}

func cloneCachedHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}

	cloned := src.Clone()
	delete(cloned, "X-Cache")
	return cloned
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

func applyCachedHeaders(dst http.Header, resp *ResponseCache) {
	if len(resp.Headers) > 0 {
		for key, values := range resp.Headers {
			copied := append([]string(nil), values...)
			dst[key] = copied
		}
		return
	}

	for key, value := range resp.Header {
		dst.Set(key, value)
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
