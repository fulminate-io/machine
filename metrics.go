// Package machine - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package machine

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type times []time.Duration

// MetricCollector holds event durations
// and counts.
type MetricCollector struct {
	mutex      sync.Mutex
	size       uint64
	count      uint64
	errorCount uint64
	ts         times
}

// Metrics holds the calculated outputs
// produced from a MetricCollector sample set.
type Metrics struct {
	Count      uint64        // Total number of events observed.
	Samples    uint64        // Number of events included in the sample set.
	Errors     uint64        // Number of errors observed.
	Cumulative time.Duration // Cumulative time of all sampled events.
	Avg        time.Duration // Event duration average.
	P50        time.Duration // Event duration nth percentiles ..
	P95        time.Duration
	P99        time.Duration
	Max        time.Duration // Highest event duration.
	Min        time.Duration // Lowest event duration.
}

// newCollector initializes a new MetricCollector.
func newCollector(size uint64) *MetricCollector {
	return &MetricCollector{
		size: size,
		ts:   make([]time.Duration, size),
	}
}

// Satisfy sort for times.
func (ts times) Len() int           { return len(ts) }
func (ts times) Less(i, j int) bool { return int64(ts[i]) < int64(ts[j]) }
func (ts times) Swap(i, j int)      { ts[i], ts[j] = ts[j], ts[i] }

func (ts times) avg() time.Duration {
	var total time.Duration
	for _, t := range ts {
		total += t
	}
	return time.Duration(int(total) / ts.Len())
}

func (ts times) p(p float64) time.Duration {
	return ts[int(float64(ts.Len())*p+0.5)-1]
}

func (ts times) min() time.Duration {
	return ts[0]
}

func (ts times) max() time.Duration {
	return ts[ts.Len()-1]
}

// Reset resets a MetricCollector
func (m *MetricCollector) Reset() {
	m.mutex.Lock()
	atomic.StoreUint64(&m.count, 0)
	m.mutex.Unlock()
}

// Record adds a time.Duration to MetricCollector and records an error if err is true.
func (m *MetricCollector) Record(t time.Duration, err error) {
	if err != nil {
		atomic.AddUint64(&m.errorCount, 1)
	}
	m.ts[(atomic.AddUint64(&m.count, 1)-1)%m.size] = t
}

// Return summarizes MetricCollector sample data
// and returns it in the form of a *Metrics.
func (m *MetricCollector) Return() *Metrics {
	metrics := &Metrics{}
	if atomic.LoadUint64(&m.count) == 0 {
		return metrics
	}

	m.mutex.Lock()

	metrics.Samples = uint64(math.Min(float64(atomic.LoadUint64(&m.count)), float64(atomic.LoadUint64(&m.size))))
	metrics.Count = atomic.LoadUint64(&m.count)
	metrics.Errors = atomic.LoadUint64(&m.errorCount)
	ts := make(times, metrics.Samples)
	copy(ts, m.ts[:metrics.Samples])

	m.mutex.Unlock()

	sort.Sort(ts)

	metrics.Avg = ts.avg()
	metrics.P50 = ts[ts.Len()/2]
	metrics.P95 = ts.p(0.95)
	metrics.P99 = ts.p(0.99)
	metrics.Min = ts.min()
	metrics.Max = ts.max()

	return metrics
}
