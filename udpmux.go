// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"net"
	"strconv"
	"sync"

	"github.com/pion/ice/v4"
)

// sharedMux is ONE UDP socket + ICE mux per (iceHost, TOTV_STREAM_UDP_PORT) for the whole process. Several sessions cannot bind the same fixed port
// each, so they all take the same mux: pion demultiplexes the datagrams by the ICE ufrag (STUN username) and by the remote address learned from it,
// so every PeerConnection of every session only ever sees its own traffic. The socket is opened by the first session that needs it and closed when the
// last one releases it (refs).
type sharedMux struct {
	key  string
	conn net.PacketConn
	mux  ice.UDPMux
	refs int
}

type muxPool struct {
	mu sync.Mutex
	m  map[string]*sharedMux
}

// acquire returns the mux listening on ip:port, opening it if no session holds one. A failed bind (port taken by another process) is an error.
func (p *muxPool) acquire(ip string, port int) (*sharedMux, error) {
	key := net.JoinHostPort(ip, strconv.Itoa(port))
	p.mu.Lock()
	defer p.mu.Unlock()
	if sm := p.m[key]; sm != nil {
		sm.refs++
		return sm, nil
	}
	conn, err := net.ListenPacket("udp4", key)
	if err != nil {
		return nil, err
	}
	if p.m == nil {
		p.m = map[string]*sharedMux{}
	}
	sm := &sharedMux{key: key, conn: conn, mux: ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}), refs: 1}
	p.m[key] = sm
	return sm, nil
}

// release drops one reference; the last one closes the mux and the socket.
func (p *muxPool) release(sm *sharedMux) {
	if sm == nil {
		return
	}
	p.mu.Lock()
	sm.refs--
	last := sm.refs <= 0
	if last {
		delete(p.m, sm.key)
	}
	p.mu.Unlock()
	if last {
		_ = sm.mux.Close()
		_ = sm.conn.Close()
	}
}

// open is the number of sockets currently held (tests, /health).
func (p *muxPool) open() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.m) }
