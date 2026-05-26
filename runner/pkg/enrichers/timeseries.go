package enrichers

import (
	"math"
)

// timeSeries is a fixed-step series of float64 samples backed by a NaN-filled
// slice.
//
// Use newTimeSeries with the start time and total point count; call set(t,v)
// to fill samples; reduce(f, init) walks the points and accumulates.
type timeSeries struct {
	fromTime int64
	step     int64
	data     []float64
}

func newTimeSeries(fromTime int64, points int, step int64) *timeSeries {
	if points < 0 {
		points = 0
	}
	d := make([]float64, points)
	nan := math.NaN()
	for i := range d {
		d[i] = nan
	}
	return &timeSeries{fromTime: fromTime, step: step, data: d}
}

// set writes value v at unix-time t. Out-of-range writes are ignored.
func (s *timeSeries) set(t int64, v float64) {
	if s == nil || s.step == 0 || t < s.fromTime {
		return
	}
	idx := int((t - s.fromTime) / s.step)
	if idx < 0 || idx >= len(s.data) {
		return
	}
	s.data[idx] = v
}

func (s *timeSeries) last() float64 {
	if s == nil || len(s.data) == 0 {
		return math.NaN()
	}
	return s.data[len(s.data)-1]
}

// reduce walks the points and returns the accumulated value. Pass nan-aware
// reducers (rMax, rNanSum, rMin) so NaN samples don't poison the result.
func (s *timeSeries) reduce(f func(acc, v float64) float64) float64 {
	if s == nil {
		return math.NaN()
	}
	acc := math.NaN()
	for _, v := range s.data {
		acc = f(acc, v)
	}
	return acc
}

// merge applies f pairwise across two TimeSeries with matching grid (same
// fromTime + step + length). Our use case only ever has compatible grids
// so we skip resampling.
func mergeSeries(dest, ts *timeSeries, f func(acc, v float64) float64) *timeSeries {
	switch {
	case dest == nil && ts == nil:
		return nil
	case dest == nil:
		return ts
	case ts == nil:
		return dest
	}
	n := len(dest.data)
	if len(ts.data) < n {
		n = len(ts.data)
	}
	for i := 0; i < n; i++ {
		dest.data[i] = f(dest.data[i], ts.data[i])
	}
	return dest
}

// rMax / rNanSum / rMin / rLast mirror the backend's timeseries reducers.
// Each handles NaN cleanly:
//   - max/min ignore NaN-then-real, real-then-NaN; NaN+NaN stays NaN
//   - nan_sum treats NaN as 0
func rMax(acc, v float64) float64 {
	if math.IsNaN(acc) {
		return v
	}
	if math.IsNaN(v) {
		return acc
	}
	if v > acc {
		return v
	}
	return acc
}

func rMin(acc, v float64) float64 {
	if math.IsNaN(acc) {
		return v
	}
	if math.IsNaN(v) {
		return acc
	}
	if v < acc {
		return v
	}
	return acc
}

func rNanSum(acc, v float64) float64 {
	if math.IsNaN(acc) {
		acc = 0
	}
	if !math.IsNaN(v) {
		acc += v
	}
	return acc
}
