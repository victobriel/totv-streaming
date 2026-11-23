// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"sort"
	"sync"
	"time"
)

// ring keeps the last N durations (ms) for percentiles.
type ring struct {
	mu  sync.Mutex
	v   []float64
	pos int
	n   int
}

func newRing(n int) *ring { return &ring{v: make([]float64, n)} }
func (r *ring) add(d time.Duration) {
	r.mu.Lock()
	r.v[r.pos] = float64(d) / float64(time.Millisecond)
	r.pos = (r.pos + 1) % len(r.v)
	if r.n < len(r.v) {
		r.n++
	}
	r.mu.Unlock()
}

type pct struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

func (r *ring) pct() pct {
	r.mu.Lock()
	c := append([]float64(nil), r.v[:r.n]...)
	r.mu.Unlock()
	if len(c) == 0 {
		return pct{}
	}
	sort.Float64s(c)
	at := func(q float64) float64 { return c[int(q*float64(len(c)-1))] }
	return pct{N: len(c), P50: at(0.5), P95: at(0.95), Max: c[len(c)-1]}
}
