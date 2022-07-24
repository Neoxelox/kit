package kit

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

const (
	_MIGRATOR_DEFAULT_MIGRATIONS_PATH = "./migrations"
	_MIGRATOR_POSTGRES_DSN            = "postgresql://%s:%s@%s:%d/%s?sslmode=%s&x-multi-statement=true"
)

var _DB_ALREADY_CLOSED_ERR_REGEX = regexp.MustCompile(`.*connection is already closed.*`)

type MigratorRetryConfig struct {
	Attempts     int
	InitialDelay time.Duration
	LimitDelay   time.Duration
}

type MigratorConfig struct {
	MigrationsPath   *string
	DatabaseHost     string
	DatabasePort     int
	DatabaseSSLMode  string
	DatabaseUser     string
	DatabasePassword string
	DatabaseName     string
	RetryConfig      *MigratorRetryConfig
}

type Migrator struct {
	config   MigratorConfig
	logger   Logger
	migrator migrate.Migrate
	done     chan struct{}
}

func NewMigrator(ctx context.Context, logger Logger, config MigratorConfig) (*Migrator, error) {
	logger.SetFile()

	migrationsPath := _MIGRATOR_DEFAULT_MIGRATIONS_PATH
	if config.MigrationsPath != nil {
		migrationsPath = *config.MigrationsPath
	}

	migrationsPath = fmt.Sprintf("file://%s", migrationsPath)

	dsn := fmt.Sprintf(
		_MIGRATOR_POSTGRES_DSN,
		config.DatabaseUser,
		config.DatabasePassword,
		config.DatabaseHost,
		config.DatabasePort,
		config.DatabaseName,
		config.DatabaseSSLMode,
	)

	attempts := 1
	initialDelay := 0 * time.Second
	limitDelay := 0 * time.Second
	if config.RetryConfig != nil { // nolint
		attempts = config.RetryConfig.Attempts
		initialDelay = config.RetryConfig.InitialDelay
		limitDelay = config.RetryConfig.LimitDelay
	}

	var migrator *migrate.Migrate

	// TODO: only retry on specific errors
	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		return Utils.ExponentialRetry(attempts, initialDelay, limitDelay, nil, func(attempt int) error {
			var err error

			logger.Infof("Trying to connect to the database %d/%d", attempt, attempts)

			migrator, err = migrate.New(migrationsPath, dsn)
			if err != nil {
				return Errors.ErrMigratorGeneric().WrapAs(err)
			}

			return nil
		})
	})
	switch {
	case err == nil:
	case Errors.ErrDeadlineExceeded().Is(err):
		return nil, Errors.ErrMigratorTimedOut()
	default:
		return nil, Errors.ErrMigratorGeneric().Wrap(err)
	}

	logger.Info("Connected to the database")

	migrator.Log = *newMigrateLogger(logger)

	done := make(chan struct{}, 1)
	close(done)

	return &Migrator{
		logger:   logger,
		config:   config,
		migrator: *migrator,
		done:     done,
	}, nil
}

// TODO: concurrent-safe
func (self *Migrator) Assert(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version() // nolint
			if err != nil && err != migrate.ErrNilVersion {
				return Errors.ErrMigratorGeneric().WrapAs(err)
			}

			if bad {
				return Errors.ErrMigratorGeneric().Withf("current schema version %d is dirty", currentSchemaVersion)
			}

			if currentSchemaVersion > uint(schemaVersion) {
				return Errors.ErrMigratorGeneric().Withf("desired schema version %d behind from current one %d",
					schemaVersion, currentSchemaVersion)
			} else if currentSchemaVersion < uint(schemaVersion) {
				return Errors.ErrMigratorGeneric().Withf("desired schema version %d ahead of current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.logger.Infof("Desired schema version %d asserted", schemaVersion)

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

	switch {
	case err == nil:
		return nil
	case Errors.ErrDeadlineExceeded().Is(err):
		return Errors.ErrMigratorTimedOut()
	default:
		return Errors.ErrMigratorGeneric().Wrap(err)
	}
}

// TODO: concurrent-safe
func (self *Migrator) Apply(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version() // nolint
			if err != nil && err != migrate.ErrNilVersion {
				return Errors.ErrMigratorGeneric().WrapAs(err)
			}

			if bad {
				return Errors.ErrMigratorGeneric().Withf("current schema version %d is dirty", currentSchemaVersion)
			}

			if currentSchemaVersion == uint(schemaVersion) {
				self.logger.Info("No migrations to apply")

				return nil
			}

			if currentSchemaVersion > uint(schemaVersion) {
				return Errors.ErrMigratorGeneric().Withf("desired schema version %d behind from current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.logger.Infof("%d migrations to be applied", schemaVersion-int(currentSchemaVersion))

			err = self.migrator.Migrate(uint(schemaVersion))
			if err != nil {
				return Errors.ErrMigratorGeneric().WrapAs(err)
			}

			self.logger.Info("Applied all migrations successfully")

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

	switch {
	case err == nil:
		return nil
	case Errors.ErrDeadlineExceeded().Is(err):
		return Errors.ErrMigratorTimedOut()
	default:
		return Errors.ErrMigratorGeneric().Wrap(err)
	}
}

// TODO: concurrent-safe
func (self *Migrator) Rollback(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version() // nolint
			if err != nil {
				return Errors.ErrMigratorGeneric().WrapAs(err)
			}

			if bad {
				self.logger.Infof("Current schema version %d is dirty, ignoring", currentSchemaVersion)

				err = self.migrator.Force(int(currentSchemaVersion))
				if err != nil {
					return Errors.ErrMigratorGeneric().WrapAs(err)
				}
			}

			if currentSchemaVersion == uint(schemaVersion) {
				self.logger.Info("No migrations to rollback")

				return nil
			}

			if currentSchemaVersion < uint(schemaVersion) {
				return Errors.ErrMigratorGeneric().Withf("desired schema version %d ahead of current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.logger.Infof("%d migrations to be rollbacked", int(currentSchemaVersion)-schemaVersion)

			err = self.migrator.Migrate(uint(schemaVersion))
			if err != nil {
				return Errors.ErrMigratorGeneric().WrapAs(err)
			}

			self.logger.Info("Rollbacked all migrations successfully")

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

	switch {
	case err == nil:
		return nil
	case Errors.ErrDeadlineExceeded().Is(err):
		return Errors.ErrMigratorTimedOut()
	default:
		return Errors.ErrMigratorGeneric().Wrap(err)
	}
}

func (self *Migrator) Close(ctx context.Context) error { // nolint
	self.logger.Info("Closing migrator")

	select {
	case self.migrator.GracefulStop <- true:
	default:
	}

	<-self.done

	err, errD := self.migrator.Close()
	if errD != nil && _DB_ALREADY_CLOSED_ERR_REGEX.MatchString(errD.Error()) {
		errD = nil
	}

	err = Utils.CombineErrors(err, errD)
	if err != nil {
		return Errors.ErrMigratorGeneric().Wrap(err)
	}

	self.logger.Info("Closed migrator")

	return nil
}

type _migrateLogger struct {
	logger Logger
}

func newMigrateLogger(logger Logger) *_migrateLogger {
	return &_migrateLogger{
		logger: logger,
	}
}

func (self _migrateLogger) Printf(format string, v ...interface{}) {
	format = strings.TrimSpace(format)
	self.logger.Infof(format, v...)
}

func (self _migrateLogger) Verbose() bool {
	return false
}
