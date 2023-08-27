# cache

Experimental generic cache library

## Usage

```go
	f := func() (string, error) {
		return "Hello, World!", nil
	}
	cache := NewCache(f, 10*time.Second)

	cache.Get()
	cache.Get()
	cache.Get()
  // f is called only once
```