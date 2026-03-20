package serve

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/neirth/openlobster/internal/domain/models"
	domainservices "github.com/neirth/openlobster/internal/domain/services"
)

// seedSystemTasks ensures that built-in recurring tasks exist in the database
// so they are visible and manageable from the UI like any other cron task.
//
// Currently seeded tasks:
//   - Memory consolidation (every N hours, controlled by scheduler.memory_interval)
//
// Each task is only created once; subsequent restarts are no-ops.
func (a *App) seedSystemTasks(ctx context.Context) {
	if a.TaskRepo == nil {
		return
	}

	if a.Cfg.Scheduler.MemoryEnabled {
		a.seedTaskIfAbsent(ctx,
			domainservices.MemoryConsolidationPrompt,
			durationToHourlyCron(a.Cfg.Scheduler.MemoryInterval),
		)
	}
}

// seedTaskIfAbsent creates a cron task with the given prompt and schedule only
// if no task with that exact prompt already exists in the database.
func (a *App) seedTaskIfAbsent(ctx context.Context, prompt, schedule string) {
	tasks, err := a.TaskRepo.ListAll(ctx)
	if err != nil {
		log.Printf("scheduler: failed to list tasks for seeding: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Prompt == prompt {
			return // already seeded
		}
	}
	task := models.NewTask(prompt, schedule)
	if err := a.TaskRepo.Add(ctx, task); err != nil {
		log.Printf("scheduler: failed to seed task %q: %v", prompt[:min(40, len(prompt))], err)
		return
	}
	if a.SchedulerNotify != nil {
		a.SchedulerNotify()
	}
	log.Printf("scheduler: seeded system task (schedule=%s): %s…", schedule, prompt[:min(60, len(prompt))])
}

// durationToHourlyCron converts a duration to the nearest whole-hour cron
// expression. Fractions of an hour are rounded down; durations < 1h default
// to hourly ("0 * * * *").
func durationToHourlyCron(d time.Duration) string {
	h := int(d.Hours())
	if h <= 1 {
		return "0 * * * *"
	}
	return fmt.Sprintf("0 */%d * * *", h)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
