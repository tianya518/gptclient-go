package main

import (
	"fmt"
	"log"

	"sentinel-go/server"
)

func main() {
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
	log.Printf("============================================")

	// 2. 初始化 Token 池
	pool := server.NewTokenPool(cfg.TokensFile)
	total, valid, _ := pool.Stats()
	log.Printf("[startup] Token pool: total=%d, valid=%d", total, valid)

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
