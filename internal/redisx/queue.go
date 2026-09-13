package redisx

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Task represents a background job to be executed.
type Task struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

type TaskHandler func(ctx context.Context, payload []byte) error

// Queue provides asynchronous background job execution backed by Redis List (LPUSH/BRPOP)
// or an in-memory buffered channel fallback.
type Queue struct {
	client    *Client
	handlers  map[string]TaskHandler
	inMemChan chan Task
	stopChan  chan struct{}
	wg        sync.WaitGroup
	mu        sync.RWMutex
}

const defaultRedisQueueKey = "squadup:queue:tasks"

// NewQueue creates a new task queue.
func NewQueue(client *Client) *Queue {
	return &Queue{
		client:    client,
		handlers:  make(map[string]TaskHandler),
		inMemChan: make(chan Task, 500),
		stopChan:  make(chan struct{}),
	}
}

// Register registers a handler function for a given task type.
func (q *Queue) Register(taskType string, handler TaskHandler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[taskType] = handler
}

// Enqueue submits a job for asynchronous execution.
func (q *Queue) Enqueue(ctx context.Context, taskType string, payload any) (string, error) {
	var data []byte
	var err error

	if b, ok := payload.([]byte); ok {
		data = b
	} else {
		data, err = json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("failed to marshal task payload: %w", err)
		}
	}

	task := Task{
		ID:        uuid.NewString(),
		Type:      taskType,
		Payload:   data,
		CreatedAt: time.Now(),
	}

	raw, err := json.Marshal(task)
	if err != nil {
		return "", err
	}

	if q.client != nil && q.client.IsAvailable() {
		if err := q.client.Raw().LPush(ctx, defaultRedisQueueKey, raw).Err(); err != nil {
			log.Printf("[Queue] Redis LPush failed (%v); falling back to in-memory queue", err)
		} else {
			return task.ID, nil
		}
	}

	// In-memory queue fallback
	select {
	case q.inMemChan <- task:
		return task.ID, nil
	default:
		return "", fmt.Errorf("in-memory task queue is full")
	}
}

// Start begins processing tasks in background worker goroutines.
func (q *Queue) Start(ctx context.Context, workerCount int) {
	if workerCount <= 0 {
		workerCount = 2
	}

	for i := 0; i < workerCount; i++ {
		q.wg.Add(1)
		go q.worker(ctx, i)
	}
	log.Printf("[Queue] Started %d background task workers", workerCount)
}

func (q *Queue) worker(ctx context.Context, workerID int) {
	defer q.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-q.stopChan:
			return
		default:
		}

		if q.client != nil && q.client.IsAvailable() {
			res, err := q.client.Raw().BRPop(ctx, 1*time.Second, defaultRedisQueueKey).Result()
			if err != nil {
				if err != redis.Nil && err != context.Canceled {
					time.Sleep(100 * time.Millisecond)
				}
				continue
			}
			if len(res) >= 2 {
				var t Task
				if err := json.Unmarshal([]byte(res[1]), &t); err == nil {
					q.dispatch(ctx, t)
				}
			}
			continue
		}

		// In-memory worker fallback
		select {
		case <-ctx.Done():
			return
		case <-q.stopChan:
			return
		case t := <-q.inMemChan:
			q.dispatch(ctx, t)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (q *Queue) dispatch(ctx context.Context, task Task) {
	q.mu.RLock()
	handler, exists := q.handlers[task.Type]
	q.mu.RUnlock()

	if !exists {
		log.Printf("[Queue] No handler registered for task type: %s", task.Type)
		return
	}

	if err := handler(ctx, task.Payload); err != nil {
		log.Printf("[Queue] Error processing task %s (%s): %v", task.ID, task.Type, err)
	}
}

// Stop stops the background workers.
func (q *Queue) Stop() {
	select {
	case <-q.stopChan:
		// already stopped
	default:
		close(q.stopChan)
	}
	q.wg.Wait()
}
