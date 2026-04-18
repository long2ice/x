package traffic

import (
	"sync"
	"time"
)

// ttlNoExpiration matches cache.NoExpiration (-1) so callers can keep using
// the same sentinel when migrating from patrickmn/go-cache.
const ttlNoExpiration = time.Duration(-1)

// ttlItem mirrors cache.Item: Object + Expiration (unix nanos, 0 = never).
type ttlItem struct {
	Object     any
	Expiration int64
}

func (i *ttlItem) expired(now int64) bool {
	return i.Expiration > 0 && now > i.Expiration
}

// ttlMap is a drop-in replacement for *cache.Cache that avoids the per-instance
// janitor goroutine.  Expired entries are removed lazily on Get/Items access
// and eagerly by callers of evictExpired (hooked into the existing reload
// ticker).  With 2e4 trafficLimiter instances this saves ~8e4 janitor
// goroutines plus their 30s-wakeup storm.
type ttlMap struct {
	m sync.Map // string -> *ttlItem
}

func newTTLMap() *ttlMap {
	return &ttlMap{}
}

func (c *ttlMap) Get(key string) (any, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return nil, false
	}
	it := v.(*ttlItem)
	if it.expired(time.Now().UnixNano()) {
		c.m.CompareAndDelete(key, v)
		return nil, false
	}
	return it.Object, true
}

func (c *ttlMap) Set(key string, val any, ttl time.Duration) {
	var exp int64
	if ttl > 0 {
		exp = time.Now().Add(ttl).UnixNano()
	}
	c.m.Store(key, &ttlItem{Object: val, Expiration: exp})
}

func (c *ttlMap) Delete(key string) {
	c.m.Delete(key)
}

func (c *ttlMap) Flush() {
	c.m.Range(func(k, _ any) bool {
		c.m.Delete(k)
		return true
	})
}

// Items returns a snapshot of non-expired entries.  Expired entries are
// removed eagerly during the walk.
func (c *ttlMap) Items() map[string]*ttlItem {
	out := make(map[string]*ttlItem)
	now := time.Now().UnixNano()
	c.m.Range(func(k, v any) bool {
		it := v.(*ttlItem)
		if it.expired(now) {
			c.m.CompareAndDelete(k, v)
			return true
		}
		out[k.(string)] = it
		return true
	})
	return out
}

// evictExpired drops expired entries without returning a snapshot.  Call from
// a long-running reload loop instead of a dedicated janitor goroutine.
func (c *ttlMap) evictExpired() {
	now := time.Now().UnixNano()
	c.m.Range(func(k, v any) bool {
		if v.(*ttlItem).expired(now) {
			c.m.CompareAndDelete(k, v)
		}
		return true
	})
}
