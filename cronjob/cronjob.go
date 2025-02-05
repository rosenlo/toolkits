package cronjob

import (
	"context"

	"github.com/robfig/cron/v3"
)

type Config struct {

	// The spec is parsed using the time zone of this Cron instance as the default.
	Spec string

	// Adds a func to the Cron to be run on the given schedule.
	Job cron.Job
}

type CronJob struct {
	cron *cron.Cron
}

var (
	defaultCronJob = New()
)

func Register(cfg Config) error {
	return defaultCronJob.Register(cfg)
}

func Default() *CronJob { return defaultCronJob }

func New() *CronJob {
	c := cron.New()
	return &CronJob{cron: c}
}

func (c *CronJob) Register(cfg Config) error {
	_, err := c.cron.AddJob(cfg.Spec, cfg.Job)
	return err
}

func (c *CronJob) Start(ctx context.Context) {
	c.cron.Start()
	select {
	case <-ctx.Done():
		c.Stop()
	}
}

func (c *CronJob) Stop() {
	ctx := c.cron.Stop()
	select {
	case <-ctx.Done():
		return
	}
}
