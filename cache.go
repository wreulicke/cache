package cache

import (
	"sync"
	"time"
)

type result[T any] struct {
	value  T
	err    error
	expire time.Time
}

type call[T any] struct {
	wg   sync.WaitGroup
	done bool
	res  result[T]
}

type Cache[T any] interface {
	Get() (T, error)
	Refresh() (T, error)
}

type cache[T any] struct {
	duration time.Duration
	mu       sync.Mutex
	call     *call[T]
	f        func() (T, error)
}

func NewCache[T any](f func() (T, error), duration time.Duration) Cache[T] {
	return &cache[T]{f: f, duration: duration}
}

func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	call := c.call
	if call == nil {
		return c.refresh()
	} else if call.done && time.Now().After(call.res.expire) {
		return c.refresh()
	}
	c.mu.Unlock()
	call.wg.Wait()

	return call.res.value, call.res.err
}

func (c *cache[T]) Refresh() (T, error) {
	c.mu.Lock()
	return c.refresh()
}

func (c *cache[T]) refresh() (T, error) {
	call := &call[T]{}
	c.call = call
	call.wg.Add(1)
	c.mu.Unlock()

	call.res.value, call.res.err = c.f()
	call.res.expire = time.Now().Add(c.duration)
	call.wg.Done()
	call.done = true
	return call.res.value, call.res.err
}

type RefreshingCache[T any] struct {
	duration time.Duration
	stop     chan struct{}
	cache    Cache[T]
	listener RefreshingListener[T]
}

type RefreshingListener[T any] interface {
	OnError(err error)
	OnValue(v T)
}

func NewRefreshingCache[T any](f func() (T, error), duration time.Duration, listener RefreshingListener[T]) *RefreshingCache[T] {
	cache := NewCache(f, duration)
	return &RefreshingCache[T]{
		cache:    cache,
		stop:     make(chan struct{}),
		duration: duration / 2,
		listener: listener,
	}
}

func (rc *RefreshingCache[T]) Start() {
	tick := time.NewTicker(rc.duration)
	go func() {
		for {
			select {
			case <-tick.C:
				v, err := rc.cache.Refresh()
				if rc.listener != nil {
					if err != nil {
						rc.listener.OnError(err)
					} else {
						rc.listener.OnValue(v)
					}
				}
			case <-rc.stop:
				return
			}
		}
	}()
}

func (rc *RefreshingCache[T]) Stop() {
	close(rc.stop)
}

// Get implements Cache.
func (rc *RefreshingCache[T]) Get() (T, error) {
	return rc.cache.Get()
}

// Refresh implements Cache.
func (rc *RefreshingCache[T]) Refresh() (T, error) {
	return rc.cache.Refresh()
}
