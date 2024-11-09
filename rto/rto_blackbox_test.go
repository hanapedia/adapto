package rto_test

import (
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/hanapedia/adapto/logger"
	"github.com/hanapedia/adapto/rto"
	"github.com/stretchr/testify/assert"
)

func TestStartWithOutSLO(t *testing.T) {
	logger := logger.NewDefaultLogger()
	config := rto.Config{
		Id:       "test1",
		Max:      5 * time.Second,
		Min:      1 * time.Millisecond,
		SLO:      0,
		Capacity: 1000,
		Interval: 1 * time.Second,
	}

	normalRps := 800
	normalInterval := 1 * time.Second / time.Duration(normalRps)
	normalRtts := generateParetoSamples(2500, 5.0, 5)

	abnormalRps := 1500
	abnormalInterval := 1 * time.Second / time.Duration(abnormalRps)
	abnormalRtts := generateParetoSamples(2000, 5.0, 1.5)

	var wg sync.WaitGroup

	for _, rtt := range normalRtts {
		time.Sleep(normalInterval)
		wg.Add(1)
		go func(rtt float64) {
			timeout, rttCh, err := rto.GetTimeout(config)
			assert.NoError(t, err, "Error should be nil for GetTimeout")
			rttD := time.Duration(rtt) * time.Millisecond
			if timeout < rttD {
				time.Sleep(timeout)
				logger.Error("timeout exceeded", "rto", timeout, "rrtD", rttD.String())
				rttCh <- -timeout
				wg.Done()
			} else {
				time.Sleep(rttD)
				rttCh <- rttD
				wg.Done()
			}
		}(rtt)
	}

	logger.Info("<--------------------------Beginning of Abnormal Load-------------------------->")

	for _, rtt := range abnormalRtts {
		time.Sleep(abnormalInterval)
		wg.Add(1)
		go func(rtt float64) {
			timeout, rttCh, err := rto.GetTimeout(config)
			assert.NoError(t, err, "Error should be nil for GetTimeout")
			rttD := time.Duration(rtt) * time.Millisecond
			if timeout < rttD {
				time.Sleep(timeout)
				logger.Error("timeout exceeded", "rto", timeout, "rrtD", rttD.String())
				rttCh <- -timeout
				wg.Done()
			} else {
				time.Sleep(rttD)
				rttCh <- rttD
				wg.Done()
			}
		}(rtt)
	}

	logger.Info("<--------------------------End of Abnormal Load-------------------------->")

	for _, rtt := range normalRtts {
		time.Sleep(normalInterval)
		wg.Add(1)
		go func(rtt float64) {
			timeout, rttCh, err := rto.GetTimeout(config)
			assert.NoError(t, err, "Error should be nil for GetTimeout")
			rttD := time.Duration(rtt) * time.Millisecond
			if timeout < rttD {
				time.Sleep(timeout)
				logger.Error("timeout exceeded", "rto", timeout, "rrtD", rttD.String())
				rttCh <- -timeout
				wg.Done()
			} else {
				time.Sleep(rttD)
				rttCh <- rttD
				wg.Done()
			}
		}(rtt)
	}

	wg.Wait()
}


func TestStartWithSLO(t *testing.T) {
	logger := logger.NewDefaultLogger()
	config := rto.Config{
		Id:       "test1",
		Max:      5 * time.Second,
		Min:      1 * time.Millisecond,
		SLO:      0.01,
		Capacity: 1000,
		Interval: 1 * time.Second,
	}

	normalRps := 800
	normalInterval := 1 * time.Second / time.Duration(normalRps)
	normalRtts := generateParetoSamples(50000, 5.0, 5)

	abnormalRps := 1500
	abnormalInterval := 1 * time.Second / time.Duration(abnormalRps)
	abnormalRtts := generateParetoSamples(2000, 5.0, 1.5)

	var wg sync.WaitGroup

	for _, rtt := range normalRtts {
		time.Sleep(normalInterval)
		wg.Add(1)
		go func(rtt float64) {
			timeout, rttCh, err := rto.GetTimeout(config)
			assert.NoError(t, err, "Error should be nil for GetTimeout")
			rttD := time.Duration(rtt) * time.Millisecond
			if timeout < rttD {
				time.Sleep(timeout)
				/* logger.Error("timeout exceeded", "rto", timeout, "rrtD", rttD.String()) */
				rttCh <- -timeout
				wg.Done()
			} else {
				time.Sleep(rttD)
				rttCh <- rttD
				wg.Done()
			}
		}(rtt)
	}

	logger.Info("<--------------------------Beginning of Abnormal Load-------------------------->")

	for _, rtt := range abnormalRtts {
		time.Sleep(abnormalInterval)
		wg.Add(1)
		go func(rtt float64) {
			timeout, rttCh, err := rto.GetTimeout(config)
			assert.NoError(t, err, "Error should be nil for GetTimeout")
			rttD := time.Duration(rtt) * time.Millisecond
			if timeout < rttD {
				time.Sleep(timeout)
				/* logger.Error("timeout exceeded", "rto", timeout, "rrtD", rttD.String()) */
				rttCh <- -timeout
				wg.Done()
			} else {
				time.Sleep(rttD)
				rttCh <- rttD
				wg.Done()
			}
		}(rtt)
	}

	logger.Info("<--------------------------End of Abnormal Load-------------------------->")

	for _, rtt := range normalRtts {
		time.Sleep(normalInterval)
		wg.Add(1)
		go func(rtt float64) {
			timeout, rttCh, err := rto.GetTimeout(config)
			assert.NoError(t, err, "Error should be nil for GetTimeout")
			rttD := time.Duration(rtt) * time.Millisecond
			if timeout < rttD {
				time.Sleep(timeout)
				/* logger.Error("timeout exceeded", "rto", timeout, "rrtD", rttD.String()) */
				rttCh <- -timeout
				wg.Done()
			} else {
				time.Sleep(rttD)
				rttCh <- rttD
				wg.Done()
			}
		}(rtt)
	}

	wg.Wait()
}

// generateNormalSamples generates a slice of n int64 samples from a normal distribution
// with a specified mean and standard deviation.
func generateNormalSamples(n int, mean, std float64) []float64 {
	samples := make([]float64, n)
	for i := 0; i < n; i++ {
		// Generate a normally distributed sample, scale it, and shift by mean
		sample := mean + std*rand.NormFloat64()
		samples[i] = sample
	}
	return samples
}

// generateParetoSamples generates a slice of n samples with a heavy-tailed Pareto distribution.
func generateParetoSamples(n int, scale, shape float64) []float64 {
	samples := make([]float64, n)
	for i := 0; i < n; i++ {
		U := rand.Float64()
		samples[i] = scale / math.Pow(1-U, 1/shape)
	}
	return samples
}
