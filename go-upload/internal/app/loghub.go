package app

import (
	"sync"

	"github.com/gorilla/websocket"
)

type LogHub struct {
	mu    sync.Mutex
	conns map[string]map[*websocket.Conn]struct{}
}

func NewLogHub() *LogHub {
	return &LogHub{conns: make(map[string]map[*websocket.Conn]struct{})}
}

func (h *LogHub) Add(jobID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[jobID] == nil {
		h.conns[jobID] = make(map[*websocket.Conn]struct{})
	}
	h.conns[jobID][conn] = struct{}{}
}

func (h *LogHub) Remove(jobID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[jobID] == nil {
		return
	}
	delete(h.conns[jobID], conn)
}

func (h *LogHub) Broadcast(jobID string, message string) {
	h.mu.Lock()
	conns := h.conns[jobID]
	h.mu.Unlock()
	for conn := range conns {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(message))
	}
}
