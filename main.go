package main

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"time"
)

// 安全升级器配置（生产环境优化）
var upgrader = websocket.Upgrader{
	ReadBufferSize:   64 * 1024,  // 64KB读缓冲区
	WriteBufferSize:  128 * 1024, // 128KB写缓冲区
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin: func(r *http.Request) bool {
		// 生产环境应严格校验Origin
		allowedOrigins := []string{
			"https://yourdomain.com",
			"https://api.yourdomain.com",
		}
		origin := r.Header.Get("Origin")
		for _, allowed := range allowedOrigins {
			if origin == allowed {
				return true
			}
		}
		return false
	},
	EnableCompression: true, // 启用压缩减少带宽
}

// BroadcastMessage 广播消息结构
type BroadcastMessage struct {
	Type      string      `json:"type"`       // 消息类型：broadcast, user, room
	Data      interface{} `json:"data"`       // 消息数据
	TargetIDs []string    `json:"target_ids"` // 目标用户ID列表
	RoomID    string      `json:"room_id"`    // 房间ID
}

func main() {
	// 初始化Hub（4个分片）
	hub := NewHub(4, "localhost:6379")
	go hub.run()

	// 设置路由
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})
	http.HandleFunc("/health", healthHandler(hub))
	http.HandleFunc("/stats", statsHandler(hub))

	// 优雅关闭处理
	server := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Println("WebSocket服务器启动在 :8080")
	if err := server.ListenAndServe(); err != nil {
		log.Fatal("服务器启动失败: ", err)
	}
}

// WebSocket连接处理
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// 升级HTTP连接到WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		return
	}

	// 从请求中获取用户信息（实际应从认证中间件获取）
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error": "user_id required"}`))
		conn.Close()
		return
	}

	// 创建客户端实例
	client := &Client{
		hub:       hub,
		conn:      conn,
		send:      make(chan []byte, 256), // 缓冲256条消息
		userID:    userID,
		sessionID: generateSessionID(),
	}

	// 注册客户端到Hub
	hub.registerClient(client)

	// 启动读写协程
	go client.writePump()
	go client.readPump()

	log.Printf("客户端连接成功: %s", userID)
}

// 生成会话ID
func generateSessionID() string {
	return "session_" + time.Now().Format("20060102150405")
}

// 健康检查端点
func healthHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		totalClients := 0
		for _, shard := range hub.shards {
			shard.mu.RLock()
			totalClients += len(shard.clients)
			shard.mu.RUnlock()
		}

		response := map[string]interface{}{
			"status":    "healthy",
			"timestamp": time.Now().Unix(),
			"clients":   totalClients,
			"shards":    len(hub.shards),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// 统计信息端点
func statsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := make(map[int]int)
		totalClients := 0

		for i, shard := range hub.shards {
			shard.mu.RLock()
			clientCount := len(shard.clients)
			stats[i] = clientCount
			totalClients += clientCount
			shard.mu.RUnlock()
		}

		response := map[string]interface{}{
			"total_clients": totalClients,
			"shard_stats":   stats,
			"timestamp":     time.Now().Unix(),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}
