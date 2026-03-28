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
// If a system task already exists but its schedule differs from the configured
// value, the schedule is updated in-place.
//
// Currently seeded tasks:
//   - Memory consolidation (every N hours, controlled by scheduler.memory_interval)
//   - Confidence check (daily at 10:00)
func (a *App) seedSystemTasks(ctx context.Context) {
	if a.TaskRepo == nil {
		return
	}

	if a.Cfg.Scheduler.MemoryEnabled {
		// Memory consolidation is now driven by the scheduler's memTickerC
		// (map-reduce pipeline) — no longer a cron task through loopback.
		// Remove any legacy consolidation task that may still exist in the DB.
		a.removeObsoleteTask(ctx, domainservices.MemoryConsolidationPrompt)

		// Confidence check: daily at 10:00 — reviews low-confidence assertions
		// and proactively messages users to verify uncertain information.
		a.seedOrUpdateSystemTask(ctx,
			domainservices.ConfidenceCheckPrompt,
			"0 10 * * *",
		)

		// Archivist: daily at 04:00 — promotes mature assertions, merges
		// duplicate entities, expires stale relationships, creates missing
		// entity-to-entity links. Runs after consolidation to clean up.
		a.seedOrUpdateSystemTask(ctx,
			domainservices.ArchivistPrompt,
			"0 4 * * *",
		)
	}
}

// seedOrUpdateSystemTask creates a cron task with the given prompt and schedule
// if no task with that exact prompt exists. If a matching task already exists
// but has a different schedule, the schedule is updated to match the config.
func (a *App) seedOrUpdateSystemTask(ctx context.Context, prompt, schedule string) {
	tasks, err := a.TaskRepo.ListAll(ctx)
	if err != nil {
		log.Printf("scheduler: failed to list tasks for seeding: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Prompt == prompt {
			if t.Schedule != schedule {
				t.Schedule = schedule
				if err := a.TaskRepo.Update(ctx, &t); err != nil {
					log.Printf("scheduler: failed to update task schedule: %v", err)
					return
				}
				if a.SchedulerNotify != nil {
					a.SchedulerNotify()
				}
				log.Printf("scheduler: updated system task schedule to %s: %s…", schedule, prompt[:min(60, len(prompt))])
			}
			return
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

// removeObsoleteTask deletes a seeded system task by prompt text if it exists.
// Used to clean up tasks that have been superseded by other mechanisms.
func (a *App) removeObsoleteTask(ctx context.Context, prompt string) {
	tasks, err := a.TaskRepo.ListAll(ctx)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if t.Prompt == prompt {
			if err := a.TaskRepo.Delete(ctx, t.ID); err != nil {
				log.Printf("scheduler: failed to remove obsolete task: %v", err)
			} else {
				log.Printf("scheduler: removed obsolete system task: %s…", prompt[:min(60, len(prompt))])
			}
			if a.SchedulerNotify != nil {
				a.SchedulerNotify()
			}
			return
		}
	}
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
