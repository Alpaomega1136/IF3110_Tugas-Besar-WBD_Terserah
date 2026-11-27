package metrics

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type poolTotals struct {
	active, idle, pools         int64
	acquires, empty, canceled   int64
	acquireSeconds, waitSeconds float64
}

func (a *poolTotals) add(s *pgxpool.Stat, live bool) {
	if live {
		a.active += int64(s.AcquiredConns())
		a.idle += int64(s.IdleConns())
		a.pools++
	}
	a.acquires += s.AcquireCount()
	a.empty += s.EmptyAcquireCount()
	a.canceled += s.CanceledAcquireCount()
	a.acquireSeconds += s.AcquireDuration().Seconds()
	a.waitSeconds += s.EmptyAcquireWaitTime().Seconds()
}

var poolMu sync.Mutex
var livePools = make(map[*Pool]struct{})
var closedTotals [2]poolTotals
var hijacked int64
var roles = [...]string{"metadata", "target"}

type Pool struct {
	*pgxpool.Pool
	role int
	once sync.Once
}

func TrackPool(pool *pgxpool.Pool, role string) *Pool {
	i := 0
	if role == "target" {
		i = 1
	}
	p := &Pool{Pool: pool, role: i}
	poolMu.Lock()
	livePools[p] = struct{}{}
	poolMu.Unlock()
	return p
}
func (p *Pool) Close() {
	p.once.Do(func() {
		p.Pool.Close()
		poolMu.Lock()
		closedTotals[p.role].add(p.Stat(), false)
		delete(livePools, p)
		poolMu.Unlock()
	})
}

func TrackHijacked() func() {
	poolMu.Lock()
	hijacked++
	poolMu.Unlock()
	return func() { poolMu.Lock(); hijacked--; poolMu.Unlock() }
}

func writePools(b *bytes.Buffer) {
	poolMu.Lock()
	totals, guards := closedTotals, hijacked
	for p := range livePools {
		totals[p.role].add(p.Stat(), true)
	}
	poolMu.Unlock()
	header(b, "terserah_db_pool_connections", "gauge", "Currently acquired or idle pgxpool connections by role; excludes hijacked connections.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pool_connections{role=%q,state=\"active\"} %d\nterserah_db_pool_connections{role=%q,state=\"idle\"} %d\n", roles[i], t.active, roles[i], t.idle)
	}
	header(b, "terserah_db_target_hijacked_connections", "gauge", "Target advisory-lock guard connections outside pgxpool.")
	fmt.Fprintf(b, "terserah_db_target_hijacked_connections %d\n", guards)
	header(b, "terserah_db_pools", "gauge", "Open tracked pools by role.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pools{role=%q} %d\n", roles[i], t.pools)
	}
	header(b, "terserah_db_pool_acquires_total", "counter", "Successful pool acquisitions including closed pools.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pool_acquires_total{role=%q} %d\n", roles[i], t.acquires)
	}
	header(b, "terserah_db_pool_empty_acquires_total", "counter", "Successful acquisitions that waited for an empty pool.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pool_empty_acquires_total{role=%q} %d\n", roles[i], t.empty)
	}
	header(b, "terserah_db_pool_canceled_acquires_total", "counter", "Canceled acquisitions.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pool_canceled_acquires_total{role=%q} %d\n", roles[i], t.canceled)
	}
	header(b, "terserah_db_pool_acquire_seconds_total", "counter", "Cumulative successful acquisition duration, including closed pools.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pool_acquire_seconds_total{role=%q} %g\n", roles[i], t.acquireSeconds)
	}
	header(b, "terserah_db_pool_wait_seconds_total", "counter", "Cumulative successful empty-pool acquisition wait, including resource creation.")
	for i, t := range totals {
		fmt.Fprintf(b, "terserah_db_pool_wait_seconds_total{role=%q} %g\n", roles[i], t.waitSeconds)
	}
}
