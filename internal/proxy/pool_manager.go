package proxy

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"
)

// PoolManager manages connection pools for multiple backends.
type PoolManager struct {
	config      PoolConfig
	dialTimeout time.Duration
	logger      *slog.Logger

	mu    sync.RWMutex
	pools map[string]*BackendPool
}

// NewPoolManager creates a new pool manager.
func NewPoolManager(cfg PoolConfig, dialTimeout time.Duration, logger *slog.Logger) *PoolManager {
	return &PoolManager{
		config:      cfg,
		dialTimeout: dialTimeout,
		logger:      logger.With("component", "pool-manager"),
		pools:       make(map[string]*BackendPool),
	}
}

// Acquire gets a connection to the specified backend.
// Creates a new pool for the backend if one doesn't exist.
func (pm *PoolManager) Acquire(ctx context.Context, address string) (net.Conn, error) {
	pool := pm.getOrCreatePool(address)
	return pool.Acquire(ctx)
}

// Release returns a connection to its pool.
func (pm *PoolManager) Release(address string, conn net.Conn) {
	pm.mu.RLock()
	pool, ok := pm.pools[address]
	pm.mu.RUnlock()

	if ok {
		pool.Release(conn)
	} else {
		// Pool was removed, just close the connection.
		conn.Close()
	}
}

// Discard closes a connection without returning it to the pool.
func (pm *PoolManager) Discard(address string, conn net.Conn) {
	pm.mu.RLock()
	pool, ok := pm.pools[address]
	pm.mu.RUnlock()

	if ok {
		pool.Discard(conn)
	} else {
		conn.Close()
	}
}

// RemovePool removes and closes the pool for a backend.
func (pm *PoolManager) RemovePool(address string) {
	pm.mu.Lock()
	pool, ok := pm.pools[address]
	if ok {
		delete(pm.pools, address)
	}
	pm.mu.Unlock()

	if ok {
		pool.Close()
		pm.logger.Info("removed pool", "backend", address)
	}
}

// Close closes all pools.
func (pm *PoolManager) Close() {
	pm.mu.Lock()
	pools := pm.pools
	pm.pools = make(map[string]*BackendPool)
	pm.mu.Unlock()

	for _, pool := range pools {
		pool.Close()
	}
	pm.logger.Info("closed all pools")
}

// Stats returns statistics for all pools.
func (pm *PoolManager) Stats() map[string]map[string]any {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	stats := make(map[string]map[string]any, len(pm.pools))
	for addr, pool := range pm.pools {
		stats[addr] = pool.Stats()
	}
	return stats
}

// PoolCount returns the number of active pools.
func (pm *PoolManager) PoolCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.pools)
}

// getOrCreatePool gets or creates a pool for the given address.
func (pm *PoolManager) getOrCreatePool(address string) *BackendPool {
	// Fast path: check if pool exists.
	pm.mu.RLock()
	pool, ok := pm.pools[address]
	pm.mu.RUnlock()

	if ok {
		return pool
	}

	// Slow path: create pool.
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Double-check after acquiring write lock.
	if pool, ok := pm.pools[address]; ok {
		return pool
	}

	pool = NewBackendPool(address, pm.config, pm.dialTimeout, pm.logger)
	pm.pools[address] = pool
	pm.logger.Info("created pool", "backend", address)

	return pool
}

// DrainAll drains all pools gracefully.
func (pm *PoolManager) DrainAll(timeout time.Duration) {
	pm.mu.Lock()
	pools := make([]*BackendPool, 0, len(pm.pools))
	for _, pool := range pm.pools {
		pools = append(pools, pool)
	}
	pm.mu.Unlock()

	// Close all pools concurrently.
	var wg sync.WaitGroup
	for _, pool := range pools {
		wg.Add(1)
		go func(p *BackendPool) {
			defer wg.Done()
			p.Close()
		}(pool)
	}

	// Wait with timeout.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		pm.logger.Info("all pools drained")
	case <-time.After(timeout):
		pm.logger.Warn("pool drain timeout")
	}
}

// UpdateConfig updates the pool configuration.
// Note: This only affects newly created pools. Existing pools are not modified.
func (pm *PoolManager) UpdateConfig(cfg PoolConfig) {
	pm.mu.Lock()
	pm.config = cfg
	pm.mu.Unlock()
	pm.logger.Info("pool configuration updated",
		"max_size", cfg.MaxPoolSize,
		"min_idle", cfg.MinIdleConns,
	)
}

// GetConfig returns the current pool configuration.
func (pm *PoolManager) GetConfig() PoolConfig {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.config
}

// IsEnabled returns whether pooling is enabled.
func (pm *PoolManager) IsEnabled() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.config.Enabled
}
