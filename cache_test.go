package cache

import (
	"sync"
	"testing"
	"time"
)

func TestCache(t *testing.T) {
	count := 0
	cache := NewCache(func() (string, error) {
		count++
		return "Hello, World!", nil
	}, 10*time.Second)

	cache.Get()
	cache.Get()
	cache.Get()
	if count != 1 {
		t.Errorf("Expected count to be 1, got %d", count)
	}
}

func TestCache_Race(t *testing.T) {
	count := 0
	cache := NewCache(func() (string, error) {
		count++
		return "Hello, World!", nil
	}, 10*time.Second)

	go cache.Get()
	go cache.Get()
	go cache.Get()
	cache.Get()
	if count != 1 {
		t.Errorf("Expected count to be 1, got %d", count)
	}
}

func TestCache_Expire(t *testing.T) {
	var wg sync.WaitGroup
	cache := NewCache(func() (string, error) {
		wg.Done()
		return "Hello, World!", nil
	}, 10*time.Millisecond)

	wg.Add(2)
	go func() {
		for {
			cache.Get()
		}
	}()
	wg.Wait()
}

func BenchmarkTestCache(b *testing.B) {
	cache := NewCache(func() (string, error) {
		return "Hello, World!", nil
	}, 10*time.Microsecond)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Get()
	}
}

type listener struct {
	f func(string, error)
}

func (l *listener) OnError(err error) {
	l.f("", err)
}
func (l *listener) OnValue(v string) {
	l.f(v, nil)
}

func TestRefreshingCache(t *testing.T) {
	var wg sync.WaitGroup
	cache := NewRefreshingCache(func() (string, error) {
		return "Hello, World!", nil
	}, 10*time.Millisecond, RefreshingListener[string](&listener{
		f: func(v string, err error) {
			wg.Done()
		},
	}))

	wg.Add(1)
	cache.Start()
	wg.Wait()
	cache.Stop()
}
