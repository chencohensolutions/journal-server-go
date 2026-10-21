package media

import "sync"

// keyedMutex is a mutex per key, dropped again when nobody holds or waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	lock, ok := k.locks[key]
	if !ok {
		lock = &keyedLock{}
		k.locks[key] = lock
	}
	lock.refs++
	k.mu.Unlock()

	lock.Lock()
	return func() {
		lock.Unlock()
		k.mu.Lock()
		if lock.refs--; lock.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
