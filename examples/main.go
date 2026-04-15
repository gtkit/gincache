// 生产级 gincache 使用示例
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
	"github.com/redis/go-redis/v9"
)

// =========================================================================
// 配置
// =========================================================================

type Config struct {
	ServerAddr    string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
}

func loadConfig() *Config {
	return &Config{
		ServerAddr:    getEnv("SERVER_ADDR", ":8080"),
		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       0,
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// =========================================================================
// 日志适配器
// =========================================================================

type stdLogger struct{}

func (l *stdLogger) Errorf(format string, args ...any) {
	log.Printf("[ERROR] "+format, args...)
}

func (l *stdLogger) Debugf(format string, args ...any) {
	log.Printf("[DEBUG] "+format, args...)
}

// =========================================================================
// 统计
// =========================================================================

type CacheStats struct {
	Hit  atomic.Uint64
	Miss atomic.Uint64
}

func (s *CacheStats) HitRate() float64 {
	hit := s.Hit.Load()
	miss := s.Miss.Load()
	total := hit + miss
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total) * 100
}

// =========================================================================
// 主函数
// =========================================================================

func main() {
	cfg := loadConfig()
	gin.SetMode(gin.ReleaseMode)

	// =====================================================================
	// 初始化 Redis
	// =====================================================================
	redisClient := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		PoolSize:     100,
		MinIdleConns: 20,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  4 * time.Second,
	})

	// 检查 Redis 连接
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("Redis connection failed: %v", err)
	}
	log.Println("Redis connected")

	// =====================================================================
	// 初始化缓存存储
	// =====================================================================

	// 方案 1: Redis 单层缓存（适合大多数场景）
	redisStore := persist.NewRedisStore(redisClient,
		persist.WithKeyPrefix("myapp:api:"),
		persist.WithReadTimeout(2*time.Second),
		persist.WithWriteTimeout(2*time.Second),
	)

	// 方案 2: 两级缓存（适合高并发热点数据）
	twoLevelStore := persist.NewTwoLevelStore(redisClient,
		persist.WithLocalTTL(15*time.Second),
		persist.WithRemoteTTL(5*time.Minute),
		persist.WithTwoLevelKeyPrefix("myapp:hot:"),
	)
	defer twoLevelStore.Close()

	// 方案 3: 纯内存缓存（适合单实例、开发环境）
	memoryStore := persist.NewMemoryStore(time.Minute,
		persist.WithCleanupInterval(30*time.Second),
	)
	defer memoryStore.Close()

	// =====================================================================
	// 缓存统计
	// =====================================================================
	stats := &CacheStats{}
	logger := &stdLogger{}

	// 公共 Option
	commonOpts := []gincache.Option{
		gincache.WithOnHitCache(func(c *gin.Context) {
			stats.Hit.Add(1)
		}),
		gincache.WithOnMissCache(func(c *gin.Context) {
			stats.Miss.Add(1)
		}),
		gincache.WithLogger(logger),
		gincache.WithSingleFlightForgetTimeout(5 * time.Second),
		gincache.WithMaxBodySize(1 << 20), // 1MB 限制
	}

	// =====================================================================
	// 路由
	// =====================================================================
	r := gin.New()
	r.Use(gin.Recovery())

	// 健康检查（不缓存）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"time":   time.Now().Format(time.RFC3339),
		})
	})

	api := r.Group("/api/v1")
	{
		// =================================================================
		// 示例 1: 按 URI 缓存（含 query string）
		// 适用：列表接口、搜索接口
		// =================================================================
		api.GET("/products",
			gincache.CacheByRequestURI(redisStore, time.Minute, commonOpts...),
			func(c *gin.Context) {
				time.Sleep(50 * time.Millisecond) // 模拟 DB 查询
				page := c.DefaultQuery("page", "1")
				c.JSON(http.StatusOK, gin.H{
					"data":      []string{"product1", "product2"},
					"page":      page,
					"cached_at": time.Now().Unix(),
				})
			},
		)

		// =================================================================
		// 示例 2: 按路径缓存（忽略 query string）
		// 适用：详情接口
		// =================================================================
		api.GET("/products/:id",
			gincache.CacheByRequestPath(redisStore, 5*time.Minute, commonOpts...),
			func(c *gin.Context) {
				time.Sleep(30 * time.Millisecond)
				id := c.Param("id")
				c.JSON(http.StatusOK, gin.H{
					"id":        id,
					"name":      "Product " + id,
					"cached_at": time.Now().Unix(),
				})
			},
		)

		// =================================================================
		// 示例 3: 自定义缓存策略
		// 适用：复杂条件缓存
		// =================================================================
		api.GET("/orders",
			gincache.Cache(redisStore, time.Minute,
				append(commonOpts,
					gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
						status := c.Query("status")
						userID := c.GetHeader("X-User-ID")

						// 只缓存已完成订单
						if status != "completed" {
							return false, gincache.Strategy{}
						}

						// 按用户+状态缓存
						cacheKey := fmt.Sprintf("orders:user:%s:status:%s", userID, status)

						return true, gincache.Strategy{
							CacheKey:      cacheKey,
							CacheDuration: 2 * time.Minute,
						}
					}),
				)...,
			),
			func(c *gin.Context) {
				time.Sleep(80 * time.Millisecond)
				c.JSON(http.StatusOK, gin.H{
					"orders":    []string{"order1", "order2"},
					"status":    c.Query("status"),
					"cached_at": time.Now().Unix(),
				})
			},
		)

		// =================================================================
		// 示例 4: 两级缓存（热点数据）
		// 适用：首页配置、热门商品等高并发场景
		// =================================================================
		api.GET("/hot/config",
			gincache.CacheByRequestPath(twoLevelStore, 10*time.Minute, commonOpts...),
			func(c *gin.Context) {
				time.Sleep(20 * time.Millisecond)
				c.JSON(http.StatusOK, gin.H{
					"config":    map[string]any{"key": "value"},
					"cached_at": time.Now().Unix(),
				})
			},
		)

		// =================================================================
		// 示例 5: 内存缓存（单实例场景）
		// =================================================================
		api.GET("/local/data",
			gincache.CacheByRequestURI(memoryStore, 30*time.Second, commonOpts...),
			func(c *gin.Context) {
				time.Sleep(10 * time.Millisecond)
				c.JSON(http.StatusOK, gin.H{
					"data":      "local only",
					"cached_at": time.Now().Unix(),
				})
			},
		)

		// =================================================================
		// 示例 6: Query 参数排序
		// ?a=1&b=2 和 ?b=2&a=1 共享缓存
		// =================================================================
		api.GET("/search",
			gincache.CacheByRequestURI(redisStore, time.Minute,
				append(commonOpts, gincache.WithIgnoreQueryOrder())...,
			),
			func(c *gin.Context) {
				time.Sleep(100 * time.Millisecond)
				c.JSON(http.StatusOK, gin.H{
					"query":     c.Request.URL.RawQuery,
					"cached_at": time.Now().Unix(),
				})
			},
		)
	}

	// =====================================================================
	// 管理接口
	// =====================================================================
	admin := r.Group("/admin")
	{
		// 缓存统计
		admin.GET("/cache/stats", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"hit":       stats.Hit.Load(),
				"miss":      stats.Miss.Load(),
				"hit_rate":  fmt.Sprintf("%.2f%%", stats.HitRate()),
				"memory":    memoryStore.Stats(),
				"two_level": twoLevelStore.Stats(),
			})
		})

		// 手动删除缓存
		admin.DELETE("/cache/key/:key", func(c *gin.Context) {
			key := c.Param("key")
			if err := redisStore.Delete(key); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"deleted": key})
		})

		// 按模式删除
		admin.DELETE("/cache/pattern/:pattern", func(c *gin.Context) {
			pattern := c.Param("pattern")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			n, err := redisStore.DeletePattern(ctx, pattern)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"deleted": n, "pattern": pattern})
		})
	}

	// =====================================================================
	// 启动服务
	// =====================================================================
	srv := &http.Server{
		Addr:         cfg.ServerAddr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Server starting on %s", cfg.ServerAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}

	log.Println("Server stopped")
}

// =========================================================================
// 业务层缓存失效示例
// =========================================================================

// ProductService 商品服务（演示缓存失效）
type ProductService struct {
	cache *persist.RedisStore
}

// Update 更新商品
func (s *ProductService) Update(ctx context.Context, id int64, data map[string]any) error {
	// 1. 更新数据库
	// db.Update(...)

	// 2. 删除详情缓存
	detailKey := "/api/v1/products/" + strconv.FormatInt(id, 10)
	if err := s.cache.DeleteWithContext(ctx, detailKey); err != nil {
		log.Printf("Failed to delete cache: %v", err)
	}

	// 3. 删除列表缓存（按模式）
	if _, err := s.cache.DeletePattern(ctx, "/api/v1/products\\?*"); err != nil {
		log.Printf("Failed to delete pattern cache: %v", err)
	}

	return nil
}
