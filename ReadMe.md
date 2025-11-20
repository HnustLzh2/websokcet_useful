# WebSocket 高可用配置

一个支持高并发、多分片、集群部署、断线重连的 WebSocket 服务器实现。

## 核心特性

- ✅ **多分片架构**：通过分片机制提高并发处理能力
- ✅ **集群支持**：通过 Redis 实现多实例消息同步
- ✅ **断线重连**：自动暂存离线消息，重连后自动推送
- ✅ **多连接支持**：同一用户可打开多个标签页/设备，消息同步推送
- ✅ **消息队列**：使用 Redis 存储离线消息（最多100条，30分钟过期）
- ✅ **心跳机制**：自动检测连接状态，保持连接活跃
- ✅ **优雅关闭**：支持优雅关闭和资源清理

## 架构设计

### Hub 中心分片管理器
```go
// Hub 中心管理器（支持多分片）
type Hub struct {
	shards      []*Shard                    // 分片数组
	broadcast   chan BroadcastMessage       // 全局广播通道
	redis       *redis.Client               // Redis客户端用于集群通信
	ctx         context.Context             // 上下文
	userClients map[string]map[*Client]bool // 用户ID到客户端集合的映射（支持多连接）
	mu          sync.RWMutex                // 保护userClients的锁
}
```

**说明**：
- `shards`: 分片数组，用于负载均衡
- `broadcast`: 全局广播通道，接收需要广播的消息
- `redis`: Redis 客户端，用于集群间消息同步和离线消息队列
- `userClients`: 用户连接映射，支持同一用户的多个连接（多标签页/多设备）
- `mu`: 保护用户连接映射的读写锁

### Shard 分片（Hub 中的一个小处理器）
```go
// Shard 分片管理器
type Shard struct {
	clients    map[*Client]bool // 当前分片客户端集合
	register   chan *Client      // 注册通道
	unregister chan *Client      // 注销通道
	broadcast  chan []byte       // 分片内广播通道
	mu         sync.RWMutex      // 分片级别锁
}
```

**说明**：
- 每个 shard 都有一组活跃的客户端集合
- 一个用于添加新用户的注册通道
- 一个用于注销用户的通道
- 一个单分片内的广播通道
- 一个分片锁用于保护并发访问

### Client 客户端（最底层的设计）
```go
// Client 客户端连接结构
type Client struct {
	hub       *Hub            // hub管理器
	conn      *websocket.Conn // 一个websocket连接
	send      chan []byte     // 发送消息缓冲通道
	userID    string          // 用户标识
	sessionID string          // 会话标识
}
```

**说明**：
- 每个客户端都有一个 hub 管理器，用于使用全局广播
- 一个 conn 用于发送和接收消息
- 一个 send 发送消息的缓冲通道（256条消息）
- 用户标识和此 websocket 会话标识（用于断线重连）

## 执行流程

### 1. 初始化

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

### 2. Hub 启动流程

```go
// 启动中心管理器
func (h *Hub) run() {
	// 启动所有分片
	for i, shard := range h.shards {
		go shard.run()
		log.Printf("分片 %d 启动成功", i)
	}

	// 启动Redis消息订阅（用于集群通信）
	go h.subscribeRedis()

	// 处理全局广播（持续监听直到 channel 关闭）
	for message := range h.broadcast {
		h.handleBroadcast(message)
	}
	log.Println("广播通道已关闭，Hub 停止运行")
}
```

### 3. Shard 运行流程

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

### 4. 客户端连接处理

```go
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

	// 发送连接确认消息（包含sessionID，用于前端重连标识）
	connectMsg := map[string]interface{}{
		"type":       "connected",
		"session_id": client.sessionID,
		"user_id":    userID,
		"timestamp":  time.Now().Unix(),
	}
	msgBytes, _ := json.Marshal(connectMsg)
	client.send <- msgBytes

	log.Printf("客户端连接成功: %s (session: %s)", userID, client.sessionID)
}
```

### 5. 客户端读写泵

```go
// 客户端读取泵（处理接收消息）
func (c *Client) readPump() {
	defer func() {
		c.hub.unregisterClient(c) // 注销此客户端
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

## 核心功能详解

### 1. 多连接支持（多标签页/多设备）

**实现原理**：
- 使用 `map[string]map[*Client]bool` 存储每个用户的所有连接
- 当用户打开多个标签页时，每个连接都会注册到该用户的连接集合中
- 发送消息时，会向该用户的所有连接广播

**使用场景**：
- 用户在同一浏览器打开多个标签页
- 用户在不同设备上登录（手机、电脑等）
- 所有连接都会收到相同的消息

### 2. 断线重连支持

**实现原理**：
1. **消息队列**：用户离线时，消息存入 Redis 队列（`websocket:queue:{userID}`）
   - 最多保存 100 条消息
   - 30 分钟过期时间
   
2. **重连检测**：当用户从离线（0个连接）变为在线（1个连接）时：
   - 自动从 Redis 获取待处理消息
   - 向该用户的所有连接发送待处理消息
   - 获取后清空队列，避免重复发送

3. **连接确认**：连接成功后发送包含 `sessionID` 的确认消息
   - 前端可以保存 `sessionID` 用于标识会话

**前端配合示例**：
```javascript
let ws = null;
let sessionId = null;
let reconnectAttempts = 0;

function connect() {
  const userId = getUserId();
  ws = new WebSocket(`ws://localhost:8080/ws?user_id=${userId}`);
  
  ws.onopen = () => {
    reconnectAttempts = 0;
    console.log('WebSocket连接成功');
  };
  
  ws.onmessage = (event) => {
    const msg = JSON.parse(event.data);
    if (msg.type === 'connected') {
      sessionId = msg.session_id; // 保存sessionID
      console.log('收到连接确认，session:', sessionId);
    }
    // 处理其他消息...
  };
  
  ws.onclose = () => {
    console.log('连接断开，准备重连...');
    reconnect();
  };
  
  ws.onerror = (error) => {
    console.error('WebSocket错误:', error);
  };
}

function reconnect() {
  if (reconnectAttempts >= 10) {
    console.error('达到最大重连次数');
    return;
  }
  
  const delay = Math.min(1000 * Math.pow(2, reconnectAttempts), 30000);
  setTimeout(() => {
    reconnectAttempts++;
    console.log(`第 ${reconnectAttempts} 次重连...`);
    connect();
  }, delay);
}
```

### 3. Redis 集群通信

**实现原理**：
- 当某个实例需要广播消息时，会发布到 Redis 的 `websocket:broadcast` 频道
- 所有实例都订阅该频道，收到消息后在自己的实例内广播
- 实现跨实例的消息同步

**工作流程**：
```
用户A (实例1) 发送消息
    ↓
实例1: handleBroadcast() → 发送给本地客户端
    ↓
实例1: redis.Publish() → Redis
    ↓
Redis 广播给所有订阅者
    ↓
实例2: subscribeRedis() 收到消息 → 发送给用户B
实例3: subscribeRedis() 收到消息 → 发送给用户C
```

**注意**：当前实现中，从 Redis 收到的消息会再次调用 `handleBroadcast()`，这会导致消息再次发布到 Redis。如果需要避免循环，可以添加消息来源标识。

### 4. 消息路由

支持三种消息类型：

```go
type BroadcastMessage struct {
	Type      string      `json:"type"`       // 消息类型：broadcast, user, room
	Data      interface{} `json:"data"`       // 消息数据
	TargetIDs []string    `json:"target_ids"` // 目标用户ID列表
	RoomID    string      `json:"room_id"`    // 房间ID
}
```

- **broadcast**: 全局广播，发送给所有连接的客户端
- **user**: 定向发送，发送给指定的用户ID列表
- **room**: 房间广播（需要扩展房间管理功能）

## API 端点

### WebSocket 连接
```
ws://localhost:8080/ws?user_id={userID}
```

### 健康检查
```
GET /health
```
返回：
```json
{
  "status": "healthy",
  "timestamp": 1234567890,
  "clients": 100,
  "shards": 4
}
```

### 统计信息
```
GET /stats
```
返回：
```json
{
  "total_clients": 100,
  "shard_stats": {
    "0": 25,
    "1": 25,
    "2": 25,
    "3": 25
  },
  "timestamp": 1234567890
}
```

## 配置说明

### Redis 配置
- 地址：通过 `NewHub(shardCount, redisAddr)` 传入
- 密码：生产环境应在代码中设置
- 维护通知：已禁用，避免旧版 Redis 不支持 `maint_notifications` 的警告

### 分片策略
- 使用简单的哈希分片：`shardIndex = userID[0] % len(shards)`
- **注意**：运行时 shard 数量不能变化，userID 不能变化

### 消息队列配置
- 最大消息数：100 条
- 过期时间：30 分钟
- 存储位置：Redis List (`websocket:queue:{userID}`)

## 性能优化

1. **分片机制**：将客户端分散到多个分片，减少锁竞争
2. **批量发送**：`writePump` 中批量发送缓冲区消息，减少网络调用
3. **缓冲通道**：使用缓冲通道（256条消息）避免阻塞
4. **心跳机制**：50秒心跳间隔，保持连接活跃
5. **压缩支持**：启用 WebSocket 压缩，减少带宽

## 注意事项

1. **分片数量**：运行时不能改变分片数量，否则会导致用户路由错误
2. **Redis 版本**：建议使用 Redis 6.0+，旧版本需要禁用维护通知
3. **消息队列**：离线消息最多保存 100 条，超过会被丢弃
4. **连接限制**：单个用户理论上可以建立无限个连接，但建议设置合理限制
5. **集群通信**：当前实现可能存在消息循环问题，生产环境需要添加消息来源标识

## 扩展建议

1. **房间管理**：实现房间功能，支持群组聊天
2. **消息持久化**：将重要消息持久化到数据库
3. **限流保护**：添加连接数和消息频率限制
4. **监控告警**：添加 Prometheus 指标和告警
5. **消息去重**：添加消息ID，避免重复发送
