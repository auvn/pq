package pq

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/log/zapadapter"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/pkg/errors"
)

type PgxAdapter struct {
	pool        *pgxpool.Pool
	withMetrics bool
	withTracing bool
	name        string
}

var _ Client = &PgxAdapter{}

func (p *PgxAdapter) Transaction(ctx context.Context, f func(context.Context, Executor) error) error {
	tx, er := p.pool.BeginTx(ctx, defaultTxOptions)
	if er != nil {
		return er
	}

	var txAdapter Executor = &PgxTxAdapter{tx}
	if p.withTracing {
		txAdapter = &tracingAdapter{Executor: txAdapter}
	}
	if p.withMetrics {
		txAdapter = &metricsAdapter{Executor: txAdapter, name: p.name}
	}
	execErr := f(ctx, txAdapter)
	var err error

	if execErr != nil {
		err = errors.Wrap(execErr, "failed to exec transaction")
		rbErr := tx.Rollback(ctx)
		if rbErr != nil {
			err = errors.Wrapf(err, "failed to rollback failed transaction: %v", rbErr)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return errors.Wrap(err, "failed to commit transaction")
	}

	return nil
}

func (p *PgxAdapter) Exec(ctx context.Context, sql string, args ...interface{}) (result RowsAffected, err error) {
	return p.pool.Exec(ctx, sql, args...)
}

func (p *PgxAdapter) Query(ctx context.Context, sql string, args ...interface{}) (Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

func (p *PgxAdapter) QueryRow(ctx context.Context, sql string, args ...interface{}) Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

func (p PgxAdapter) SetLogLevel(lvl int) error {
	panic("implement me")
}

func NewClient(ctx context.Context, cfg Config) Client {
	cfg = cfg.withDefaults()

	poolCfg, err := pgxpool.ParseConfig(cfg.ConnString)
	if err != nil {
		if err != nil {
			panic(fmt.Sprintf("failed to connect to postgres %s: %v", cfg.ConnString, err))
		}
	}

	if cfg.TCPKeepAlivePeriod == 0 {
		cfg.TCPKeepAlivePeriod = 5 * time.Minute // that's default value used by pgx internally
	}
	dialer := &net.Dialer{
		Timeout:   cfg.AcquireTimeout,
		KeepAlive: cfg.TCPKeepAlivePeriod,
	}

	poolCfg.ConnConfig.DialFunc = dialer.DialContext
	poolCfg.MaxConns = cfg.MaxConnections
	if cfg.Logger != nil {
		poolCfg.ConnConfig.Logger = zapadapter.NewLogger(cfg.Logger)
	}
	poolCfg.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
		return !conn.IsClosed()
	}

	connPool, err := pgxpool.ConnectConfig(ctx, poolCfg)
	if err != nil {
		panic(fmt.Sprintf("failed to connect to postgres %s: %v", cfg.ConnString, err))
	}

	if err := collector.register(cfg.Name, connPool); err != nil {
		panic(fmt.Sprintf("failed to register dbx pool %q: %v", cfg.Name, err))
	}

	var adapter Client = &PgxAdapter{
		pool:        connPool,
		withMetrics: cfg.Metrics,
		withTracing: cfg.Tracing,
		name:        cfg.Name,
	}

	if cfg.Tracing {
		adapter = &tracingAdapter{Executor: adapter, Transactor: adapter}
	}

	if cfg.Metrics {
		adapter = &metricsAdapter{Executor: adapter, Transactor: adapter, name: cfg.Name}
	}

	return adapter
}
