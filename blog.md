
今回は、singleflightを参考にして、Thundering Herd問題を解決したキャッシュライブラリを書いてみたので記事にしてみました。

以下の流れで書いていきます。

1. 初めに
2. singleflightを使って実装してみる
3. signleflightの中の実装を見てみる
4. singleflightを参考にキャッシュライブラリを変更する
5. 時間でexpireするような実装を入れてみる
6. race condition を解決する前に callとresultを分離する
7. race condition を 解決する
8. 終わりに

なお、今回作ったパッケージは、実験的に実装したもので、本番での稼働を考慮したものではありません。ご注意ください。

## 1. 初めに

アプリケーション内で、シンプルなキャッシュの機構が欲しくなり、実験的に実装してみました。
Goの場合、キャッシュライブラリは色々公開されており、選択肢も多いのですが、大規模なメモリを意識した実装やLRUなどEvictionの実装が含まれたものが多いです。
ただ、今回欲しかったのでは、複雑な実装は必要ではなく、単一の値をキャッシュするだけの機構が欲しくなったので自分で実装してみます。

また、Goでは、キャッシュのThundering Herd問題のためによく使われる[singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight)ですが
singleflightでは、[singleflight.Group#Do](https://pkg.go.dev/golang.org/x/sync/singleflight#Group.Do) のシグネチャにも現れている通り
keyというインターフェースが存在します。
これは、keyごとに並行性を制御するものですが、今回の要件では、キャッシュkeyによるキャッシュ分散の必要性が存在しません。
そこで、今回は singleflightの実装を参考にしつつ、singleflightを使わずに実装してみます。

今回実装するインターフェースとしては以下になります。
キャッシュしたい値とerrorをただ返すインターフェースです。
一応、genericsを使って、使いやすい形にしてあります。

```go
type Cache[T any] interface {
	Get() (T, error)
}
```

## 2. singleflightを使って実装してみる

先ほどはsingleflightは使わないと言ったものの、ひとまず、singleflightを使って実装してみましょう。

とりあえず、singleflight.Groupをフィールドに生やします。
また、NewCacheのようなインターフェースを考えているので、値を取ってくる関数fをフィールドに追加します。


```go
type cache[T any] struct {
	f func() (T, error)
	group singleflight.Group
}

func NewCache[T](f func() (T, error)) Cache {
	return &cache{
		f: f,
	}
}
```

上の構造体の構成では不十分なのですが、Getを実装してみましょう。
以下のような実装になりました。
keyは1つしかないような状況なので、固定で空文字を入れてあります。

```go
func (c *cache[T]) Get() (T, error) {
	v, err, _ := c.group.Do("", func() (any, error) {
		return c.f()
	})
	if err != nil {
		return v.(T), err
	}
	return v.(T), nil
}
```

さて、現状の実装だと、cacheといいつつ、一回取った値をキャッシュしていません。
ここでcacheのフィールドにキャッシュをしてみます。
ここでは、実装が楽になるのでvalueとerrを持つresultを追加し、その構造体のポインタをcacheに生やしてみました。

```go
type result[T any] struct {
	value T
	err error
}

type cache[T any] struct {
	res *result[T]
	f func() (T, error)
	group singleflight.Group
}
```

先ほどのgetの実装でresフィールドを確認して値があれば、返却するような実装にしてみます。
なければ、singleflightで保護された区間で値を取りつつ、resフィールドを更新してみます。
こんな感じになりました。

```go
func (c *cache[T]) Get() (T, error) {
	if c.res != nil {
		return c.res.value, c.res.err
	}

	v, err, _ := c.group.Do("", func() (any, error) {
		value, err := c.f()
		c.res = &result{
			value: value,
			err: err
		}
		return value, err
	})
	if err != nil {
		return v.(T), err
	}
	return v.(T), nil
}
```

ただ、この実装では問題があります。c.resの参照において race conditionが発生する可能性があります。
そこで、sync.Mutexを使って排他制御を入れてみます。

```go
type cache[T any] struct {
	mu sync.Mutex
	res *result[T]
	f func() (T, error)
	group singleflight.Group
}

func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	if c.res != nil {
		return c.res.value, c.res.err
	}
	c.mu.Unlock()

	v, err, _ := c.group.Do("", func() (any, error) {
		value, err := c.f()
		c.mu.Lock()
		c.res = &result{
			value: value,
			err: err
		}
		c.mu.Unlock()
		return value, err
	})
	if err != nil {
		return v.(T), err
	}
	return v.(T), nil
}
```

ひとまずこれで、初期段階のsingleflightを使ったキャッシュライブラリの実装が終わりました。

## 3. signleflightの中の実装を見てみる

初期実装が終わったのですが、ここで、`singleflight.group#Do` の中の実装を見てみます。以下にコードを示します。
何やらGroup構造体のmというmapに、keyに対応する値があるかどうかで分岐しています。

```go
func (g *Group) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	// keyに対応する値があったらその値を待ち受けて戻り値を返している
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()

		if e, ok := c.err.(*panicError); ok {
			panic(e)
		} else if c.err == errGoexit {
			runtime.Goexit()
		}
		return c.val, c.err, true
	}
	// keyに対応する値がない場合
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	g.doCall(c, key, fn)
	return c.val, c.err, c.dups > 0
}
```

ここで、mというmapには、[call](https://cs.opensource.google/go/x/sync/+/master:singleflight/singleflight.go;l=47;drc=8fcdb60fdcc0539c5e357b2308249e4e752147f1)という構造体を保持しています。
この構造体を簡略化して示したのが以下のコードです。

先ほど初期実装で書いたresultに似ています。

```go
// singleflight.Doで渡した関数の呼び出しを抽象化する構造体
type call struct {
	// 呼び出しが終わったらWaitが終わるWaitGroup
	wg sync.WaitGroup

	val interface{}
	err error
}
```

では、ここで最初に見た `singleflight.group#Do` を簡略化して読んでみましょう。
以下のようなコードになるはずです。
Lockもエラーハンドリングも要らなさそうなところを外してみます。

```go
func (g *Group) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	g.mu.Lock()
	// keyに対応する値があったらその値を待ち受けて戻り値を返している
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		// callが終わるまで待ち受ける
		c.wg.Wait()

		return c.val, c.err, true
	}
	// keyに対応する値がない場合
	c := new(call)
	// callが終わっていないのでAdd(1)をして表現する
	c.wg.Add(1)
	// 新しいcallをmに保存
	g.m[key] = c
	g.mu.Unlock()

	g.doCall(c, key, fn) // 内部で c.wg.Done()が呼ばれる
	return c.val, c.err, c.dups > 0
}
```

これでだいぶ見やすくなったのではないでしょうか？
keyに対応するcallがあれば、そのcallを取り出して、callが終わるのを待って値を返却する
keyに対応するcallがなければ、新たに追加して、callを実行する。
それだけです。

## 4. singleflightを参考にキャッシュライブラリを変更する

では、先ほど見たsingleflightの実装を参考にキャッシュライブラリを変更してみます。
新たに、callという構造体を追加します。

```go
type result[T any] struct {
	value T
	err error
}

type call[T any] struct {
	wg	sync.WaitGroup
	res result[T]
}

type cache[T any] struct {
	mu sync.Mutex
	call *call[T]
	f func() (T, error)
}
```

Getを書き換えます。こんな感じになりました。

```go
func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	if call := c.call; call != nil {
		c.mu.Unlock()
		call.wg.Wait()
		return call.res.value, call.res.err
	}

	call := new(call[T])
	call.wg.Add(1)
	c.call = call
	c.mu.Unlock()

	call.res.value, call.res.err = c.f()
	call.wg.Done()
	return call.res.value, call.res.err
}
```

singleflightが消えて、少しスッキリしました（ほんまか？）。
次に行きましょう。

## 5. 一定時間でexpireする実装を入れてみる

さて、現状のソースがこんな感じになっています。

```go
type result[T any] struct {
	value T
	err error
}

type call[T any] struct {
	wg	sync.WaitGroup
	res result[T]
}

type cache[T any] struct {
	mu sync.Mutex
	call *call[T]
	f func() (T, error)
}

func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	if call := c.call; call != nil {
		c.mu.Unlock()
		call.wg.Wait()
		return call.res.value, call.res.err
	}

	call := new(call[T])
	call.wg.Add(1)
	c.call = call
	c.mu.Unlock()

	call.res.value, call.res.err = c.f()
	call.wg.Done()
	return call.res.value, call.res.err
}
```

まず、refresh関数を作って、callを生成する処理を外に出してみます。

```go
func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	call := c.call
	if call == nil {
		return refresh()
	}
	c.mu.Unlock()
	call.wg.Wait()
	return call.value, call.err
}

func (c *cache[T]) refresh() (T, error) {
	call := new(call[T])
	call.wg.Add(1)
	c.call = call
	c.mu.Unlock()

	call.value, call.err = c.f()
	call.wg.Done()
	return call.value, call.err
}
```

次に、キャッシュのexpire時間をcallに追加して、Getに条件分岐を足して、refreshにはexpireを計算する処理を追加します。

```go
type result[T any] struct {
	value T
	err error
}

type call[T any] struct {
	wg	sync.WaitGroup
	res result[T]
	expire time.Time
}

type cache[T any] struct {
	mu sync.Mutex
	call *call[T]
	f func() (T, error)
}

func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	call := c.call
	if call == nil {
		return refresh()
	} else if time.Now().After(call.res.expire) {
		return refresh()
	}
	c.mu.Unlock()
	call.wg.Wait()
	return call.res.value, call.res.err
}

func (c *cache[T]) refresh() (T, error) {
	call := new(call[T])
	call.wg.Add(1)
	c.call = call
	c.mu.Unlock()

	call.res.value, call.res.err = c.f()
	call.res.expire = time.Now().Add(30 * time.Second) // TODO: 後でcacheのfieldに入れる
	call.wg.Done()
	return call.res.value, call.res.err
}
```

さて、どうでしょうか。それっぽく見えるとは思います。
が、これは動きません。

これは、なぜでしょうか。
Get関数でcall.res.expireを見ていますが、callが終了して、expireに値が設定されるのは
call.wg.Wait()で待った後です。
つまり、race conditionが発生しています。

```go
func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	call := c.call
	if call == nil {
		return refresh()
	} else if time.Now().After(call.res.expire) {
		return refresh()
	}
	c.mu.Unlock()
	call.wg.Wait()
	return call.res.value, call.res.err
}
```

これは、どうすれば良いでしょうか。
これを解決するために、少し構造を変えてみます。

## 6. race condition を解決する前に callとresultを分離する

callの構造体は、呼び出しを待ち受けるWaitGroupと
それに付随する、resultとも言うべき、計算結果を持っていました。
そこに、expireというキャッシュのexpireする時間を追加しました。

では、ここでcallとresultを分離解決します。この後の変更の都合上、ここではresultにexpireを移しています。

```go
type result[T any] struct {
	value	T
	err		error
	expire time.Time
}

type call[T any] struct {
	wg sync.WaitGroup
}
```

では、この分離した結果を cache構造体にフィードバックします。

```go
type cache[T any] struct {
	mu sync.Mutex
	call *call[T]
	result *result[T]
	f func() (T, error)
}
```

では、これらを踏まえて、race conditionを直してみましょう。

## 7. race condition を 解決する

まず、resultとcallを分離したので、Get関数を見直します。
cache構造体に生えているresultの値を基に、必要があればrefreshするというシンプルな構造になり、WaitGroup周りのコードが消えました。

```go
func (c *cache[T]) Get() (T, error) {
	c.mu.Lock()
	res := c.result
	if res == nil {
		return refresh()
	} else if time.Now().After(res.expire) {
		return refresh()
	}
	c.mu.Unlock()
	return res.value, res.err
}
```

では、今度はrefresh関数を見直してみます。

callがあれば、その呼び出しを待って、結果を返し
callがなければ、新しい呼び出しを開始する形にします。
呼び出しが終わった後に、callをnilにresetする処理も忘れずに入れておきます。

```go
func (c *cache[T]) refresh() (T, error) {
	if call := c.call; call != nil {
		c.mu.Unlock()
		call.wg.Wait()
		return c.result.value, c.result.err
	}

	call := new(call[T])
	call.wg.Add(1)
	c.call = call
	c.mu.Unlock()

	var res result[T]
	res.value, res.err = c.f()
	res.expire = time.Now().Add(30 * time.Second) // TODO: durationは後でcacheのfieldに入れる

	c.mu.Lock()
	c.call = nil
	c.result = &res
	c.mu.Unlock()

	call.wg.Done()
	return call.value, call.err
}
```

どうでしょう。これでrace conditionが治ったはずです。
そして、欲しかった単一の値をキャッシュするライブラリが出来ました。

## 8. 終わりに

ここまでで、Thundering Herdを解決しつつ単一の値をキャッシュするライブラリが出来ました。
singleflightを参考にしつつ、書いていきましたが
expireを入れた結果、singleflightとは少し違う構造になりました。

実際には、testを書いて -raceでrace conitionが発生していないか、テストしながら書いていたのですが
実はブログに書いた内容は最終的に整理したものを書いており、途中では試行錯誤して書いていました。
mutexが必要な並行処理を普段書いていないのもあって、ガッツリ並行処理を書いた感じがして、少し楽しくなりました。

現状の実装では、エラーの扱いが甘い実装になっていますが
単一の値をキャッシュするライブラリとしては、使えるような形になったのではないかと思います。
皆さんもキャッシュライブラリを書いてみてはどうでしょうか。

今回は、ここまで。