package server

import (
	"sync"
	"sync/atomic"
)

// Hub is an in-process pub/sub with bounded, lossy per-subscriber buffers.
// Slow SSE clients drop messages instead of blocking writers; clients treat a
// message as "something changed" and refetch.
type Hub struct {
	mu    sync.RWMutex
	subs  map[string]map[*Sub]struct{}
	count atomic.Int64
}

type Sub struct {
	C     chan []byte
	topic string
	hub   *Hub
}

const subBuffer = 8

func NewHub() *Hub { return &Hub{subs: map[string]map[*Sub]struct{}{}} }

func (h *Hub) Subscribe(topic string) *Sub {
	s := &Sub{C: make(chan []byte, subBuffer), topic: topic, hub: h}
	h.mu.Lock()
	if h.subs[topic] == nil {
		h.subs[topic] = map[*Sub]struct{}{}
	}
	h.subs[topic][s] = struct{}{}
	h.mu.Unlock()
	h.count.Add(1)
	return s
}

func (s *Sub) Close() {
	h := s.hub
	h.mu.Lock()
	if m := h.subs[s.topic]; m != nil {
		if _, ok := m[s]; ok {
			delete(m, s)
			h.count.Add(-1)
			if len(m) == 0 {
				delete(h.subs, s.topic)
			}
		}
	}
	h.mu.Unlock()
}

func (h *Hub) Publish(topic string, msg []byte) {
	h.mu.RLock()
	for s := range h.subs[topic] {
		select {
		case s.C <- msg:
		default: // full: drop
		}
	}
	h.mu.RUnlock()
}

func (h *Hub) Subscribers() int { return int(h.count.Load()) }
