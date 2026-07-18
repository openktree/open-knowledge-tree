---
id: tutorial-background-task
sidebar_position: 5
title: Writing a Background Task
---

# Tutorial: Writing a Background Task

OKT uses [River](https://github.com/riverqueue/river) for background job processing. Each task is a Go type that satisfies River's `JobArgs` interface. This tutorial shows how to add a new task — for example, a periodic "health check" that pings Qdrant and logs the result.

## The worker pattern

Every task file in `backend/internal/taskmanager/tasks/` follows the same structure:

1. **Queue constant** — the River queue name
2. **Args struct** — carries job parameters, implements `JobArgs`
3. **Result struct** — recorded in the job row after completion
4. **Worker struct** — holds injected dependencies, embeds `river.WorkerDefaults`
5. **Constructor** — `NewXxxWorker(deps...)`
6. **Work method** — the actual job logic

## Step 1: Create the task file

Create `backend/internal/taskmanager/tasks/health_check.go`:

```go
package tasks

import (
    "context"
    "fmt"
    "log"

    "github.com/riverqueue/river"
)

const QueueHealthCheck = "health_check"

type HealthCheckArgs struct{}

func (HealthCheckArgs) Kind() string { return "health_check" }
func (HealthCheckArgs) InsertOpts() river.InsertOpts {
    return river.InsertOpts{
        Queue: QueueHealthCheck,
    }
}

type HealthCheckResult struct {
    Status string `json:"status"`
    Qdrant bool   `json:"qdrant_healthy"`
}

type HealthCheckWorker struct {
    river.WorkerDefaults[HealthCheckArgs]
}

func NewHealthCheckWorker() *HealthCheckWorker {
    return &HealthCheckWorker{}
}

func (w *HealthCheckWorker) Work(ctx context.Context, job *river.Job[HealthCheckArgs]) error {
    log.Printf("health_check: running for repository %s", job.Metadata.RepositoryID)

    // Your health check logic here.
    // For example, ping Qdrant:
    //   healthy := qdrant.HealthCheck(ctx)

    result := HealthCheckResult{
        Status: "ok",
        Qdrant: true,
    }

    log.Printf("health_check: %s", result.Status)
    return nil
}
```

## Step 2: Register the worker

In `backend/internal/taskmanager/taskmanager.go` (or wherever the River worker list is built), add your worker:

```go
import "github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"

// In the worker list:
workers.Add(tasks.NewHealthCheckWorker())
```

## Step 3: Register the queue

In the same file, add the queue to River's queue map with a worker count:

```go
Queues: map[string]river.QueueConfig{
    tasks.QueueHealthCheck: {MaxWorkers: 10},
    // ... existing queues
},
```

## Step 4: Add a config entry (optional)

If the task needs a configurable worker count, add it to `config.default.yaml` under `task.queues`:

```yaml
task:
  queues:
    health_check: 10
```

## Step 5: Enqueue jobs

### From a handler

Inject the task enqueuer into your handler and call it:

```go
// In a handler method:
_, err = h.taskEnqueuer.Enqueue(ctx, tasks.HealthCheckArgs{}, river.InsertOpts{
    Metadata: map[string]interface{}{
        "repository_id": repoID.String(),
    },
})
```

### Periodic (cron-style)

For periodic tasks, register a periodic job in the task manager:

```go
periodicJobs: []river.PeriodicJob{
    {
        Constructor: river.PeriodicJobArgs{
            Schedule: river.PeriodicSchedule{Period: 1 * time.Hour},
            QueueName: tasks.QueueHealthCheck,
        },
        Enabled: true,
    },
},
```

## Step 6: Test it

### Unit test

```go
package tasks_test

import (
    "context"
    "testing"

    "github.com/riverqueue/river"

    "github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
)

func TestHealthCheckWorker_Work(t *testing.T) {
    w := tasks.NewHealthCheckWorker()
    job := &river.Job[tasks.HealthCheckArgs]{
        Args: tasks.HealthCheckArgs{},
    }
    err := w.Work(t.Context(), job)
    if err != nil {
        t.Fatalf("Work failed: %v", err)
    }
}
```

### E2E test

Enqueue the job via the API and poll the task status endpoint until it completes.

## Summary

| File | Change |
|------|--------|
| `backend/internal/taskmanager/tasks/health_check.go` | New file — args, result, worker |
| `backend/internal/taskmanager/taskmanager.go` | Register worker + queue |
| `backend/configs/config.default.yaml` | Add queue worker count (optional) |
| Handler file | Enqueue jobs where needed |
