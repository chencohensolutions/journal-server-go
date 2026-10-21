package media

import (
	"fmt"
	"log"
	"slices"
	"time"
)

// Library is the stores of all users that have a data folder.
type Library struct {
	stores map[string]*Store // by username
}

// OpenLibrary opens a store for each username → data folder.
func OpenLibrary(folders map[string]string, now func() time.Time) (*Library, error) {
	library := &Library{stores: map[string]*Store{}}
	for user, folder := range folders {
		store, err := OpenStore(folder, now)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		library.stores[user] = store
	}
	return library, nil
}

// Store is nil when the user has no data folder configured.
func (l *Library) Store(user string) *Store {
	if l == nil {
		return nil
	}
	return l.stores[user]
}

// Users are the usernames that have a store, sorted.
func (l *Library) Users() []string {
	users := make([]string, 0, len(l.stores))
	for user := range l.stores {
		users = append(users, user)
	}
	slices.Sort(users)
	return users
}

// PurgeExpiredUploads deletes unfinished uploads that were not continued in time.
func (l *Library) PurgeExpiredUploads() {
	for user, store := range l.stores {
		if err := store.PurgeExpiredUploads(); err != nil {
			log.Printf("media: purging expired uploads of %s: %v", user, err)
		}
	}
}

// TrimPreviews applies the limit to every user's store.
func (l *Library) TrimPreviews(maxBytes int64) {
	for user, store := range l.stores {
		if err := store.TrimPreviews(maxBytes); err != nil {
			log.Printf("media: trimming previews of %s: %v", user, err)
		}
	}
}

// RefreshIndexCaches brings every user's .cache/index.json up to date, so the next start is quick.
func (l *Library) RefreshIndexCaches() {
	for user, store := range l.stores {
		if err := store.RefreshIndexCache(); err != nil {
			log.Printf("media: refreshing the index cache of %s: %v", user, err)
		}
	}
}
