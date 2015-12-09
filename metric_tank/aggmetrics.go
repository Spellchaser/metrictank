package main

import (
	"sync"
	"time"

	"github.com/grafana/grafana/pkg/log"
)

type AggMetrics struct {
	sync.RWMutex
	Metrics        map[string]*AggMetric
	chunkSpan      uint32
	numChunks      uint32
	aggSettings    []aggSetting // for now we apply the same settings to all AggMetrics. later we may want to have different settings.
	chunkMaxStale  uint32
	metricMaxStale uint32
}

func NewAggMetrics(chunkSpan, numChunks, chunkMaxStale, metricMaxStale uint32, aggSettings []aggSetting) *AggMetrics {
	ms := AggMetrics{
		Metrics:        make(map[string]*AggMetric),
		chunkSpan:      chunkSpan,
		numChunks:      numChunks,
		aggSettings:    aggSettings,
		chunkMaxStale:  chunkMaxStale,
		metricMaxStale: metricMaxStale,
	}

	go ms.stats()
	go ms.GC()
	return &ms
}

func (ms *AggMetrics) stats() {
	pointsPerMetric.Value(0)

	for range time.Tick(time.Duration(1) * time.Second) {
		ms.RLock()
		l := len(ms.Metrics)
		ms.RUnlock()
		metricsActive.Value(int64(l))
	}
}

// periodically scan chunks and close any that have not received data in a while
// TODO instrument occurences and duration of GC
func (ms *AggMetrics) GC() {
	ticker := time.Tick(time.Duration(*gcInterval) * time.Second)
	for now := range ticker {
		log.Info("checking for stale chunks that need persisting.")
		now := uint32(now.Unix())
		chunkMinTs := now - (now % ms.chunkSpan) - uint32(ms.chunkMaxStale)
		metricMinTs := now - (now % ms.chunkSpan) - uint32(ms.metricMaxStale)

		// as this is the only goroutine that can delete from ms.Metrics
		// we only need to lock long enough to get the list of actives metrics.
		// it doesnt matter if new metrics are added while we iterate this list.
		ms.RLock()
		keys := make([]string, 0, len(ms.Metrics))
		for k := range ms.Metrics {
			keys = append(keys, k)
		}
		ms.RUnlock()
		for _, key := range keys {
			ms.RLock()
			a := ms.Metrics[key]
			ms.RUnlock()
			if stale := a.GC(chunkMinTs, metricMinTs); stale {
				log.Info("metric %s is stale. Purging data from memory.", key)
				delete(ms.Metrics, key)
			}
		}

	}
}

func (ms *AggMetrics) Get(key string) (Metric, bool) {
	ms.RLock()
	m, ok := ms.Metrics[key]
	ms.RUnlock()
	return m, ok
}

func (ms *AggMetrics) GetOrCreate(key string) Metric {
	ms.Lock()
	m, ok := ms.Metrics[key]
	if !ok {
		m = NewAggMetric(key, ms.chunkSpan, ms.numChunks, ms.aggSettings...)
		ms.Metrics[key] = m
	}
	ms.Unlock()
	return m
}