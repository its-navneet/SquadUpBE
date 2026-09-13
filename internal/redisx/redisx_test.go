package redisx

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"squadup/backend/internal/config"
)

func TestClientFallbackWhenUnconfigured(t *testing.T) {
	cfg := &config.Config{
		RedisEnabled: false,
	}
	client := New(cfg)
	if client.IsAvailable() {
		t.Fatal("expected client to be marked as unavailable when disabled")
	}

	ctx := context.Background()
	if err := client.Ping(ctx); err == nil {
		t.Fatal("expected error on Ping when redis is unavailable")
	}

	if err := client.Publish(ctx, "test_channel", "hello"); err == nil {
		t.Fatal("expected error on Publish when redis is unavailable")
	}
}

func TestLockerInMemoryFallback(t *testing.T) {
	locker := NewLocker(nil) // nil client tests in-memory fallback
	ctx := context.Background()
	key := "test_lock_key"

	token1, ok1, err := locker.Acquire(ctx, key, 500*time.Millisecond)
	if err != nil || !ok1 || token1 == "" {
		t.Fatalf("expected to acquire lock: err=%v, ok=%v, token=%s", err, ok1, token1)
	}

	// Attempt to acquire same key should fail
	_, ok2, err := locker.Acquire(ctx, key, 500*time.Millisecond)
	if err != nil || ok2 {
		t.Fatalf("expected second acquire to fail while locked: err=%v, ok=%v", err, ok2)
	}

	// Release with wrong token should fail
	relWrong, err := locker.Release(ctx, key, "invalid-token")
	if err != nil || relWrong {
		t.Fatalf("expected release with invalid token to fail")
	}

	// Release with valid token should succeed
	relOK, err := locker.Release(ctx, key, token1)
	if err != nil || !relOK {
		t.Fatalf("expected release to succeed: err=%v, ok=%v", err, relOK)
	}

	// Now acquiring again should succeed
	token3, ok3, err := locker.Acquire(ctx, key, 500*time.Millisecond)
	if err != nil || !ok3 || token3 == "" {
		t.Fatalf("expected to re-acquire after release: err=%v, ok=%v", err, ok3)
	}
	_, _ = locker.Release(ctx, key, token3)
}

func TestCacheInMemoryFallback(t *testing.T) {
	cache := NewCache(nil)
	ctx := context.Background()

	type Item struct {
		Name  string `json:"name"`
		Score int    `json:"score"`
	}

	original := Item{Name: "Striker", Score: 95}
	err := cache.Set(ctx, "item:1", original, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("cache.Set failed: %v", err)
	}

	var fetched Item
	found, err := cache.Get(ctx, "item:1", &fetched)
	if err != nil || !found {
		t.Fatalf("cache.Get failed: found=%v, err=%v", found, err)
	}
	if fetched.Name != "Striker" || fetched.Score != 95 {
		t.Fatalf("unexpected fetched item: %+v", fetched)
	}

	// Delete
	err = cache.Delete(ctx, "item:1")
	if err != nil {
		t.Fatalf("cache.Delete failed: %v", err)
	}

	foundAfterDel, _ := cache.Get(ctx, "item:1", &fetched)
	if foundAfterDel {
		t.Fatal("expected item to not be found after Delete")
	}
}

func TestQueueInMemoryFallback(t *testing.T) {
	queue := NewQueue(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processedCount int32
	queue.Register("test_task", func(c context.Context, payload []byte) error {
		atomic.AddInt32(&processedCount, 1)
		return nil
	})

	queue.Start(ctx, 2)

	_, err := queue.Enqueue(ctx, "test_task", map[string]string{"foo": "bar"})
	if err != nil {
		t.Fatalf("queue.Enqueue failed: %v", err)
	}

	// Give worker time to process
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&processedCount) != 1 {
		t.Fatalf("expected task to be processed, count=%d", atomic.LoadInt32(&processedCount))
	}

	queue.Stop()
}
