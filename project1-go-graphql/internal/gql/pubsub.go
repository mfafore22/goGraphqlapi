package gql

import (
	"sync"

	"taskflow/internal/model"
)

// TaskPubSub is a tiny in-memory pub/sub used by the update endpoints to
// notify long-polling clients that a task has changed.
//
// We originally sketched this for GraphQL subscriptions, but graphql-go does
// not ship with a subscription transport, so the HTML frontend uses a
// long-poll endpoint (see server.go: /api/events) that is backed by this
// same pub/sub. The design note in the README discusses why.
//
// PRODUCTION CAVEATS:
//   - In-memory: will not fan out across multiple server instances.
//   - No persistence: a slow client that disconnects briefly will miss events.
//   - For a real system: Redis pub/sub, NATS, or Postgres LISTEN/NOTIFY.
type TaskPubSub struct {
	mu   sync.RWMutex
	subs map[string]map[chan *model.Task]struct{}
}

func NewTaskPubSub() *TaskPubSub {
	return &TaskPubSub{subs: make(map[string]map[chan *model.Task]struct{})}
}

func (p *TaskPubSub) Subscribe(projectID string) (<-chan *model.Task, func()) {
	// Buffer of 8 lets a client briefly lag without us dropping events
	// immediately. Tuned small — if you see drops in practice, increase.
	ch := make(chan *model.Task, 8)

	p.mu.Lock()
	if p.subs[projectID] == nil {
		p.subs[projectID] = make(map[chan *model.Task]struct{})
	}
	p.subs[projectID][ch] = struct{}{}
	p.mu.Unlock()

	// Cleanup runs when the caller is done. It removes the channel from
	// the subscriber set and closes it so receivers observe "done".
	cleanup := func() {
		p.mu.Lock()
		if set := p.subs[projectID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(p.subs, projectID)
			}
		}
		p.mu.Unlock()
		close(ch)
	}
	return ch, cleanup
}

// Publish delivers a task update to every subscriber of the given project.
// Non-blocking send: if a channel's buffer is full we skip that subscriber.
// Tradeoff: a very slow consumer may miss events, which is acceptable here
// (the frontend refetches state periodically anyway). For a trading-grade
// system you'd want either a larger buffer, backpressure, or per-subscriber
// goroutines with persistent queues.
func (p *TaskPubSub) Publish(projectID string, t *model.Task) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for ch := range p.subs[projectID] {
		select {
		case ch <- t:
		default:
			// Drop. Logged at the application level in a real deployment.
		}
	}
}
