package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// errRefused stands in for the error a new tenant pod gets from its own
// Postgres in the first second after a roll on node robot-1
// (molecule-core#5303): "dial tcp …:5432: connect: connection refused".
var errRefused = errors.New("ping: dial tcp 10.42.2.215:5432: connect: connection refused")

// fakeClock is a clock that only moves when the code under test sleeps.
type fakeClock struct {
	t      time.Time
	slept  time.Duration
	sleeps int
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(d time.Duration) {
	c.t = c.t.Add(d)
	c.slept += d
	c.sleeps++
}

// A refused first connection must not end the sidecar: the tenant entrypoint
// waits 30 s for /v1/health, and the same Postgres accepts a connection a few
// seconds later. Before the fix the sidecar exited on the first refusal, so a
// tenant without MEMORY_PLUGIN_REQUIRED (jrs) booted without memory at every
// roll, and the others crashed once.
func TestOpenDBWithRetry_SucceedsAfterRefusals(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	want := &sql.DB{}
	calls := 0
	open := func(string) (*sql.DB, error) {
		calls++
		if calls <= 3 {
			return nil, errRefused
		}
		return want, nil
	}

	got, err := openDBWithRetry("postgres://stub", open, 25*time.Second, time.Second, clk.now, clk.sleep)
	if err != nil {
		t.Fatalf("openDBWithRetry returned an error after 3 refusals and a success: %v", err)
	}
	if got != want {
		t.Fatalf("openDBWithRetry did not return the database the successful attempt opened")
	}
	if calls != 4 {
		t.Errorf("open called %d times, want 4 (3 refusals, then the success)", calls)
	}
	if clk.sleeps != 3 || clk.slept != 3*time.Second {
		t.Errorf("slept %d times for %s, want 3 times for 3s (one interval between attempts)", clk.sleeps, clk.slept)
	}
}

func TestOpenDBWithRetry_FirstAttemptSucceedsWithoutWaiting(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	want := &sql.DB{}
	calls := 0
	open := func(string) (*sql.DB, error) { calls++; return want, nil }

	got, err := openDBWithRetry("postgres://stub", open, 25*time.Second, time.Second, clk.now, clk.sleep)
	if err != nil || got != want {
		t.Fatalf("openDBWithRetry = (%v, %v), want the opened database and no error", got, err)
	}
	if calls != 1 || clk.sleeps != 0 {
		t.Errorf("open called %d times and slept %d times, want 1 and 0", calls, clk.sleeps)
	}
}

// A database that never answers still fails the boot, within the wait: the
// entrypoint's own 30 s limit and MEMORY_PLUGIN_REQUIRED keep their meaning.
func TestOpenDBWithRetry_GivesUpWithinTheWait(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	open := func(string) (*sql.DB, error) { calls++; return nil, errRefused }

	got, err := openDBWithRetry("postgres://stub", open, 25*time.Second, time.Second, clk.now, clk.sleep)
	if err == nil || got != nil {
		t.Fatalf("openDBWithRetry = (%v, %v), want no database and an error", got, err)
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("error %q does not wrap the last attempt's error", err)
	}
	if calls != 25 {
		t.Errorf("open called %d times, want 25 (one per second from 0s to 24s)", calls)
	}
	if clk.slept >= 25*time.Second {
		t.Errorf("slept %s, want less than the 25s wait", clk.slept)
	}
	if !strings.Contains(err.Error(), "25 attempt") {
		t.Errorf("error %q does not say how many attempts were made", err)
	}
}

// run() itself must go through the retry: with nothing listening on the
// database port, the boot error says how many attempts were made.
func TestRun_RetriesTheFirstDatabaseConnection(t *testing.T) {
	oldWait, oldInterval := dbConnectWait, dbConnectInterval
	dbConnectWait, dbConnectInterval = 1500*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { dbConnectWait, dbConnectInterval = oldWait, oldInterval })

	// Port 1 on loopback: nothing listens there, so every connection is refused.
	t.Setenv("MEMORY_PLUGIN_DATABASE_URL", "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	t.Setenv("MEMORY_PLUGIN_LISTEN_ADDR", "127.0.0.1:0")

	err := run()
	if err == nil {
		t.Fatal("run() succeeded with no database listening")
	}
	if !strings.Contains(err.Error(), "open db: after ") || !strings.Contains(err.Error(), "attempt") {
		t.Errorf("run() error %q: want the open-db error from the retry (\"open db: after N attempt(s) …\")", err)
	}
}
