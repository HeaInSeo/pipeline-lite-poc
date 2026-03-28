//go:build redis

// Package queue provides a Redis Streams-backed durable queue candidate.
//
// This file is behind the `redis` build tag because it requires a live Redis
// server. It is not compiled in the default PoC build.
//
// WHY Redis Streams over PostgreSQL at this stage:
//   - No schema migrations needed: Redis Streams are schemaless
//   - Consumer groups provide built-in pending/ack/reclaim semantics
//   - Horizontally scalable: multiple consumer groups can read the same stream
//   - Low operational overhead for PoC: single Redis instance, no RDBMS setup
//   - XPENDING + XCLAIM give exactly-once delivery guarantees without a WAL
//
// WHY NOT PostgreSQL at this stage:
//   - Schema migrations add friction to a 10-day PoC
//   - Polling SELECT ... FOR UPDATE or LISTEN/NOTIFY adds complexity
//   - PostgreSQL is the right production choice but over-engineered for validation
//
// Trade-offs vs JsonRunStore:
//   - Redis Streams: durable (AOF/RDB), consumer groups, XPENDING reclaim
//   - JsonRunStore: simpler, zero dependencies, file-based, single-process only
//
// ASSUMPTION: production uses Redis Streams (or Kafka) for the release queue
// and PostgreSQL for persistent run state (replacing JsonRunStore).
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRunQueue is a durable run queue backed by Redis Streams with consumer groups.
//
// Stream layout:
//
//	stream key:     "poc:runs"
//	consumer group: "poc-workers"
//	message fields: run_id (string), payload (JSON-encoded RunMessage)
//
// Guarantees:
//   - At-least-once delivery: XACK removes from pending; unacked messages
//     are reclaimed by XCLAIM after a visibility timeout.
//   - Fan-out: multiple consumers can be added to the group.
//   - Restart recovery: XPENDING shows messages delivered but not acked.
type RedisRunQueue struct {
	client    *redis.Client
	stream    string
	group     string
	consumer  string
	blockTime time.Duration
}

// RunMessage is the payload stored in each stream entry.
type RunMessage struct {
	RunID     string    `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
}

var ErrStreamEmpty = errors.New("redis stream: no messages available")

// NewRedisRunQueue creates a RedisRunQueue connected to addr.
// The consumer group is created if it does not exist.
//
// addr: Redis address, e.g. "localhost:6379"
// stream: stream key, e.g. "poc:runs"
// group: consumer group name, e.g. "poc-workers"
// consumer: this consumer's name (unique per process/pod)
func NewRedisRunQueue(ctx context.Context, addr, stream, group, consumer string) (*RedisRunQueue, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connect %s: %w", addr, err)
	}

	q := &RedisRunQueue{
		client:    client,
		stream:    stream,
		group:     group,
		consumer:  consumer,
		blockTime: 2 * time.Second,
	}

	// Create consumer group. MKSTREAM creates the stream if absent.
	// BUSYGROUP is returned if the group already exists — that is OK.
	err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return nil, fmt.Errorf("redis XGROUP CREATE %s/%s: %w", stream, group, err)
	}

	return q, nil
}

// Enqueue adds a run message to the stream via XADD.
// Returns the stream entry ID on success.
func (q *RedisRunQueue) Enqueue(ctx context.Context, runID string) (string, error) {
	msg := RunMessage{RunID: runID, CreatedAt: time.Now()}
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}

	id, err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		Values: map[string]any{
			"run_id":  runID,
			"payload": string(payload),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("redis XADD %s: %w", q.stream, err)
	}
	return id, nil
}

// Consume reads the next undelivered message for this consumer group via XREADGROUP.
// It blocks for up to blockTime if no message is immediately available.
// Returns ErrStreamEmpty if the stream has no new messages.
//
// The caller must call Ack(ctx, id) after processing to remove the message
// from the pending entry list (PEL).
func (q *RedisRunQueue) Consume(ctx context.Context) (entryID string, msg RunMessage, err error) {
	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.group,
		Consumer: q.consumer,
		Streams:  []string{q.stream, ">"},
		Count:    1,
		Block:    q.blockTime,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", RunMessage{}, ErrStreamEmpty
		}
		return "", RunMessage{}, fmt.Errorf("redis XREADGROUP: %w", err)
	}

	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return "", RunMessage{}, ErrStreamEmpty
	}

	entry := streams[0].Messages[0]
	payloadStr, ok := entry.Values["payload"].(string)
	if !ok {
		return "", RunMessage{}, fmt.Errorf("redis: message %s has no payload field", entry.ID)
	}

	if err := json.Unmarshal([]byte(payloadStr), &msg); err != nil {
		return "", RunMessage{}, fmt.Errorf("redis: unmarshal payload: %w", err)
	}

	return entry.ID, msg, nil
}

// Ack acknowledges a message (removes from PEL) via XACK.
// Call this after successfully processing the message.
func (q *RedisRunQueue) Ack(ctx context.Context, entryID string) error {
	return q.client.XAck(ctx, q.stream, q.group, entryID).Err()
}

// ReclaimStale redelivers messages that have been in the PEL for longer than
// minIdle without being acked. These are likely from crashed consumers.
//
// Returns the reclaimed messages. Caller should process and ack them.
// This implements the XCLAIM pattern for exactly-once delivery on crashes.
func (q *RedisRunQueue) ReclaimStale(ctx context.Context, minIdle time.Duration) ([]redis.XMessage, error) {
	pending, err := q.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream,
		Group:  q.group,
		Start:  "-",
		End:    "+",
		Count:  100,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis XPENDING: %w", err)
	}

	var staleIDs []string
	for _, p := range pending {
		if p.Idle >= minIdle {
			staleIDs = append(staleIDs, p.ID)
		}
	}
	if len(staleIDs) == 0 {
		return nil, nil
	}

	claimed, err := q.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   q.stream,
		Group:    q.group,
		Consumer: q.consumer,
		MinIdle:  minIdle,
		Messages: staleIDs,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis XCLAIM: %w", err)
	}
	return claimed, nil
}

// Pending returns the count of messages delivered but not yet acked.
// Useful for observability: pending > 0 signals release queue congestion.
func (q *RedisRunQueue) Pending(ctx context.Context) (int64, error) {
	info, err := q.client.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		return 0, fmt.Errorf("redis XPENDING count: %w", err)
	}
	return info.Count, nil
}

// Close closes the Redis client connection.
func (q *RedisRunQueue) Close() error {
	return q.client.Close()
}

// isBusyGroup returns true if err is the Redis BUSYGROUP error,
// which indicates the consumer group already exists.
func isBusyGroup(err error) bool {
	return err != nil && err.Error() == "BUSYGROUP Consumer Group name already exists"
}
