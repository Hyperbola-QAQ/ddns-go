package audit

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteAsyncPersistenceAndFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	w, err := Open("", path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Record(Event{Actor: "admin", Host: "node-a", Config: "group", Node: "node-b", Operation: "config.edit", Before: "old", After: "new"})
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	events, err := w.Query(context.Background(), Filter{Config: "group", Limit: 10, Offset: 10})
	if err != nil || len(events) != 10 {
		t.Fatalf("%v %v", events, err)
	}
	if events[0].Time == "" || events[0].Before != "old" || events[0].ID <= events[1].ID {
		t.Fatal("invalid audit record")
	}
	events, err = w.Query(context.Background(), Filter{Config: "' OR 1=1 --"})
	if err != nil || len(events) != 0 {
		t.Fatal("filter was not parameterized")
	}
	if _, err := Open("unregistered", ""); err == nil {
		t.Fatal("unknown adapter accepted")
	}
}

type blockingStore struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingStore) Append(ctx context.Context, _ Event) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *blockingStore) Query(context.Context, Filter) ([]Event, error) { return nil, nil }
func (s *blockingStore) Close() error                                   { return nil }
func TestAuditQueueDoesNotBlockBusiness(t *testing.T) {
	s := &blockingStore{started: make(chan struct{}, 1), release: make(chan struct{})}
	w := New(s)
	w.Record(Event{Operation: "first"})
	<-s.started
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1025; i++ {
			w.Record(Event{Operation: "queued"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(s.release)
		t.Fatal("audit blocked caller")
	}
	if w.Lost.Load() != 1 {
		t.Fatalf("lost count %d", w.Lost.Load())
	}
	close(s.release)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
