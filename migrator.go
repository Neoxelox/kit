package kit

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/neoxelox/errors"

	"github.com/neoxelox/kit/util"
)

const (
	_MIGRATOR_POSTGRES_DSN = "postgresql://%s:%s@%s:%d/%s?sslmode=%s&x-multi-statement=true"
)

var (
	_MIGRATOR_ERR_CONNECTION_ALREADY_CLOSED = regexp.MustCompile(`.*connection is already closed.*`)
)

var (
	ErrMigratorGeneric  = errors.New("migrator failed")
	ErrMigratorTimedOut = errors.New("migrator timed out")
)

var (
	_MIGRATOR_DEFAULT_CONFIG = MigratorConfig{
		MigrationsPath: util.Pointer("./migrations"),
	}

	_MIGRATOR_DEFAULT_RETRY_CONFIG = RetryConfig{
		Attempts:     1,
		InitialDelay: 0 * time.Second,
		LimitDelay:   0 * time.Second,
		Retriables:   []error{},
	}
)

type MigratorConfig struct {
	DatabaseHost     string
	DatabasePort     int
	DatabaseSSLMode  string
	DatabaseUser     string
	DatabasePassword string
	DatabaseName     string
	MigrationsPath   *string
}

type Migrator struct {
	config   MigratorConfig
	observer *Observer
	migrator *migrate.Migrate
	done     chan struct{}
}

func NewMigrator(ctx context.Context, observer *Observer, config MigratorConfig,
	retry ...RetryConfig) (*Migrator, error) {
	util.Merge(&config, _MIGRATOR_DEFAULT_CONFIG)
	_retry := util.Optional(retry, _MIGRATOR_DEFAULT_RETRY_CONFIG)

	*config.MigrationsPath = fmt.Sprintf("file://%s", filepath.Clean(*config.MigrationsPath))

	dsn := fmt.Sprintf(
		_MIGRATOR_POSTGRES_DSN,
		config.DatabaseUser,
		config.DatabasePassword,
		config.DatabaseHost,
		config.DatabasePort,
		config.DatabaseName,
		config.DatabaseSSLMode,
	)

	var migrator *migrate.Migrate

	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		return util.ExponentialRetry(
			_retry.Attempts, _retry.InitialDelay, _retry.LimitDelay,
			_retry.Retriables, func(attempt int) error {
				var err error

				observer.Infof(ctx, "Trying to connect to the %s database %d/%d",
					config.DatabaseName, attempt, _retry.Attempts)

				migrator, err = migrate.New(*config.MigrationsPath, dsn)
				if err != nil {
					return ErrMigratorGeneric.Raise().Cause(err)
				}

				return nil
			})
	})
	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return nil, ErrMigratorTimedOut.Raise().Cause(err)
		}

		return nil, err
	}

	observer.Infof(ctx, "Connected to the %s database", config.DatabaseName)

	migrator.Log = _newMigrateLogger(observer)

	done := make(chan struct{}, 1)
	close(done)

	return &Migrator{
		observer: observer,
		config:   config,
		migrator: migrator,
		done:     done,
	}, nil
}

// TODO: concurrent-safe
func (self *Migrator) Version(ctx context.Context) (int, bool, error) {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	schemaVersion := uint(0)
	dirty := false

	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			var err error

			schemaVersion, dirty, err = self.migrator.Version()
			if err != nil && err != migrate.ErrNilVersion {
				return ErrMigratorGeneric.Raise().Cause(err)
			}

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return 0, false, ErrMigratorTimedOut.Raise().Cause(err)
		}

		return 0, false, err
	}

	return int(schemaVersion), dirty, nil
}

// TODO: concurrent-safe
func (self *Migrator) Assert(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version()
			if err != nil && err != migrate.ErrNilVersion {
				return ErrMigratorGeneric.Raise().Cause(err)
			}

			if bad {
				return ErrMigratorGeneric.Raise().With("current schema version %d is dirty", currentSchemaVersion)
			}

			if currentSchemaVersion > uint(schemaVersion) {
				return ErrMigratorGeneric.Raise().With("desired schema version %d behind from current one %d",
					schemaVersion, currentSchemaVersion)
			} else if currentSchemaVersion < uint(schemaVersion) {
				return ErrMigratorGeneric.Raise().With("desired schema version %d ahead of current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.observer.Infof(ctx, "Desired schema version %d asserted", schemaVersion)

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return ErrMigratorTimedOut.Raise().Cause(err)
		}

		return err
	}

	return nil
}

// TODO: concurrent-safe
func (self *Migrator) Apply(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version()
			if err != nil && err != migrate.ErrNilVersion {
				return ErrMigratorGeneric.Raise().Cause(err)
			}

			if bad {
				return ErrMigratorGeneric.Raise().With("current schema version %d is dirty", currentSchemaVersion)
			}

			if currentSchemaVersion == uint(schemaVersion) {
				self.observer.Info(ctx, "No migrations to apply")
				return nil
			}

			if currentSchemaVersion > uint(schemaVersion) {
				return ErrMigratorGeneric.Raise().With("desired schema version %d behind from current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.observer.Infof(ctx, "%d migrations to be applied", schemaVersion-int(currentSchemaVersion))

			err = self.migrator.Migrate(uint(schemaVersion))
			if err != nil {
				return ErrMigratorGeneric.Raise().Cause(err)
			}

			self.observer.Info(ctx, "Applied all migrations successfully")

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return ErrMigratorTimedOut.Raise().Cause(err)
		}

		return err
	}

	return nil
}

// TODO: concurrent-safe
// nolint:gocognit,revive
func (self *Migrator) Rollback(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version()
			if err != nil {
				return ErrMigratorGeneric.Raise().Cause(err)
			}

			if bad {
				self.observer.Infof(
					ctx, "Current schema version %d is dirty, setting desired to last version", currentSchemaVersion)

				err = self.migrator.Force(int(currentSchemaVersion))
				if err != nil {
					return ErrMigratorGeneric.Raise().Cause(err)
				}

				schemaVersion--
			}

			if currentSchemaVersion == uint(schemaVersion) {
				self.observer.Info(ctx, "No migrations to rollback")
				return nil
			}

			if currentSchemaVersion < uint(schemaVersion) {
				return ErrMigratorGeneric.Raise().With("desired schema version %d ahead of current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.observer.Infof(ctx, "%d migrations to be rollbacked", int(currentSchemaVersion)-schemaVersion)

			if schemaVersion == 0 {
				err = self.migrator.Down()
				if err != nil {
					return ErrMigratorGeneric.Raise().Cause(err)
				}
			} else {
				err = self.migrator.Migrate(uint(schemaVersion))
				if err != nil {
					return ErrMigratorGeneric.Raise().Cause(err)
				}
			}

			self.observer.Info(ctx, "Rollbacked all migrations successfully")

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return ErrMigratorTimedOut.Raise().Cause(err)
		}

		return err
	}

	return nil
}

func (self *Migrator) Close(ctx context.Context) error {
	err := util.Deadline(ctx, func(exceeded <-chan struct{}) error {
		self.observer.Info(ctx, "Closing migrator")

		select {
		case self.migrator.GracefulStop <- true:
		default:
		}

		<-self.done

		err, errD := self.migrator.Close()
		if errD != nil && _MIGRATOR_ERR_CONNECTION_ALREADY_CLOSED.MatchString(errD.Error()) {
			errD = nil
		}

		if err != nil {
			return ErrMigratorGeneric.Raise().Extra(map[string]any{"database_error": errD}).Cause(err)
		}

		if errD != nil {
			return ErrMigratorGeneric.Raise().Cause(errD)
		}

		self.observer.Info(ctx, "Closed migrator")

		return nil
	})
	if err != nil {
		if util.ErrDeadlineExceeded.Is(err) {
			return ErrMigratorTimedOut.Raise().Cause(err)
		}

		return err
	}

	return nil
}

type _migrateLogger struct {
	observer *Observer
}

func _newMigrateLogger(observer *Observer) *_migrateLogger {
	return &_migrateLogger{
		observer: observer,
	}
}

func (self _migrateLogger) Printf(format string, v ...any) {
	self.observer.Infof(context.Background(), strings.TrimSpace(format), v...)
}

func (self _migrateLogger) Verbose() bool {
	return false
}
