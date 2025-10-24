package main

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"log"
	"sync"
	"time"
)

// Client 客户端连接结构
type Client struct {
	hub       *Hub
	conn      *websocket.Conn
	send      chan []byte  // 发送消息缓冲通道
	userID    string       // 用户标识
	sessionID string       // 会话标识
	mu        sync.RWMutex // 客户端级别锁
}

// 客户端读取泵（处理接收消息）
func (c *Client) readPump() {
	defer func() {
		c.hub.unregisterClient(c) // 注销此客户端
		c.conn.Close()
	}()

	// 设置消息大小限制和超时[3]
	c.conn.SetReadLimit(512 * 1024) // 最大512KB
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error { // 收到pong怎么处理
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

// 处理客户端消息
func (c *Client) handleMessage(message []byte) {
	var msg map[string]interface{}
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Printf("消息解析失败: %v", err)
		return
	}

	// 根据消息类型处理
	switch msg["type"] {
	case "ping":
		//c.sendPong() 根据业务改变
	case "chat":
		//c.broadcastMessage(message)	根据业务改变
		// 其他业务消息类型...
	}
}
