package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"sentinel-go/server"
)

func setupLogging() {
	logPath := strings.TrimSpace(os.Getenv("LOG_FILE"))
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[startup] open log file %s: %v", logPath, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	log.Printf("[startup] logging to %s", logPath)
}

func main() {
	fmt.Fprintln(os.Stderr, "[sentinel] starting...")
	setupLogging()

	// 1. 读取配置
	cfg := server.LoadConfig()

	log.Printf("============================================")
	log.Printf("  sentinel-go API Server")
	log.Printf("  Port           : %s", cfg.Port)
	log.Printf("  Default Model  : %s", cfg.DefaultModel)
	log.Printf("  Temp Mode      : %v", cfg.TempMode)
	log.Printf("  Tokens File    : %s", cfg.TokensFile)
	log.Printf("  Session TTL    : %d min", cfg.SessionTTLMinutes)
	if cfg.Authorization != "" {
		log.Printf("  Authorization  : configured (pool mode)")
	} else {
		log.Printf("  Authorization  : not set (direct token mode)")
	}
	if cfg.ProxyURL != "" {
		log.Printf("  Proxy URL      : %s", cfg.ProxyURL)
	}
	log.Printf("============================================")

	// 2. 初始化 Token 池
	pool := server.NewTokenPool(cfg.TokensFile, time.Duration(cfg.TokenRefreshAheadSec)*time.Second)
	pool.SetOAuthConfig(cfg.OAuthTokenURL, cfg.OAuthClientID)
	total, valid, _ := pool.Stats()
	log.Printf("[startup] Token pool: total=%d, valid=%d", total, valid)

	// 后台定时刷新（提前用 ST/RT 换 AT），间隔可配；<=0 关闭
	if cfg.RefreshLoopSec > 0 {
		pool.StartRefreshLoop(time.Duration(cfg.RefreshLoopSec) * time.Second)
	}

	// 3. 初始化 Session 管理器
	session := server.NewSessionManager(&cfg)
	log.Printf("[startup] Session manager initialized (TTL=%d min)", cfg.SessionTTLMinutes)

	// 4. 创建路由器
	r := server.NewRouter(&cfg, pool, session)

	// 5. 启动服务
	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("[startup] Listening on http://0.0.0.0%s", addr)
	log.Printf("[startup] API endpoint: http://0.0.0.0%s/v1/chat/completions", addr)

	if err := r.Run(addr); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
