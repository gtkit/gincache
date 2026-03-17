// Package gincache 提供生产级的 Gin HTTP 响应缓存中间件
package gincache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
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
	Status int               `json:"s"`
	Header map[string]string `json:"h"`
	Body   []byte            `json:"b"`
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
	cfg := &Config{
		singleFlightForgetTimeout: 10 * time.Second,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	m := &Middleware{store: store, defaultExpire: expire, cfg: cfg}

	return func(c *gin.Context) {
		uri := c.Request.RequestURI
		if cfg.ignoreQueryOrder {
			uri = normalizeURI(c.Request.URL.Path, c.Request.URL.RawQuery)
		}
		m.handleWithKey(c, uri)
	}
}

// CacheByRequestPath 按路径缓存.
func CacheByRequestPath(store persist.CacheStore, expire time.Duration, opts ...Option) gin.HandlerFunc {
	cfg := &Config{
		singleFlightForgetTimeout: 10 * time.Second,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	m := &Middleware{store: store, defaultExpire: expire, cfg: cfg}

	return func(c *gin.Context) {
		m.handleWithKey(c, c.Request.URL.Path)
	}
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
	m.executeWithSingleFlight(c, cacheKey, store, duration)
}

func (m *Middleware) executeAndCache(c *gin.Context, cacheKey string, store persist.CacheStore, duration time.Duration) {
	resp := m.executeHandler(c)
	m.cacheResponse(cacheKey, resp, store, duration)
}

func (m *Middleware) executeWithSingleFlight(c *gin.Context, cacheKey string, store persist.CacheStore, duration time.Duration) {
	// singleflight 超时控制
	if m.cfg.singleFlightForgetTimeout > 0 {
		time.AfterFunc(m.cfg.singleFlightForgetTimeout, func() {
			m.sfGroup.Forget(cacheKey)
		})
	}

	// 标记当前请求是否是首发请求
	var isLeader atomic.Bool
	isLeader.Store(true)

	resultCh := m.sfGroup.DoChan(cacheKey, func() (any, error) {
		// Double-check 缓存
		var cached ResponseCache
		if err := store.Get(cacheKey, &cached); err == nil {
			isLeader.Store(false)
			return &cached, nil
		}

		// 执行 Handler
		resp := m.executeHandler(c)

		// 缓存响应
		m.cacheResponse(cacheKey, resp, store, duration)

		return resp, nil
	})

	// 等待结果
	result := <-resultCh
	if result.Err != nil {
		if m.cfg.logger != nil {
			m.cfg.logger.Errorf("gincache: singleflight error: %v", result.Err)
		}
		c.Next()
		return
	}

	resp, ok := result.Val.(*ResponseCache)
	if !ok {
		c.Next()
		return
	}

	// 如果是共享结果（不是首发请求），需要写入响应
	if result.Shared {
		if m.cfg.shareSingleFlightCallback != nil {
			m.cfg.shareSingleFlightCallback(c)
		}
		// 共享结果的请求需要手动写入响应
		if !isLeader.Load() {
			m.writeResponse(c, resp)
			c.Abort()
		}
	}
}

// =========================================================================
// ResponseWriter 包装
// =========================================================================

type cachedWriter struct {
	gin.ResponseWriter
	body       *bytes.Buffer
	statusCode int
	written    bool
}

var writerPool = sync.Pool{
	New: func() any {
		return &cachedWriter{
			body: bytes.NewBuffer(make([]byte, 0, 4096)),
		}
	},
}

func getWriter(w gin.ResponseWriter) *cachedWriter {
	cw := writerPool.Get().(*cachedWriter)
	cw.ResponseWriter = w
	cw.body.Reset()
	cw.statusCode = 0
	cw.written = false
	return cw
}

func putWriter(cw *cachedWriter) {
	cw.ResponseWriter = nil
	cw.body.Reset()
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
	w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *cachedWriter) WriteString(s string) (int, error) {
	if !w.written {
		w.statusCode = http.StatusOK
		w.written = true
	}
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func (w *cachedWriter) Status() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

// =========================================================================
// 辅助方法
// =========================================================================

func (m *Middleware) executeHandler(c *gin.Context) *ResponseCache {
	cw := getWriter(c.Writer)
	defer putWriter(cw)

	originalWriter := c.Writer
	c.Writer = cw

	// 执行后续 Handler
	c.Next()

	// 恢复原始 Writer
	c.Writer = originalWriter

	// 收集响应头（只保留需要的）
	header := make(map[string]string)
	for _, key := range []string{"Content-Type", "Content-Encoding", "Cache-Control", "ETag", "Last-Modified"} {
		if v := cw.Header().Get(key); v != "" {
			header[key] = v
		}
	}

	// 复制 Body（重要：必须复制，因为 buffer 会被重用）
	body := make([]byte, cw.body.Len())
	copy(body, cw.body.Bytes())

	return &ResponseCache{
		Status: cw.Status(),
		Header: header,
		Body:   body,
	}
}

func (m *Middleware) cacheResponse(key string, resp *ResponseCache, store persist.CacheStore, duration time.Duration) {
	// 检查是否应该缓存
	if !m.shouldCache(resp) {
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
	// 设置响应头
	for k, v := range resp.Header {
		c.Header(k, v)
	}
	c.Header("X-Cache", "HIT")

	// 写入响应
	contentType := resp.Header["Content-Type"]
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	c.Data(resp.Status, contentType, resp.Body)
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
	sort.Strings(params)
	return path + "?" + strings.Join(params, "&")
}

// =========================================================================
// 序列化
// =========================================================================

// Serialize 序列化响应缓存.
func Serialize(resp *ResponseCache) ([]byte, error) {
	return json.Marshal(resp)
}

// Deserialize 反序列化响应缓存.
func Deserialize(data []byte) (*ResponseCache, error) {
	var resp ResponseCache
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// =========================================================================
// 错误定义
// =========================================================================

var (
	ErrCacheMiss    = errors.New("cache: key not found")
	ErrCacheExpired = errors.New("cache: key expired")
)
