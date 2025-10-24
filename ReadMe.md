# websocket高可用配置

##  hub 中心分片管理器
```go
// Hub 中心管理器（支持多分片）
type Hub struct {
	shards    []*Shard              // 分片数组
	broadcast chan BroadcastMessage // 全局广播通道
	redis     *redis.Client         // Redis客户端用于集群通信
	ctx       context.Context       // 上下文
}
// ctx 用于给hub全局设置变量
```

## shard 分片 （hub中的一个小处理器）
```go
// Shard 分片管理器
type Shard struct {
	clients    map[*Client]bool // 当前分片客户端集合
	register   chan *Client     // 注册通道
	unregister chan *Client     // 注销通道
	broadcast  chan []byte      // 分片内广播通道
	mu         sync.RWMutex     // 分片级别锁
}
// 每个shard都有一组活跃的客户端集合 一个用于添加新用户的注册通道 
//一个用于注销用户的通道 一个单分片内的广播通道 一个分片锁用于占用广播通道
```

## client 客户端 最底层的设计
```go
// Client 客户端连接结构
type Client struct {
	hub       *Hub  // hub管理器
	conn      *websocket.Conn   // 一个websocket连接
	send      chan []byte  // 发送消息缓冲通道
	userID    string       // 用户标识
	sessionID string       // 会话标识
	mu        sync.RWMutex // 客户端级别锁
}
// 每个客户端都有一个hub管理器 用于使用全局广播
// 一个conn发送和接受消息
// 一个send 发送消息的缓冲通道 
// 用户标识和此websocket会话标识
// 客户   端锁
```
## 执行流程
1. 初始化
```go
// 初始化Hub（4个分片）
hub := NewHub(4, "localhost:6379")
go hub.run()    // 监听广播通道

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
```
```go
// 启动中心管理器
func (h *Hub) run() {
	// 启动所有分片
	for i, shard := range h.shards {
		go shard.run()
		log.Printf("分片 %d 启动成功", i)
	}

	// 启动Redis消息订阅
	go h.subscribeRedis()

	// 处理全局广播
	for {
		select {
		case message := <-h.broadcast:
			h.handleBroadcast(message)
		}
	}
}
```

```go
// 启动分片运行
func (s *Shard) run() {
	for {
		select {
		case client := <-s.register:
			// 注册新客户端
			s.mu.Lock()
			s.clients[client] = true
			s.mu.Unlock()
			log.Printf("客户端注册成功，分片客户端数: %d", len(s.clients))

		case client := <-s.unregister:
			// 注销客户端
			s.mu.Lock()
			if _, ok := s.clients[client]; ok {
				delete(s.clients, client)
				close(client.send) // 关闭发送通道
			}
			s.mu.Unlock()
			log.Printf("客户端注销成功，分片客户端数: %d", len(s.clients))

		case message := <-s.broadcast:
			// 分片内广播消息
			s.mu.RLock()
			for client := range s.clients {
				select {
				case client.send <- message:
					// 消息成功发送
				default:
					// 客户端发送队列满，主动断开
					close(client.send)
					delete(s.clients, client)
				}
			}
			s.mu.RUnlock()
		}
	}
}
```
2. 所有客户端启动 监听msg通道
```go
// 客户端读取泵（处理接收消息）
func (c *Client) readPump() {
	defer func() {
		c.hub.unregisterClient(c)
		c.conn.Close()
	}()

	// 设置消息大小限制和超时
	c.conn.SetReadLimit(512 * 1024) // 最大512KB
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("读取错误: %v", err)
			}
			break
		}

		// 处理业务消息
		c.handleMessage(message)
	}
}

// 客户端写入泵（处理发送消息）
func (c *Client) writePump() {
	ticker := time.NewTicker(50 * time.Second) // 心跳间隔
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// 通道关闭，发送关闭消息
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// 使用NextWriter进行流式写入
			writer, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			writer.Write(message)

			// 批量发送缓冲区的其他消息
			n := len(c.send)
			for i := 0; i < n; i++ {
				writer.Write(<-c.send)
			}

			if err := writer.Close(); err != nil {
				return
			}

		case <-ticker.C:
			// 发送心跳
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
```