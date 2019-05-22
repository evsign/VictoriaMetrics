package promql

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmselect/netstorage"
	"github.com/VictoriaMetrics/metrics"
)

// ExpandWithExprs expands WITH expressions inside q and returns the resulting
// PromQL without WITH expressions.
func ExpandWithExprs(q string) (string, error) {
	e, err := parsePromQLWithCache(q)
	if err != nil {
		return "", err
	}
	buf := e.AppendString(nil)
	return string(buf), nil
}

// Exec executes q for the given ec until the deadline.
func Exec(ec *EvalConfig, q string) ([]netstorage.Result, error) {
	ec.validate()

	e, err := parsePromQLWithCache(q)
	if err != nil {
		return nil, err
	}

	// Add an additional point to the end. This point is used
	// in calculating the last value for rate, deriv, increase
	// and delta funcs.
	ec.End += ec.Step

	rv, err := evalExpr(ec, e)
	if err != nil {
		return nil, err
	}

	// Remove the additional point at the end.
	for _, ts := range rv {
		ts.Values = ts.Values[:len(ts.Values)-1]

		// ts.Timestamps may be shared between timeseries, so truncate it with len(ts.Values) instead of len(ts.Timestamps)-1
		ts.Timestamps = ts.Timestamps[:len(ts.Values)]
	}
	ec.End -= ec.Step

	maySort := maySortResults(e, rv)
	result, err := timeseriesToResult(rv, maySort)
	if err != nil {
		return nil, err
	}
	return result, err
}

func maySortResults(e expr, tss []*timeseries) bool {
	if len(tss) > 100 {
		// There is no sense in sorting a lot of results
		return false
	}
	fe, ok := e.(*funcExpr)
	if !ok {
		return true
	}
	switch fe.Name {
	case "sort", "sort_desc":
		return false
	default:
		return true
	}
}

func timeseriesToResult(tss []*timeseries, maySort bool) ([]netstorage.Result, error) {
	tss = removeNaNs(tss)
	result := make([]netstorage.Result, len(tss))
	m := make(map[string]bool)
	bb := bbPool.Get()
	for i, ts := range tss {
		bb.B = marshalMetricNameSorted(bb.B[:0], &ts.MetricName)
		if m[string(bb.B)] {
			return nil, fmt.Errorf(`duplicate output timeseries: %s%s`, ts.MetricName.MetricGroup, stringMetricName(&ts.MetricName))
		}
		m[string(bb.B)] = true

		rs := &result[i]
		rs.MetricNameMarshaled = append(rs.MetricNameMarshaled[:0], bb.B...)
		rs.MetricName.CopyFrom(&ts.MetricName)
		rs.Values = append(rs.Values[:0], ts.Values...)
		rs.Timestamps = append(rs.Timestamps[:0], ts.Timestamps...)
	}
	bbPool.Put(bb)

	if maySort {
		sort.Slice(result, func(i, j int) bool {
			return string(result[i].MetricNameMarshaled) < string(result[j].MetricNameMarshaled)
		})
	}

	return result, nil
}

func removeNaNs(tss []*timeseries) []*timeseries {
	rvs := tss[:0]
	for _, ts := range tss {
		nans := 0
		for _, v := range ts.Values {
			if math.IsNaN(v) {
				nans++
			}
		}
		if nans == len(ts.Values) {
			// Skip timeseries with all NaNs.
			continue
		}
		rvs = append(rvs, ts)
	}
	return rvs
}

func parsePromQLWithCache(q string) (expr, error) {
	pcv := parseCacheV.Get(q)
	if pcv == nil {
		e, err := parsePromQL(q)
		pcv = &parseCacheValue{
			e:   e,
			err: err,
		}
		parseCacheV.Put(q, pcv)
	}
	if pcv.err != nil {
		return nil, pcv.err
	}
	return pcv.e, nil
}

var parseCacheV = func() *parseCache {
	pc := &parseCache{
		m: make(map[string]*parseCacheValue),
	}
	metrics.NewGauge(`vm_cache_requests_total{type="promql/parse"}`, func() float64 {
		return float64(pc.Requests())
	})
	metrics.NewGauge(`vm_cache_misses_total{type="promql/parse"}`, func() float64 {
		return float64(pc.Misses())
	})
	metrics.NewGauge(`vm_cache_entries{type="promql/parse"}`, func() float64 {
		return float64(pc.Len())
	})
	return pc
}()

const parseCacheMaxLen = 10e3

type parseCacheValue struct {
	e   expr
	err error
}

type parseCache struct {
	m  map[string]*parseCacheValue
	mu sync.RWMutex

	requests uint64
	misses   uint64
}

func (pc *parseCache) Requests() uint64 {
	return atomic.LoadUint64(&pc.requests)
}

func (pc *parseCache) Misses() uint64 {
	return atomic.LoadUint64(&pc.misses)
}

func (pc *parseCache) Len() uint64 {
	pc.mu.RLock()
	n := len(pc.m)
	pc.mu.RUnlock()
	return uint64(n)
}

func (pc *parseCache) Get(q string) *parseCacheValue {
	atomic.AddUint64(&pc.requests, 1)

	pc.mu.RLock()
	pcv := pc.m[q]
	pc.mu.RUnlock()

	if pcv == nil {
		atomic.AddUint64(&pc.misses, 1)
	}
	return pcv
}

func (pc *parseCache) Put(q string, pcv *parseCacheValue) {
	pc.mu.Lock()
	overflow := len(pc.m) - parseCacheMaxLen
	if overflow > 0 {
		// Remove 10% of items from the cache.
		overflow = int(float64(len(pc.m)) * 0.1)
		for k := range pc.m {
			delete(pc.m, k)
			overflow--
			if overflow <= 0 {
				break
			}
		}
	}
	pc.m[q] = pcv
	pc.mu.Unlock()
}
