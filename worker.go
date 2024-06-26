package kit

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/hibiken/asynq"
	"github.com/neoxelox/errors"

	"github.com/neoxelox/kit/util"
)

const (
	_WORKER_REDIS_DSN = "%s:%d"
)

var (
	ErrWorkerGeneric  = errors.New("worker failed")
	ErrWorkerTimedOut = errors.New("worker timed out")
)

var _KlevelToAlevel = map[Level]asynq.LogLevel{
	LvlTrace: asynq.DebugLevel,
	LvlDebug: asynq.DebugLevel,
	LvlInfo:  asynq.InfoLevel,
	LvlWarn:  asynq.WarnLevel,
	LvlError: asynq.ErrorLevel,
	LvlNone:  asynq.FatalLevel,
}

var (
	_WORKER_DEFAULT_CONFIG = WorkerConfig{
		Concurrency:          util.Pointer(4 * runtime.GOMAXPROCS(-1)),
		StrictPriority:       util.Pointer(false),
		StopTimeout:          util.Pointer(30 * time.Second),
		TimeZone:             time.UTC,
		ScheduleDefaultRetry: util.Pointer(0),
		CacheMaxConns:        util.Pointer(max(8, 4*runtime.GOMAXPROCS(-1))),
		CacheReadTimeout:     util.Pointer(30 * time.Second),
		CacheWriteTimeout:    util.Pointer(30 * time.Second),
		CacheDialTimeout:     util.Pointer(30 * time.Second),
	}
)

type WorkerConfig struct {
	Queues               map[string]int
	Concurrency          *int
	StrictPriority       *bool
	StopTimeout          *time.Duration
	TimeZone             *time.Location
	ScheduleDefaultRetry *int
	CacheHost            string
	CachePort            int
	CacheSSLMode         bool
	CachePassword        string
	CacheMaxConns        *int
	CacheReadTimeout     *time.Duration
	CacheWriteTimeout    *time.Duration
	CacheDialTimeout     *time.Duration
}

type Worker struct {
	config    WorkerConfig
	observer  *Observer
	server    *asynq.Server
	register  *asynq.ServeMux
	scheduler *asynq.Scheduler
}

func NewWorker(observer *Observer, errorHandler *ErrorHandler, config WorkerConfig) *Worker {
	util.Merge(&config, _WORKER_DEFAULT_CONFIG)

	dsn := fmt.Sprintf(_WORKER_REDIS_DSN, config.CacheHost, config.CachePort)

	var ssl *tls.Config
	if config.CacheSSLMode {
		ssl = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	redisConfig := asynq.RedisClientOpt{
		Addr:         dsn,
		TLSConfig:    ssl,
		Password:     config.CachePassword,
		DialTimeout:  *config.CacheDialTimeout,
		ReadTimeout:  *config.CacheReadTimeout,
		WriteTimeout: *config.CacheWriteTimeout,
		PoolSize:     *config.CacheMaxConns,
	}

	asynqLogger := _newAsynqLogger(observer)
	asynqLogLevel := _KlevelToAlevel[asynqLogger.observer.Level()]

	// Asynq debug level is too much!
	if asynqLogLevel <= asynq.DebugLevel {
		asynqLogLevel = asynq.InfoLevel
	}

	serverConfig := asynq.Config{
		Concurrency:     *config.Concurrency,
		Queues:          config.Queues,
		StrictPriority:  *config.StrictPriority,
		ShutdownTimeout: *config.StopTimeout,
		Logger:          asynqLogger,
		LogLevel:        asynqLogLevel,
		ErrorHandler:    asynq.ErrorHandlerFunc(errorHandler.HandleTask),
	}

	schedulerConfig := asynq.SchedulerOpts{
		Location: config.TimeZone,
		Logger:   asynqLogger,
		LogLevel: asynqLogLevel,
		PostEnqueueFunc: func(info *asynq.TaskInfo, err error) {
			if err == nil {
				asynqLogger.observer.Infof(context.Background(),
					"Enqueued task %s on queue %s with id %s", info.Type, info.Queue, info.ID)
			}
		},
		EnqueueErrorHandler: func(task *asynq.Task, opts []asynq.Option, err error) {
			errorHandler.HandleTask(context.Background(), task, err)
		},
	}

	return &Worker{
		config:    config,
		observer:  observer,
		server:    asynq.NewServer(redisConfig, serverConfig),
		register:  asynq.NewServeMux(),
		scheduler: asynq.NewScheduler(redisConfig, &schedulerConfig),
	}
}

func (self *Worker) Run(ctx context.Context) error {
	self.observer.Infof(ctx, "Worker started with queues %v", self.config.Queues)

	err := self.server.Start(self.register)
	if err != nil && err != asynq.ErrServerClosed {
		return ErrWorkerGeneric.Raise().Cause(err)
	}

	err = self.scheduler.Start()
	if err != nil {
		return ErrWorkerGeneric.Raise().Cause(err)
	}

	return nil
}

func (self *Worker) Use(middleware ...asynq.MiddlewareFunc) {
	self.register.Use(middleware...)
}

func (self *Worker) Register(task string, handler func(context.Context, *asynq.Task) error) {
	self.register.HandleFunc(task, handler)
}

func (self *Worker) Schedule(task string, params any, cron string, options ...asynq.Option) {
	payload, err := json.Marshal(params)
	if err != nil {
		self.observer.Panicf(context.Background(), "%s: %v", task, err)
	}

	_, err = self.scheduler.Register(cron,
		asynq.NewTask(task, payload, asynq.MaxRetry(*self.config.ScheduleDefaultRetry)), options...)
	if err != nil {
		self.observer.Panicf(context.Background(), "%s: %v", task, err)
	}
}

func (self *Worker) Close(ctx context.Context) error {
	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		self.observer.Info(ctx, "Closing worker")

		self.scheduler.Shutdown()
		self.server.Stop()
		self.server.Shutdown()

		self.observer.Info(ctx, "Closed worker")

		return nil
	})
	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return ErrWorkerTimedOut.Raise().Cause(err)
		}

		return err
	}

	return nil
}

type _asynqLogger struct {
	observer *Observer
}

func _newAsynqLogger(observer *Observer) *_asynqLogger {
	return &_asynqLogger{
		observer: observer,
	}
}

func (self _asynqLogger) Debug(args ...any) {
	self.observer.Debug(context.Background(), args...)
}

func (self _asynqLogger) Info(args ...any) {
	self.observer.Info(context.Background(), args...)
}

func (self _asynqLogger) Warn(args ...any) {
	self.observer.Warn(context.Background(), args...)
}

func (self _asynqLogger) Error(args ...any) {
	self.observer.Error(context.Background(), args...)
}

func (self _asynqLogger) Fatal(args ...any) {
	self.observer.Fatal(context.Background(), args...)
}
