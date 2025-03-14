package common

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/datastore/revision"

	"github.com/prometheus/client_golang/prometheus"
	promclient "github.com/prometheus/client_model/go"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Fake garbage collector that returns a new incremented revision each time
// TxIDBefore is called.
type fakeGC struct {
	lastRevision int64
	deleter      gcDeleter
	metrics      gcMetrics
	lock         sync.RWMutex
}

type gcMetrics struct {
	deleteBeforeTxCount   int
	markedCompleteCount   int
	resetGCCompletedCount int
}

func newFakeGC(deleter gcDeleter) fakeGC {
	return fakeGC{
		lastRevision: 0,
		deleter:      deleter,
	}
}

func (*fakeGC) ReadyState(_ context.Context) (datastore.ReadyState, error) {
	return datastore.ReadyState{
		Message: "Ready",
		IsReady: true,
	}, nil
}

func (*fakeGC) Now(_ context.Context) (time.Time, error) {
	return time.Now(), nil
}

func (gc *fakeGC) TxIDBefore(_ context.Context, _ time.Time) (datastore.Revision, error) {
	gc.lock.Lock()
	defer gc.lock.Unlock()

	gc.lastRevision++

	rev := revision.NewFromDecimal(decimal.NewFromInt(gc.lastRevision))

	return rev, nil
}

func (gc *fakeGC) DeleteBeforeTx(_ context.Context, rev datastore.Revision) (DeletionCounts, error) {
	gc.lock.Lock()
	defer gc.lock.Unlock()

	gc.metrics.deleteBeforeTxCount++

	revInt := rev.(revision.Decimal).Decimal.IntPart()

	return gc.deleter.DeleteBeforeTx(revInt)
}

func (gc *fakeGC) HasGCRun() bool {
	gc.lock.Lock()
	defer gc.lock.Unlock()

	return gc.metrics.markedCompleteCount > 0
}

func (gc *fakeGC) MarkGCCompleted() {
	gc.lock.Lock()
	defer gc.lock.Unlock()

	gc.metrics.markedCompleteCount++
}

func (gc *fakeGC) ResetGCCompleted() {
	gc.lock.Lock()
	defer gc.lock.Unlock()

	gc.metrics.resetGCCompletedCount++
}

func (gc *fakeGC) GetMetrics() gcMetrics {
	gc.lock.Lock()
	defer gc.lock.Unlock()

	return gc.metrics
}

// Allows specifying different deletion behaviors for tests
type gcDeleter interface {
	DeleteBeforeTx(revision int64) (DeletionCounts, error)
}

// Always error trying to perform a delete
type alwaysErrorDeleter struct{}

func (alwaysErrorDeleter) DeleteBeforeTx(_ int64) (DeletionCounts, error) {
	return DeletionCounts{}, fmt.Errorf("delete error")
}

// Only error on specific revisions
type revisionErrorDeleter struct {
	errorOnRevisions []int64
}

func (d revisionErrorDeleter) DeleteBeforeTx(revision int64) (DeletionCounts, error) {
	if slices.Contains(d.errorOnRevisions, revision) {
		return DeletionCounts{}, fmt.Errorf("delete error")
	}

	return DeletionCounts{}, nil
}

func TestGCFailureBackoff(t *testing.T) {
	localCounter := prometheus.NewCounter(gcFailureCounterConfig)
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(localCounter))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		gc := newFakeGC(alwaysErrorDeleter{})
		require.Error(t, startGarbageCollectorWithMaxElapsedTime(ctx, &gc, 100*time.Millisecond, 1*time.Second, 1*time.Nanosecond, 1*time.Minute, localCounter))
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	metrics, err := reg.Gather()
	require.NoError(t, err)
	var mf *promclient.MetricFamily
	for _, metric := range metrics {
		if metric.GetName() == "spicedb_datastore_gc_failure_total" {
			mf = metric
		}
	}
	require.Greater(t, *(mf.GetMetric()[0].Counter.Value), 100.0, "MaxElapsedTime=1ns did not cause backoff to get ignored")

	localCounter = prometheus.NewCounter(gcFailureCounterConfig)
	reg = prometheus.NewRegistry()
	require.NoError(t, reg.Register(localCounter))
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go func() {
		gc := newFakeGC(alwaysErrorDeleter{})
		require.Error(t, startGarbageCollectorWithMaxElapsedTime(ctx, &gc, 100*time.Millisecond, 0, 1*time.Second, 1*time.Minute, localCounter))
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	metrics, err = reg.Gather()
	require.NoError(t, err)
	for _, metric := range metrics {
		if metric.GetName() == "spicedb_datastore_gc_failure_total" {
			mf = metric
		}
	}
	require.Less(t, *(mf.GetMetric()[0].Counter.Value), 3.0, "MaxElapsedTime=0 should have not caused backoff to get ignored")
}

// Ensure the garbage collector interval is reset after recovering from an
// error. The garbage collector should not continue to use the exponential
// backoff interval that is activated on error.
func TestGCFailureBackoffReset(t *testing.T) {
	gc := newFakeGC(revisionErrorDeleter{
		// Error on revisions 1 - 5, giving the exponential
		// backoff enough time to fail the test if the interval
		// is not reset properly.
		errorOnRevisions: []int64{1, 2, 3, 4, 5},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		interval := 10 * time.Millisecond
		window := 10 * time.Second
		timeout := 1 * time.Minute

		require.Error(t, StartGarbageCollector(ctx, &gc, interval, window, timeout))
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	// The next interval should have been reset after recovering from the error.
	// If it is not reset, the last exponential backoff interval will not give
	// the GC enough time to run.
	require.Greater(t, gc.GetMetrics().markedCompleteCount, 20, "Next interval was not reset with backoff")
}
