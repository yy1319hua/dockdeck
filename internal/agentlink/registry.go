// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"sync"

	"github.com/coder/websocket"
)

// Registry 维护 server_id → 活跃反向连接。
//
// 并发模型：Attach/Detach/Conn 都是短临界区；连接自身的读写各有独立 goroutine，
// 不在锁内做任何 I/O。
type Registry struct {
	mu       sync.RWMutex
	byServer map[int64]*Conn
	max      int
}

// NewRegistry 构造注册表。max<=0 时用默认上限 512。
func NewRegistry(max int) *Registry {
	if max <= 0 {
		max = 512
	}
	return &Registry{byServer: make(map[int64]*Conn), max: max}
}

// Attach 接管一条已握手完成的连接。
//
// 顶号语义：同一 server_id 已有连接时，新连接直接替换，旧连接收到 goaway 后被关闭。
// 这比拒绝新连接更实用——Agent 重启/网络切换后旧连接常常是僵尸，拒绝会让运维必须等超时。
func (r *Registry) Attach(serverID int64, ws *websocket.Conn, hello Frame) (*Conn, error) {
	r.mu.Lock()
	if len(r.byServer) >= r.max {
		if _, replacing := r.byServer[serverID]; !replacing {
			r.mu.Unlock()
			_ = ws.Close(websocket.StatusTryAgainLater, "hub reverse-link limit reached")
			return nil, ErrTooMany
		}
	}
	old := r.byServer[serverID]
	c := newConn(serverID, ws, hello)
	r.byServer[serverID] = c
	r.mu.Unlock()

	if old != nil {
		old.GoAway("replaced", "同一服务器建立了更新的连接")
	}
	go func() {
		<-c.Done()
		r.Detach(c)
	}()
	return c, nil
}

// Conn 返回某服务器的活跃连接；没有则 nil（调用方据此回落 HTTP）。
func (r *Registry) Conn(serverID int64) *Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byServer[serverID]
}

// Detach 仅在当前登记的就是这条连接时才删除——
// 否则会把「顶号后的新连接」误删，导致明明在线却回落 HTTP。
func (r *Registry) Detach(c *Conn) {
	r.mu.Lock()
	if cur := r.byServer[c.ServerID()]; cur == c {
		delete(r.byServer, c.ServerID())
	}
	r.mu.Unlock()
}

// Count 返回当前活跃连接数。
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byServer)
}

// CloseAll 关闭所有连接（进程退出/回滚开关用）。
func (r *Registry) CloseAll() {
	r.mu.Lock()
	conns := make([]*Conn, 0, len(r.byServer))
	for _, c := range r.byServer {
		conns = append(conns, c)
	}
	r.byServer = make(map[int64]*Conn)
	r.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}
