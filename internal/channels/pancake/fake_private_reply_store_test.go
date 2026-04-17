package pancake

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakePrivateReplyStore is an in-memory test double for
// store.PancakePrivateReplyStore. Keyed by (tenantID, pageID, senderID).
//
// Honors store.WithTenantID(ctx) like the real impls so tests exercise tenant
// scoping without a DB. Pass SkipTenantCheck=true to emulate
// non-tenant-seeded test contexts (use carefully — real impls fail-closed).
type fakePrivateReplyStore struct {
	mu              sync.Mutex
	rows            map[string]time.Time
	wasSentErr      error
	markSentErr     error
	tryClaimErr     error
	unclaimErr      error
	deleteErr       error
	SkipTenantCheck bool
	markSentCalls   int
	wasSentCalls    int
	tryClaimCalls   int
	unclaimCalls    int
	deleteExpCalls  int
}

func newFakePrivateReplyStore() *fakePrivateReplyStore {
	return &fakePrivateReplyStore{rows: map[string]time.Time{}}
}

func (f *fakePrivateReplyStore) key(ctx context.Context, pageID, senderID string) (string, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil && !f.SkipTenantCheck {
		return "", errors.New("fakePrivateReplyStore: missing tenant_id")
	}
	return tenantID.String() + "|" + pageID + "|" + senderID, nil
}

func (f *fakePrivateReplyStore) WasSent(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wasSentCalls++
	if f.wasSentErr != nil {
		return false, f.wasSentErr
	}
	k, err := f.key(ctx, pageID, senderID)
	if err != nil {
		return false, err
	}
	ts, ok := f.rows[k]
	if !ok {
		return false, nil
	}
	if ttl > 0 && time.Since(ts) > ttl {
		return false, nil
	}
	return true, nil
}

func (f *fakePrivateReplyStore) MarkSent(ctx context.Context, pageID, senderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markSentCalls++
	if f.markSentErr != nil {
		return f.markSentErr
	}
	k, err := f.key(ctx, pageID, senderID)
	if err != nil {
		return err
	}
	f.rows[k] = time.Now()
	return nil
}

func (f *fakePrivateReplyStore) TryClaim(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tryClaimCalls++
	if f.tryClaimErr != nil {
		return false, f.tryClaimErr
	}
	k, err := f.key(ctx, pageID, senderID)
	if err != nil {
		return false, err
	}
	ts, exists := f.rows[k]
	if exists && ttl > 0 && time.Since(ts) <= ttl {
		return false, nil // fresh row — claim denied
	}
	// Insert (new) or refresh (stale) — caller wins the claim.
	f.rows[k] = time.Now()
	return true, nil
}

func (f *fakePrivateReplyStore) Unclaim(ctx context.Context, pageID, senderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unclaimCalls++
	if f.unclaimErr != nil {
		return f.unclaimErr
	}
	k, err := f.key(ctx, pageID, senderID)
	if err != nil {
		return err
	}
	delete(f.rows, k)
	return nil
}

func (f *fakePrivateReplyStore) DeleteExpired(ctx context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteExpCalls++
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	var n int64
	for k, ts := range f.rows {
		if !ts.After(olderThan) {
			delete(f.rows, k)
			n++
		}
	}
	return n, nil
}

// stats snapshot for assertions.
func (f *fakePrivateReplyStore) stats() (was, mark, del int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wasSentCalls, f.markSentCalls, f.deleteExpCalls
}
