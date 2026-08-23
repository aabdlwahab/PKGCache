// Package job runs persisted background work with per-project ordering.
package job

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/obs"
)

// Runner executes one job and appends durable log lines through logf.
type Runner func(context.Context, control.Job, func(string)) error

// Manager owns the bounded worker pool. Jobs for one project run in order; jobs for
// different projects may occupy different workers.
type Manager struct {
	db      *control.DB
	events  *obs.Bus
	ctx     context.Context
	cancel  context.CancelFunc
	sem     chan struct{}
	metrics *obs.Metrics

	mu      sync.Mutex
	queues  map[string][]int64
	active  map[string]bool
	cancels map[int64]context.CancelFunc
	runners map[string]Runner
	closed  bool
	wg      sync.WaitGroup
}

// SetMetrics enables job duration instrumentation without changing the constructor
// used by embedders and tests.
func (m *Manager) SetMetrics(metrics *obs.Metrics) { m.metrics = metrics }

// New creates a manager with a bounded cross-project pool.
func New(db *control.DB, events *obs.Bus, workers int) (*Manager, error) {
	if workers <= 0 {
		workers = 4
	}
	if err := db.InterruptJobs(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		db: db, events: events, ctx: ctx, cancel: cancel, sem: make(chan struct{}, workers),
		queues: make(map[string][]int64), active: make(map[string]bool),
		cancels: make(map[int64]context.CancelFunc), runners: make(map[string]Runner),
	}, nil
}

// Register installs an action runner.
func (m *Manager) Register(action string, runner Runner) {
	m.mu.Lock()
	m.runners[action] = runner
	m.mu.Unlock()
}

// Submit persists and queues work.
func (m *Manager) Submit(project, action, actor string, params map[string]any) (control.Job, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return control.Job{}, errors.New("job: manager closed")
	}
	m.mu.Unlock()
	record := control.Job{
		Project: project, Action: action, Status: "queued", Params: params, Actor: actor,
	}
	id, err := m.db.CreateJob(record)
	if err != nil {
		return control.Job{}, err
	}
	record.ID = id
	key := project
	if key == "" {
		key = "*"
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		now := time.Now()
		_ = m.db.SetJobStatus(id, "cancelled", "", nil, &now)
		return control.Job{}, errors.New("job: manager closed")
	}
	m.queues[key] = append(m.queues[key], id)
	if !m.active[key] {
		m.active[key] = true
		m.wg.Add(1)
		go m.runProject(key)
	}
	m.mu.Unlock()
	m.publish(record)
	return record, nil
}

// Get returns a job and its full persisted log.
func (m *Manager) Get(id int64) (control.Job, error) {
	job, err := m.db.Job(id)
	if errors.Is(err, control.ErrNotFound) {
		return control.Job{}, control.NewError(http.StatusNotFound, "job_not_found",
			"no such job: %d", id)
	}
	return job, err
}

// List returns newest jobs.
func (m *Manager) List(limit int) ([]control.Job, error) { return m.db.ListJobs(limit) }

// Cancel cancels running work or removes queued work.
func (m *Manager) Cancel(id int64) error {
	record, err := m.db.Job(id)
	if errors.Is(err, control.ErrNotFound) {
		return control.NewError(http.StatusNotFound, "job_not_found", "no such job: %d", id)
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
		m.mu.Unlock()
		return nil
	}
	for key, queue := range m.queues {
		for index, queued := range queue {
			if queued != id {
				continue
			}
			m.queues[key] = append(queue[:index], queue[index+1:]...)
			m.mu.Unlock()
			now := time.Now()
			if err := m.db.SetJobStatus(id, "cancelled", "", nil, &now); err != nil {
				return err
			}
			record.Status, record.FinishedAt = "cancelled", &now
			m.publish(record)
			return nil
		}
	}
	m.mu.Unlock()
	if record.Status == "done" || record.Status == "failed" || record.Status == "cancelled" {
		return nil
	}
	return control.NewError(http.StatusConflict, "job_not_cancellable",
		"job %d cannot be cancelled", id)
}

// Close cancels running jobs and waits for owned goroutines.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	var queued []int64
	for key, ids := range m.queues {
		queued = append(queued, ids...)
		m.queues[key] = nil
	}
	for _, cancel := range m.cancels {
		cancel()
	}
	m.mu.Unlock()
	finished := time.Now()
	for _, id := range queued {
		_ = m.db.SetJobStatus(id, "cancelled", "", nil, &finished)
	}
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) runProject(key string) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		queue := m.queues[key]
		if len(queue) == 0 || m.closed {
			delete(m.active, key)
			delete(m.queues, key)
			m.mu.Unlock()
			return
		}
		id := queue[0]
		m.queues[key] = queue[1:]
		m.mu.Unlock()

		select {
		case m.sem <- struct{}{}:
		case <-m.ctx.Done():
			return
		}
		m.runOne(id)
		<-m.sem
	}
}

func (m *Manager) runOne(id int64) {
	record, err := m.db.Job(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	m.cancels[id] = cancel
	runner := m.runners[record.Action]
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()

	started := time.Now()
	if err := m.db.SetJobStatus(id, "running", "", &started, nil); err != nil {
		return
	}
	record.Status, record.StartedAt = "running", &started
	m.publish(record)

	logf := func(line string) {
		if line == "" {
			return
		}
		if line[len(line)-1] != '\n' {
			line += "\n"
		}
		_ = m.db.AppendJobLog(id, line)
	}
	if runner == nil {
		err = fmt.Errorf("action %q is not available in this build", record.Action)
	} else {
		err = runner(ctx, record, logf)
	}
	finished := time.Now()
	status, message := "done", ""
	if err != nil {
		status, message = "failed", err.Error()
		if errors.Is(err, context.Canceled) {
			status, message = "cancelled", ""
		}
		logf(err.Error())
	}
	_ = m.db.SetJobStatus(id, status, message, nil, &finished)
	if m.metrics != nil {
		m.metrics.JobDuration.WithLabelValues(record.Action, status).
			Observe(finished.Sub(started).Seconds())
	}
	record.Status, record.Error, record.FinishedAt = status, message, &finished
	m.publish(record)
}

func (m *Manager) publish(job control.Job) {
	m.events.Publish(obs.Event{
		Kind: obs.EventJobUpdate, Project: job.Project, ID: fmt.Sprint(job.ID),
		Name: job.Action, Status: job.Status, Detail: job.Error,
	})
}
