package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Hub 中心管理器（支持多分片）
type Hub struct {
	shards      []*Shard              // 分片数组
	broadcast   chan BroadcastMessage // 全局广播通道
	redis       *redis.Client         // Redis客户端用于集群通信
	ctx         context.Context       // 上下文
	userClients map[string]*Client    // 用户ID到客户端的映射（用于断线重连）
	mu          sync.RWMutex          // 保护userClients的锁
}

// NewHub 新建中心管理器
func NewHub(shardCount int, redisAddr string) *Hub {
	hub := &Hub{
		shards:      make([]*Shard, shardCount),
		broadcast:   make(chan BroadcastMessage, 10000), // 全局广播缓冲区
		ctx:         context.Background(),
		userClients: make(map[string]*Client), // 初始化用户连接映射
	}

	// 初始化Redis客户端
	hub.redis = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: "", // 生产环境设置密码
		DB:       0,
	})

	// 初始化分片
	for i := 0; i < shardCount; i++ {
		hub.shards[i] = NewShard()
	}

	return hub
}

// 启动中心管理器
func (h *Hub) run() {
	// 启动所有分片
	for i, shard := range h.shards {
		go shard.run()
		log.Printf("分片 %d 启动成功", i)
	}

	// 启动Redis消息订阅
	go h.subscribeRedis()

	// 处理全局广播（持续监听直到 channel 关闭）
	for message := range h.broadcast {
		h.handleBroadcast(message)
	}
	log.Println("广播通道已关闭，Hub 停止运行")
}

// 处理广播消息
func (h *Hub) handleBroadcast(msg BroadcastMessage) {
	messageBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("消息序列化失败: %v", err)
		return
	}

	// 根据消息类型路由
	switch msg.Type {
	case "broadcast":
		// 全局广播到所有分片
		for _, shard := range h.shards {
			select {
			case shard.broadcast <- messageBytes:
			default:
				log.Println("分片广播通道已满，消息丢弃")
			}
		}

	case "user":
		// 定向发送给特定用户
		h.sendToUsers(msg.TargetIDs, messageBytes)

	case "room":
		// 发送到特定房间（需要扩展房间管理）
		//h.sendToRoom(msg.RoomID, messageBytes)
	}

	// 发布到Redis供其他实例消费
	h.redis.Publish(h.ctx, "websocket:broadcast", messageBytes)
}

// Redis订阅处理集群消息
func (h *Hub) subscribeRedis() {
	pubsub := h.redis.Subscribe(h.ctx, "websocket:broadcast")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for msg := range ch {
		var broadcastMsg BroadcastMessage
		if err := json.Unmarshal([]byte(msg.Payload), &broadcastMsg); err != nil {
			log.Printf("Redis消息解析失败: %v", err)
			continue
		}

		// 处理来自其他实例的广播消息
		h.handleBroadcast(broadcastMsg)
	}
}

// 定向发送给特定用户（支持断线重连消息队列）
func (h *Hub) sendToUsers(userIDs []string, message []byte) {
	for _, userID := range userIDs {
		h.mu.RLock()
		client, isOnline := h.userClients[userID]
		h.mu.RUnlock()

		if isOnline && client != nil {
			// 用户在线，直接发送
			select {
			case client.send <- message:
				// 消息成功发送
			default:
				// 客户端发送队列满，将消息存入队列
				log.Printf("用户 %s 发送队列满，消息存入队列", userID)
				h.queueMessage(userID, message)
			}
		} else {
			// 用户离线，消息存入队列等待重连
			log.Printf("用户 %s 离线，消息存入队列", userID)
			h.queueMessage(userID, message)
		}
	}
}

// 将消息存入Redis队列（用于断线重连）
func (h *Hub) queueMessage(userID string, message []byte) {
	key := "websocket:queue:" + userID
	// 使用Redis List存储消息，最多保存100条，过期时间30分钟
	h.redis.LPush(h.ctx, key, message)
	h.redis.LTrim(h.ctx, key, 0, 99) // 只保留最近100条消息
	h.redis.Expire(h.ctx, key, 30*time.Minute)
}

// 获取并清空用户的消息队列
func (h *Hub) getQueuedMessages(userID string) [][]byte {
	key := "websocket:queue:" + userID
	messages, err := h.redis.LRange(h.ctx, key, 0, -1).Result()
	if err != nil {
		log.Printf("获取消息队列失败: %v", err)
		return nil
	}

	// 获取后删除队列
	h.redis.Del(h.ctx, key)

	// 转换为字节数组（消息是JSON字符串）
	result := make([][]byte, len(messages))
	for i, msg := range messages {
		result[i] = []byte(msg)
	}
	return result
}

// 注册客户端到合适的分片 通过userID和shard长度的求余 但是前提是运行的时候 shard长度不能发送变化 userID不能发生变化
func (h *Hub) registerClient(client *Client) {
	// 检查是否已有该用户的连接
	h.mu.Lock()
	oldClient, exists := h.userClients[client.userID]
	if exists && oldClient != nil {
		// 关闭旧连接
		log.Printf("检测到用户 %s 的旧连接，正在关闭", client.userID)
		go func() {
			// 在goroutine中关闭，避免阻塞
			oldClient.conn.Close()
			close(oldClient.send)
		}()
		// 从分片中注销旧客户端
		shardIndex := int(oldClient.userID[0]) % len(h.shards)
		select {
		case h.shards[shardIndex].unregister <- oldClient:
		default:
			// 如果通道已满，直接删除
			h.shards[shardIndex].mu.Lock()
			delete(h.shards[shardIndex].clients, oldClient)
			h.shards[shardIndex].mu.Unlock()
		}
	}
	// 注册新客户端
	h.userClients[client.userID] = client
	h.mu.Unlock()

	// 简单哈希分片策略
	shardIndex := int(client.userID[0]) % len(h.shards)
	h.shards[shardIndex].register <- client

	// 获取并发送待处理的消息
	go func() {
		queuedMessages := h.getQueuedMessages(client.userID)
		if len(queuedMessages) > 0 {
			log.Printf("用户 %s 重连，发送 %d 条待处理消息", client.userID, len(queuedMessages))
			// 按顺序发送队列中的消息
			for _, msg := range queuedMessages {
				select {
				case client.send <- msg:
				case <-time.After(5 * time.Second):
					log.Printf("发送待处理消息超时，用户: %s", client.userID)
					return
				}
			}
		}
	}()
}

// 注销客户端
func (h *Hub) unregisterClient(client *Client) {
	// 从用户映射中删除
	h.mu.Lock()
	if h.userClients[client.userID] == client {
		delete(h.userClients, client.userID)
	}
	h.mu.Unlock()

	// 从分片中注销
	shardIndex := int(client.userID[0]) % len(h.shards)
	h.shards[shardIndex].unregister <- client
}
