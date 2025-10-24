package main

import (
	"log"
	"sync"
)

// Shard 分片管理器
type Shard struct {
	clients    map[*Client]bool // 当前分片客户端集合
	register   chan *Client     // 注册通道
	unregister chan *Client     // 注销通道
	broadcast  chan []byte      // 分片内广播通道
	mu         sync.RWMutex     // 分片级别锁
}

// NewShard 新建分片
func NewShard() *Shard {
	return &Shard{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 1000), // 缓冲1000条消息
	}
}

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
