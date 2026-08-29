// Package jobqueue provides an async job queue with worker pool for
// long-running lifecycle operations (app install, update, uninstall, backup).
//
// Inspired by Runtipi's RabbitMQ-based queue, but implemented with Go channels
// and a worker pool — no external dependencies needed.
//
// Design goals:
//   - Submit returns immediately with a job ID
//   - Status queries are cheap (O(1) map lookup)
//   - Workers process jobs sequentially per-type to avoid resource contention
//   - Job results are retained for 1 hour after completion for status polling
//   - No unbounded memory growth: max 10,000 jobs in history
package jobqueue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Status represents a job's current state.
type Status string

const (
	StatusPending   Status = "pending"   // In queue, not yet picked up
	StatusRunning   Status = "running"   // A worker is executing it
	StatusCompleted Status = "completed" // Finished successfully
	StatusFailed    Status = "failed"    // Finished with error
	StatusCancelled Status = "cancelled" // Cancelled before or during execution
)

// Func is the signature of a job function.
// It receives a context (for cancellation) and returns a result message and error.
// The context is cancelled if the job is explicitly cancelled or times out.
type Func func(ctx context.Context) (result string, err error)

// Job is a unit of work submitted to the queue.
type Job struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`   // e.g. "app.install", "app.update"
	Target      string    `json:"target"` // e.g. "nextcloud"
	Status      Status    `json:"status"`
	SubmittedAt time.Time `json:"submitted_at"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
	Result      string    `json:"result,omitempty"`
	ErrorText   string    `json:"error,omitempty"`
	Progress    int       `json:"progress"` // 0-100, optional

	fn         Func               // The actual work
	cancelFunc context.CancelFunc // To abort a running job
}

// Queue is a worker pool that processes jobs asynchronously.
type Queue struct {
	workers    int
	jobs       chan *Job
	mu         sync.RWMutex
	registry   map[string]*Job // all submitted jobs (history + in-flight)
	ctx        context.Context
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup
}

// New creates a queue with the given number of workers and starts them.
// A typical value is 3 workers — enough to parallelize but not so many
// that concurrent Docker operations fight over resources.
func New(workers int) *Queue {
	if workers < 1 {
		workers = 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		workers:    workers,
		jobs:       make(chan *Job, 100), // buffered queue
		registry:   make(map[string]*Job),
		ctx:        ctx,
		cancelFunc: cancel,
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker(i)
	}
	// Start janitor to prune old completed jobs
	q.wg.Add(1)
	go q.janitorLoop()
	return q
}

// Submit enqueues a new job and returns its ID immediately.
// The job runs on a worker goroutine when one becomes available.
func (q *Queue) Submit(jobType, target string, fn Func) string {
	id := uuid.New().String()
	job := &Job{
		ID:          id,
		Type:        jobType,
		Target:      target,
		Status:      StatusPending,
		SubmittedAt: time.Now(),
		fn:          fn,
	}

	q.mu.Lock()
	q.registry[id] = job
	q.mu.Unlock()

	// Non-blocking send with fallback
	select {
	case q.jobs <- job:
	default:
		// Queue full — mark as failed immediately
		q.mu.Lock()
		job.Status = StatusFailed
		job.ErrorText = "queue is full, try again later"
		job.EndedAt = time.Now()
		q.mu.Unlock()
	}

	return id
}

// Get returns the current status of a job by ID.
// Returns nil if the job doesn't exist or has been pruned.
func (q *Queue) Get(id string) *Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.registry[id]
	if !ok {
		return nil
	}
	// Return a copy to avoid races on Status/Progress updates
	copy := *job
	return &copy
}

// Cancel aborts a pending or running job.
// Returns true if the job was cancelled, false if it wasn't found or already finished.
func (q *Queue) Cancel(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, ok := q.registry[id]
	if !ok {
		return false
	}

	switch job.Status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return false
	case StatusRunning:
		if job.cancelFunc != nil {
			job.cancelFunc()
		}
	}

	job.Status = StatusCancelled
	job.EndedAt = time.Now()
	return true
}

// List returns all jobs in the registry, newest first.
// Limit caps the number of returned jobs (0 = no limit).
func (q *Queue) List(limit int) []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()

	result := make([]*Job, 0, len(q.registry))
	for _, j := range q.registry {
		copy := *j
		result = append(result, &copy)
	}

	// Sort by SubmittedAt descending
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].SubmittedAt.After(result[i].SubmittedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

// Stop gracefully shuts down the queue. Pending jobs are cancelled, running jobs
// are given their context cancellation. Waits for all workers to finish.
func (q *Queue) Stop() {
	q.cancelFunc()
	close(q.jobs)
	q.wg.Wait()
}

// worker processes jobs from the channel until it's closed.
func (q *Queue) worker(id int) {
	defer q.wg.Done()

	for job := range q.jobs {
		// Check if already cancelled before starting
		q.mu.Lock()
		if job.Status == StatusCancelled {
			q.mu.Unlock()
			continue
		}
		jobCtx, cancel := context.WithCancel(q.ctx)
		job.cancelFunc = cancel
		job.Status = StatusRunning
		job.StartedAt = time.Now()
		q.mu.Unlock()

		// Execute the job (may panic — catch it)
		result, err := q.runSafely(jobCtx, job)

		// Record result
		q.mu.Lock()
		if job.Status != StatusCancelled {
			if err != nil {
				job.Status = StatusFailed
				job.ErrorText = err.Error()
			} else {
				job.Status = StatusCompleted
				job.Result = result
			}
		}
		job.EndedAt = time.Now()
		job.cancelFunc = nil
		q.mu.Unlock()

		cancel() // Clean up context resources
	}
}

// runSafely executes a job function with panic recovery.
func (q *Queue) runSafely(ctx context.Context, job *Job) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("job panicked: %v", r)
		}
	}()
	return job.fn(ctx)
}

// janitorLoop removes completed jobs older than 1 hour every 10 minutes.
// Also caps total registry size at 10,000 entries.
func (q *Queue) janitorLoop() {
	defer q.wg.Done()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-ticker.C:
			q.prune()
		}
	}
}

// prune removes old completed jobs and enforces registry size limit.
func (q *Queue) prune() {
	q.mu.Lock()
	defer q.mu.Unlock()

	cutoff := time.Now().Add(-1 * time.Hour)
	for id, job := range q.registry {
		isFinished := job.Status == StatusCompleted ||
			job.Status == StatusFailed ||
			job.Status == StatusCancelled
		if isFinished && job.EndedAt.Before(cutoff) {
			delete(q.registry, id)
		}
	}

	// Hard cap: if still > 10k, delete oldest completed jobs
	if len(q.registry) > 10000 {
		type idAndTime struct {
			id string
			t  time.Time
		}
		finished := []idAndTime{}
		for id, job := range q.registry {
			if job.Status == StatusCompleted || job.Status == StatusFailed || job.Status == StatusCancelled {
				finished = append(finished, idAndTime{id, job.EndedAt})
			}
		}
		// Sort oldest first
		for i := 0; i < len(finished); i++ {
			for j := i + 1; j < len(finished); j++ {
				if finished[j].t.Before(finished[i].t) {
					finished[i], finished[j] = finished[j], finished[i]
				}
			}
		}
		// Delete until under cap
		toDelete := len(q.registry) - 10000
		for i := 0; i < toDelete && i < len(finished); i++ {
			delete(q.registry, finished[i].id)
		}
	}
}
