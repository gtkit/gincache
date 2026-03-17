package main

import (
	"fmt"

	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
	"github.com/redis/go-redis/v9"
)

// 统计计数器
var (
	hitCount  uint64
	missCount uint64
)

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// =========================================================================
	// 初始化存储后端
	// =========================================================================

	// 1. 内存存储（适合单实例、开发环境）
	memoryStore := persist.NewMemoryStore(time.Minute)
	defer memoryStore.Close()

	// 2. Redis 存储（适合多实例、生产环境）
	redisClient := redis.NewClient(&redis.Options{
		Addr:         "localhost:6379",
		Password:     "",
		DB:           0,
		PoolSize:     50,
		MinIdleConns: 10,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	redisStore := persist.NewRedisStore(redisClient,
		persist.WithKeyPrefix("myapp:cache:"),
	)

	// 3. 两级缓存（适合高并发热点数据）
	twoLevelStore := persist.NewTwoLevelStore(
		30*time.Second, // 本地缓存 TTL
		redisClient,
		persist.WithLocalTTL(10*time.Second),
		persist.WithTwoLevelKeyPrefix("myapp:2l:"),
	)
	defer twoLevelStore.Close()

	// =========================================================================
	// 公共 Option（带统计回调）
	// =========================================================================
	defaultOpts := []gincache.Option{
		gincache.WithOnHitCache(func(c *gin.Context) {
			atomic.AddUint64(&hitCount, 1)
			log.Printf("[CACHE HIT] %s", c.Request.RequestURI)
		}),
		gincache.WithOnMissCache(func(c *gin.Context) {
			atomic.AddUint64(&missCount, 1)
			log.Printf("[CACHE MISS] %s", c.Request.RequestURI)
		}),
		gincache.WithOnShareSingleFlight(func(c *gin.Context) {
			log.Printf("[SINGLEFLIGHT] shared result for %s", c.Request.RequestURI)
		}),
		gincache.WithSingleFlightForgetTimeout(5 * time.Second),
	}

	// =========================================================================
	// 示例 1: 按 URI 缓存（含 query string）
	// =========================================================================
	var cache []gincache.Option
	cache = append(cache, defaultOpts...)
	r.GET("/products",
		gincache.CacheByRequestURI(redisStore, time.Minute, defaultOpts...),
		func(c *gin.Context) {
			time.Sleep(100 * time.Millisecond) // 模拟慢查询
			page := c.DefaultQuery("page", "1")
			c.JSON(http.StatusOK, gin.H{
				"message":   "product list",
				"page":      page,
				"timestamp": time.Now().Unix(),
			})
		},
	)

	// =========================================================================
	// 示例 2: 按路径缓存（忽略 query string）
	// =========================================================================
	r.GET("/products/:id", gincache.CacheByRequestPath(redisStore, 5*time.Minute, defaultOpts...), func(c *gin.Context) {
		time.Sleep(50 * time.Millisecond)
		id := c.Param("id")
		c.JSON(http.StatusOK, gin.H{
			"message":   "product detail",
			"id":        id,
			"timestamp": time.Now().Unix(),
		})
	})

	// =========================================================================
	// 示例 3: 自定义缓存策略
	// =========================================================================
	r.GET("/users/:id/orders", gincache.Cache(
		redisStore,
		time.Minute,
		append(defaultOpts, gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
			userID := c.Param("id")
			status := c.Query("status")

			// 只缓存已完成订单
			if status != "completed" {
				return false, gincache.Strategy{}
			}

			// 自定义缓存 Key
			cacheKey := fmt.Sprintf("user:%s:orders:%s", userID, status)

			return true, gincache.Strategy{
				CacheKey:      cacheKey,
				CacheDuration: 2 * time.Minute, // 覆盖默认 TTL
			}
		}))...,
	), func(c *gin.Context) {
		time.Sleep(80 * time.Millisecond)
		c.JSON(http.StatusOK, gin.H{
			"message":   "user orders",
			"user_id":   c.Param("id"),
			"status":    c.Query("status"),
			"timestamp": time.Now().Unix(),
		})
	})

	// =========================================================================
	// 示例 4: 使用两级缓存（热点数据）
	// =========================================================================
	r.GET("/hot/config", gincache.CacheByRequestPath(twoLevelStore, 10*time.Minute, defaultOpts...), func(c *gin.Context) {
		time.Sleep(30 * time.Millisecond)
		c.JSON(http.StatusOK, gin.H{
			"message":   "hot config (two-level cache)",
			"timestamp": time.Now().Unix(),
		})
	})

	// =========================================================================
	// 示例 5: 使用内存缓存（单实例场景）
	// =========================================================================
	r.GET("/local/data", gincache.CacheByRequestURI(memoryStore, 30*time.Second, defaultOpts...), func(c *gin.Context) {
		time.Sleep(20 * time.Millisecond)
		c.JSON(http.StatusOK, gin.H{
			"message":   "local memory cache",
			"timestamp": time.Now().Unix(),
		})
	})

	// =========================================================================
	// 示例 6: 忽略 query 参数顺序
	// =========================================================================
	r.GET("/search", gincache.CacheByRequestURI(
		redisStore,
		time.Minute,
		append(defaultOpts, gincache.WithIgnoreQueryOrder())...,
	), func(c *gin.Context) {
		time.Sleep(100 * time.Millisecond)
		c.JSON(http.StatusOK, gin.H{
			"message":   "search results (query order ignored)",
			"q":         c.Query("q"),
			"sort":      c.Query("sort"),
			"timestamp": time.Now().Unix(),
		})
	})

	// =========================================================================
	// 管理接口
	// =========================================================================

	// 缓存统计
	r.GET("/admin/cache/stats", func(c *gin.Context) {
		hit := atomic.LoadUint64(&hitCount)
		miss := atomic.LoadUint64(&missCount)
		total := hit + miss
		var hitRate float64
		if total > 0 {
			hitRate = float64(hit) / float64(total) * 100
		}
		c.JSON(http.StatusOK, gin.H{
			"hit":       hit,
			"miss":      miss,
			"total":     total,
			"hit_rate":  fmt.Sprintf("%.2f%%", hitRate),
			"local":     memoryStore.Stats(),
			"two_level": twoLevelStore.LocalStats(),
		})
	})

	// 手动删除缓存
	r.DELETE("/admin/cache/:key", func(c *gin.Context) {
		key := c.Param("key")
		if err := redisStore.Delete(key); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "deleted", "key": key})
	})

	// 按模式删除缓存
	r.DELETE("/admin/cache/pattern/:pattern", func(c *gin.Context) {
		pattern := c.Param("pattern")
		if err := redisStore.DeletePattern(pattern); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "pattern deleted", "pattern": pattern})
	})

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Unix()})
	})

	// =========================================================================
	// 启动服务
	// =========================================================================
	log.Println("Server starting on :8080")
	log.Println("Test endpoints:")
	log.Println("  GET /products?page=1")
	log.Println("  GET /products/123")
	log.Println("  GET /users/1/orders?status=completed")
	log.Println("  GET /hot/config")
	log.Println("  GET /local/data")
	log.Println("  GET /search?q=test&sort=price")
	log.Println("  GET /admin/cache/stats")

	if err := r.Run(":8080"); err != nil {
		log.Fatal(err)
	}
}

// =========================================================================
// 高级用法：在业务层主动失效缓存
// =========================================================================

// ProductService 示例：更新商品后失效缓存
type ProductService struct {
	store *persist.RedisStore
}

func (s *ProductService) Update(id int64, data map[string]any) error {
	// 1. 更新数据库
	// db.Save(...)

	// 2. 主动失效缓存
	// 删除详情缓存
	detailKey := "/products/" + strconv.FormatInt(id, 10)
	s.store.Delete(detailKey)

	// 删除列表缓存（按模式）
	s.store.DeletePattern("/products?*")

	return nil
}
