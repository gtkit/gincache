package main

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/gtkit/gincache/persist"
	"github.com/redis/go-redis/v9"
)

func TestLoadConfigAndStats(t *testing.T) {
	t.Setenv("SERVER_ADDR", "127.0.0.1:18080")
	t.Setenv("REDIS_ADDR", "127.0.0.1:63790")
	t.Setenv("REDIS_PASSWORD", "secret")

	cfg := loadConfig()
	if cfg.ServerAddr != "127.0.0.1:18080" || cfg.RedisAddr != "127.0.0.1:63790" || cfg.RedisPassword != "secret" {
		t.Fatalf("loadConfig = %#v, want env values", cfg)
	}
	if got := getEnv("MISSING_ENV", "fallback"); got != "fallback" {
		t.Fatalf("getEnv fallback = %q, want fallback", got)
	}

	stats := &CacheStats{}
	if got := stats.HitRate(); got != 0 {
		t.Fatalf("empty HitRate = %v, want 0", got)
	}
	stats.Hit.Add(3)
	stats.Miss.Add(1)
	if got := stats.HitRate(); got != 75 {
		t.Fatalf("HitRate = %v, want 75", got)
	}
}

func TestStdLogger(t *testing.T) {
	var out bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
	})

	logger := &stdLogger{}
	logger.Errorf("err %s", "x")
	logger.Debugf("debug %s", "y")

	if got := out.String(); !bytes.Contains([]byte(got), []byte("[ERROR] err x")) || !bytes.Contains([]byte(got), []byte("[DEBUG] debug y")) {
		t.Fatalf("log output = %q, want error and debug entries", got)
	}
}

func TestProductServiceUpdateInvalidatesCache(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redisClientForTest(t, mini.Addr())
	store := persist.NewRedisStore(client, persist.WithKeyPrefix(""))

	if err := store.Set("/api/v1/products/42", map[string]any{"id": 42}, time.Minute); err != nil {
		t.Fatalf("Set detail cache error: %v", err)
	}
	if err := store.Set("/api/v1/products?page=1", map[string]any{"page": 1}, time.Minute); err != nil {
		t.Fatalf("Set list cache error: %v", err)
	}

	service := &ProductService{cache: store}
	if err := service.Update(context.Background(), 42, map[string]any{"name": "updated"}); err != nil {
		t.Fatalf("Update error: %v", err)
	}

	if mini.Exists("/api/v1/products/42") {
		t.Fatal("detail cache still exists")
	}
	if mini.Exists("/api/v1/products?page=1") {
		t.Fatal("list cache still exists")
	}
}

func TestMainStartsAndStops(t *testing.T) {
	mini := miniredis.RunT(t)
	addr := freeTCPAddr(t)
	t.Cleanup(func() {
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
	})

	t.Setenv("SERVER_ADDR", addr)
	t.Setenv("REDIS_ADDR", mini.Addr())
	t.Setenv("REDIS_PASSWORD", "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		main()
	}()

	waitHTTP(t, "http://"+addr+"/health")

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("main did not stop after SIGTERM")
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port error: %v", err)
	}
	defer func() {
		_ = ln.Close()
	}()
	return ln.Addr().String()
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", url)
}

func redisClientForTest(t *testing.T, addr string) *redis.Client {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}
