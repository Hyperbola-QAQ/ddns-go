// Package audit provides bounded asynchronous audit writes and pluggable storage.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

type Event struct {
	ID        int64
	Time      string
	Actor     string
	Host      string
	Operation string
	Config    string
	Node      string
	Before    string
	After     string
	Result    string
}
type Filter struct {
	Config    string
	Node      string
	Operation string
	Limit     int
	Offset    int
}
type Store interface {
	Append(context.Context, Event) error
	Query(context.Context, Filter) ([]Event, error)
	Close() error
}
type Factory func(string) (Store, error)

var factories = map[string]Factory{"sqlite": openSQLite}
var factoryMu sync.RWMutex

// Register allows a MySQL/PostgreSQL adapter to own its SQL dialect and migrations.
func Register(name string, factory Factory) {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factories[name] = factory
}
func Open(driver, dsn string) (*Writer, error) {
	if driver == "" {
		driver = "sqlite"
	}
	factoryMu.RLock()
	f := factories[driver]
	factoryMu.RUnlock()
	if f == nil {
		return nil, errors.New("未注册审计数据库驱动: " + driver)
	}
	s, err := f(dsn)
	if err != nil {
		return nil, err
	}
	return New(s), nil
}

type Writer struct {
	store  Store
	queue  chan Event
	done   chan struct{}
	mu     sync.RWMutex
	closed bool
	Lost   atomic.Uint64
}

func New(s Store) *Writer {
	w := &Writer{store: s, queue: make(chan Event, 1024), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer s.Close()
		for e := range w.queue {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.Append(ctx, e)
			cancel()
			if err != nil {
				w.Lost.Add(1)
				log.Print("审计写入失败: ", err)
			}
		}
	}()
	return w
}
func (w *Writer) Record(e Event) {
	if w == nil {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.Lost.Add(1)
		return
	}
	select {
	case w.queue <- e:
	default:
		w.Lost.Add(1)
		log.Print("审计队列已满，事件未持久化")
	}
}
func (w *Writer) Query(ctx context.Context, f Filter) ([]Event, error) {
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return w.store.Query(ctx, f)
}
func (w *Writer) Close() error { return w.Shutdown(context.Background()) }
func (w *Writer) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var Default *Writer  // set once before the HTTP server and scheduler start
func Record(e Event) { Default.Record(e) }

type sqlStore struct{ db *sql.DB }

func openSQLite(dsn string) (Store, error) {
	if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
		f, err := os.OpenFile(dsn, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL",
		`CREATE TABLE IF NOT EXISTS audit_events (id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT NOT NULL, actor TEXT NOT NULL, host TEXT NOT NULL, operation TEXT NOT NULL, config TEXT NOT NULL, node TEXT NOT NULL, before_value TEXT NOT NULL, after_value TEXT NOT NULL, result TEXT NOT NULL)`,
		"CREATE INDEX IF NOT EXISTS audit_config_id ON audit_events(config, id)",
	} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &sqlStore{db}, nil
}
func (s *sqlStore) Append(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(time,actor,host,operation,config,node,before_value,after_value,result) VALUES(?,?,?,?,?,?,?,?,?)`, e.Time, e.Actor, e.Host, e.Operation, e.Config, e.Node, e.Before, e.After, e.Result)
	return err
}
func (s *sqlStore) Query(ctx context.Context, f Filter) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,time,actor,host,operation,config,node,before_value,after_value,result FROM audit_events WHERE (?='' OR config=?) AND (?='' OR node=?) AND (?='' OR operation=?) ORDER BY id DESC LIMIT ? OFFSET ?`, f.Config, f.Config, f.Node, f.Node, f.Operation, f.Operation, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Time, &e.Actor, &e.Host, &e.Operation, &e.Config, &e.Node, &e.Before, &e.After, &e.Result); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
func (s *sqlStore) Close() error { return s.db.Close() }
