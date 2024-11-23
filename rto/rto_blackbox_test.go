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

type Res struct {
	rto time.Duration
	rtt time.Duration
}

func TestStartWithOutSLO(t *testing.T) {
	logger := logger.NewDefaultLogger()
	config := rto.Config{
		Id:             "test1",
		Max:            5 * time.Second,
		Min:            1 * time.Millisecond,
		SLOFailureRate: 0,
		Interval:       1 * time.Second,
	}

	normalRps := 800
	normalInterval := 1 * time.Second / time.Duration(normalRps)
	normalRtts := generateParetoSamples(2500, 5.0, 5)

	abnormalRps := 1500
	abnormalInterval := 1 * time.Second / time.Duration(abnormalRps)
	abnormalRtts := generateParetoSamples(2000, 5.0, 0.5)

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
		Id:             "test1",
		Max:            30 * time.Millisecond,
		Min:            1 * time.Millisecond,
		SLOFailureRate: 0.01,
		Interval:       5 * time.Second,
	}

	normalRps := 800
	normalInterval := time.Duration(float64(time.Second) / float64(normalRps))
	logger.Info(normalInterval.String())
	normalRtts := generateParetoSamples(50000, 5.0, 5)

	abnormalRps := 1500
	abnormalInterval := time.Duration(float64(time.Second) / float64(abnormalRps))
	logger.Info(abnormalInterval.String())
	abnormalRtts := generateParetoSamples(30000, 5.0, 1.5)

	var wg sync.WaitGroup
	resCh := make(chan Res)

	total := 0
	abtotal := 0
	failed := 0
	abfailed := 0
	var rtoSum int64 = 0
	var rttSum int64 = 0
	var abrtoSum int64 = 0
	var abrttSum int64 = 0

	for _, rtt := range normalRtts {
		time.Sleep(normalInterval)
		wg.Add(1)
		go simulateRequest(t, rtt, config, &wg, resCh)
	}
	for range normalRtts {
		res := <-resCh
		if res.rto <= res.rtt {
			failed++
		}
		total++
		rtoSum += res.rto.Milliseconds()
		rttSum += res.rtt.Milliseconds()
	}

	logger.Info("<--------------------------Beginning of Abnormal Load-------------------------->")

	for _, rtt := range abnormalRtts {
		time.Sleep(abnormalInterval)
		wg.Add(1)
		go simulateRequest(t, rtt, config, &wg, resCh)
	}
	for range abnormalRtts {
		res := <-resCh
		if res.rto <= res.rtt {
			failed++
			abfailed++
		}
		total++
		abtotal++
		rtoSum += res.rto.Milliseconds()
		rttSum += res.rtt.Milliseconds()
		abrtoSum += res.rto.Milliseconds()
		abrttSum += res.rtt.Milliseconds()
	}

	logger.Info("<--------------------------End of Abnormal Load-------------------------->")

	for _, rtt := range normalRtts {
		time.Sleep(normalInterval)
		wg.Add(1)
		go simulateRequest(t, rtt, config, &wg, resCh)
	}
	for range normalRtts {
		res := <-resCh
		if res.rto <= res.rtt {
			failed++
		}
		total++
		rtoSum += res.rto.Milliseconds()
		rttSum += res.rtt.Milliseconds()
	}

	wg.Wait()

	logger.Info("Complete",
		"fr", float64(failed)/float64(total),
		"ab_fr", float64(abfailed)/float64(abtotal),
		"avg_rtt", float64(rttSum)/float64(total),
		"avg_rto", float64(rtoSum)/float64(total),
		"ab_avg_rtt", float64(abrttSum)/float64(abtotal),
		"ab_avg_rto", float64(abrtoSum)/float64(abtotal),
	)
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

func simulateRequest(t *testing.T, rtt float64, config rto.Config, wg *sync.WaitGroup, resCh chan<- Res) {
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
	resCh <- Res{rto: timeout, rtt: rttD}
}
