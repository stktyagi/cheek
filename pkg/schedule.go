package cheek

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rs/zerolog"
)

type Schedule struct {
	Jobs       map[string]*JobSpec `yaml:"jobs" json:"jobs"`
	OnSuccess  OnEvent             `yaml:"on_success,omitempty" json:"on_success,omitempty"`
	OnError    OnEvent             `yaml:"on_error,omitempty" json:"on_error,omitempty"`
	TZLocation string              `yaml:"tz_location,omitempty" json:"tz_location,omitempty"`
	LockJobs   bool                `yaml:"disable_concurrent_execution,omitempty" json:"disable_concurrent_execution,omitempty"`
	loc        *time.Location
	log        zerolog.Logger
	cfg        Config
	jobMutex   sync.Mutex
}

func (s *Schedule) Run() {
	var currentTickTime time.Time
	s.log.Info().Bool("lock_jobs", s.LockJobs).Msg("Scheduler started")
	ticker := time.NewTicker(15 * time.Second)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	if s.cfg.DB != nil {
		defer s.cfg.DB.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-sigs
		cancel()
	}()

	var wg sync.WaitGroup

	for {
		select {
		case <-ticker.C:
			s.log.Debug().Msg("tick")
			currentTickTime = s.now()

			for _, j := range s.Jobs {
				if j.Cron == "" {
					continue
				}

				if j.nextTick.Before(currentTickTime) {
					s.log.Debug().Msgf("%v is due", j.Name)

					if err := j.setNextTick(currentTickTime, false); err != nil {
    						s.log.Fatal().Err(err).Msg("error determining next tick")
					}


					wg.Add(1)
					go func(j *JobSpec) {
						defer wg.Done()
						if s.LockJobs {
							s.jobMutex.Lock()
							defer s.jobMutex.Unlock()
						}
						j.execCommandWithRetry("cron")
					}(j)
				}
			}

		case <-ctx.Done():
			s.log.Info().Msg("Shutting down scheduler due to signal")
			wg.Wait()
			return
		}
	}
}

type stringArray []string

func (a *stringArray) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var multi []string
	err := unmarshal(&multi)
	if err != nil {
		var single string
		err := unmarshal(&single)
		if err != nil {
			return err
		}
		*a = strings.Fields(single)
	} else {
		*a = multi
	}
	return nil
}

func readSpecs(fn string) (*Schedule, error) {
	yfile, err := os.ReadFile(fn)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file: %w", err)
	}

	specs := Schedule{}
	if err = yaml.Unmarshal(yfile, &specs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}
	return &specs, nil
}

func (s *Schedule) initialize() error {
	if s.TZLocation == "" {
		s.TZLocation = "Local"
	}

	loc, err := time.LoadLocation(s.TZLocation)
	if err != nil {
		return err
	}
	s.loc = loc

	for k, v := range s.Jobs {
		triggerJobs := append(v.OnSuccess.TriggerJob, v.OnError.TriggerJob...)
		for _, t := range triggerJobs {
			if _, ok := s.Jobs[t]; !ok {
				return fmt.Errorf("cannot find spec of job '%s' that is referenced in job '%s'", t, k)
			}
		}
		v.Name = k
		v.globalSchedule = s
		v.log = s.log
		v.cfg = s.cfg

		if err := v.ValidateCron(); err != nil {
			return err
		}

		if err := v.setNextTick(s.now(), true); err != nil {
			return err
		}
	}

	return nil
}

func (s *Schedule) now() time.Time {
	return time.Now().In(s.loc)
}

func loadSchedule(log zerolog.Logger, cfg Config, fn string) (*Schedule, error) {
	s, err := readSpecs(fn)
	if err != nil {
		return nil, err
	}
	s.log = log
	s.cfg = cfg

	if err := s.initialize(); err != nil {
		return nil, err
	}
	s.log.Info().Msg("Schedule loaded and validated")
	return s, nil
}

func RunSchedule(log zerolog.Logger, cfg Config, scheduleFn string) error {
	s, err := loadSchedule(log, cfg, scheduleFn)
	if err != nil {
		return err
	}

	numberJobs := len(s.Jobs)
	i := 1
	for k := range s.Jobs {
		s.log.Info().Msgf("Initializing (%v/%v) job: %s", i, numberJobs, k)
		i++
	}

	go server(s)
	s.Run()
	return nil
}
