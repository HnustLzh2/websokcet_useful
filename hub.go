package main

import (
	"context"
	"encoding/json"
	"github.com/redis/go-redis/v9"
	"log"
)

// Hub 中心管理器（支持多分片）
type Hub struct {
	shards    []*Shard              // 分片数组
	broadcast chan BroadcastMessage // 全局广播通道
	redis     *redis.Client         // Redis客户端用于集群通信
	ctx       context.Context       // 上下文
}

// NewHub 新建中心管理器
func NewHub(shardCount int, redisAddr string) *Hub {
	hub := &Hub{
		shards:    make([]*Shard, shardCount),
		broadcast: make(chan BroadcastMessage, 10000), // 全局广播缓冲区
		ctx:       context.Background(),
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

	// 处理全局广播
	for {
		select {
		case message := <-h.broadcast:
			h.handleBroadcast(message)
		}
	}
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

// 定向发送给特定用户
func (h *Hub) sendToUsers(userIDs []string, message []byte) {
	for _, shard := range h.shards {
		shard.mu.RLock()
		for client := range shard.clients {
			for _, targetID := range userIDs {
				if client.userID == targetID {
					select {
					case client.send <- message:
					default:
						// 客户端发送队列满
						close(client.send)
						delete(shard.clients, client)
					}
					break
				}
			}
		}
		shard.mu.RUnlock()
	}
}

// 注册客户端到合适的分片 通过userID和shard长度的求余 但是前提是运行的时候 shard长度不能发送变化 userID不能发生变化
func (h *Hub) registerClient(client *Client) {
	// 简单哈希分片策略
	shardIndex := int(client.userID[0]) % len(h.shards)
	h.shards[shardIndex].register <- client
}

// 注销客户端
func (h *Hub) unregisterClient(client *Client) {
	shardIndex := int(client.userID[0]) % len(h.shards)
	h.shards[shardIndex].unregister <- client
}
