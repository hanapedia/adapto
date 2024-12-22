// rto implements adaptive timeout algorithm used in TCP retransmission timeout.
package rto

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/hanapedia/adapto/logger"
	"github.com/hanapedia/adapto/ring"
)

type RTOWCQProviderState = int64

// state transition will be
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
const (
	WCQ_CRUISE RTOWCQProviderState = iota
	WCQ_STARTUP
	WCQ_DRAIN
)

type AdaptoRTOWCQProvider struct {
	// logger uses logger.DefaultLogger if not set
	logger logger.Logger

	// Main state machine
	state RTOWCQProviderState

	// timeout value returned when new timeout is requested
	timeout time.Duration

	// values used in timeout calculations and adjusted dynamically
	srtt    int64   // smoothed rtt
	rttvar  int64   // variance of rtt
	kMargin int64   // extra margin multiplied to the origin K=4
	sfr     float64 // smoothed failure rate computed as moving average.
	lastFr  float64 // most recent failure rate. not smoothed

	// keep track of minimum RTT to fallback to
	minRtt time.Duration

	// per Interval counters
	// these counters are cleared per interval
	req           int64     // number of requests sent
	res           int64     // number of responses received
	failed        int64     // number of requests failed. Only timeout failure are counted
	genericErr    int64     // number of generic errs. these should not count towards internal failure rate
	carry         int64     // carry over from previous interval
	intervalStart time.Time // timestamp of the beginning of the current interval

	// states for startup
	startupIntervalsRemaining uint64 // counter for the number of startup intervals remaining, decremented when failure rate meets the target.

	// states for overload control
	overloadThresholdReq int64                   // number of requests allowed in an interval during overload
	prevSuccRess         *ring.RingBuffer[int64] // ring buffer for recording previous successful res samples
	prevReqs             *ring.RingBuffer[int64] // ring buffer for recording previous reqs samples

	// mutex for synchronizing access to timeout calculation fields
	mu sync.Mutex

	// channel to receive the recorded rtt
	// in the event of timeout, sender should send `DeadlineExceeded` signal
	rttCh chan RttSignal

	// fields computed at initialization with config values to reduce computation
	sloFailureRateAdjusted float64 // slo with safety margin. Depends on sloFailureRate.
	minSamplesRequired     float64 // minimum samples required to compute failure rate. Depends on sloFailureRate.
	sfrWeight              float64 // smoothing factor for computing long-term failure rate. Depends on interval.
	term                   int64   // current number of terms in sfr intervals

	// configuration fields
	id                      string
	min                     time.Duration
	sloLatency              time.Duration
	sloFailureRate          float64       // slo failure rate
	interval                time.Duration // interval for computing failure rate.
	overloadDetectionTiming OverloadDetectionTiming
	// handler function to be called at the end of every interval
	// WARNING: this is called after the counter has been reset
	onIntervalHandler func(AdaptoRTOProviderInterface)
}

func NewAdaptoRTOWCQProvider(config Config) *AdaptoRTOWCQProvider {
	l := config.Logger
	if l == nil {
		l = logger.NewDefaultLogger()
	}
	kMargin := config.KMargin
	if kMargin == 0 {
		kMargin = DEFAULT_K_MARGIN
	}
	return &AdaptoRTOWCQProvider{
		logger:  l,
		state:   STARTUP,
		timeout: config.SLOLatency,

		kMargin: kMargin,

		minRtt: config.SLOLatency,

		startupIntervalsRemaining: DEFAULT_STARTUP_INTERVALS,

		intervalStart: time.Now(),
		// ring buffer with default size of 2 + ceil(max / inteval)
		prevSuccRess: ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.SLOLatency)/float64(config.Interval)))),
		prevReqs:     ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.SLOLatency)/float64(config.Interval)))),

		overloadDetectionTiming: config.OverloadDetectionTiming,

		sloFailureRateAdjusted: config.SLOFailureRate * SLO_SAFETY_MARGIN,
		minSamplesRequired:     MIN_FAILED_SAMPLES / (config.SLOFailureRate * SLO_SAFETY_MARGIN),
		sfrWeight:              float64(time.Minute / config.Interval), // 1 min / interval

		rttCh:             make(chan RttSignal),
		id:                config.Id,
		min:               config.Min,
		sloLatency:        config.SLOLatency,
		sloFailureRate:    config.SLOFailureRate,
		interval:          config.Interval,
		onIntervalHandler: config.OnIntervalHandler,
	}
}

// TransitionState conditionally updates the main state to CRUISE
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
func (arp *AdaptoRTOWCQProvider) transitionToCruise() {
	if arp.state == WCQ_STARTUP {
		if arp.startupIntervalsRemaining == 0 {
			arp.logger.Info(fmt.Sprintf("transitioning from %s to WCQ_CRUISE", StateAsString(arp.state)),
				"id", arp.id,
			)
			arp.state = WCQ_CRUISE
		}
		return
	}
	if arp.state == WCQ_DRAIN {
		arp.logger.Info(fmt.Sprintf("transitioning from %s to WCQ_CRUISE", StateAsString(arp.state)),
			"id", arp.id,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"req", arp.req,
		)
		arp.overloadThresholdReq = 0
		arp.state = WCQ_CRUISE
		return
	}
	arp.logger.Error("Cannot transition to CRUISE", "currState", StateAsString(arp.state))
	os.Exit(1)
}

// TransitionState conditionally updates the main state to DRAIN
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
func (arp *AdaptoRTOWCQProvider) transitionToDrain() {
	if arp.state == WCQ_CRUISE || arp.state == WCQ_STARTUP {
		arp.chokeTimeout()
		// define threshold using the res count instead of req
		// take max with 1 to ensure that at least 1 request is sent even during failure
		arp.overloadThresholdReq = max(arp.currentReq(), 1)
		arp.logger.Info(fmt.Sprintf("transitioning from %s to WCQ_DRAIN", StateAsString(arp.state)),
			"id", arp.id,
			"chokedRTO", arp.timeout,
			"minRtt", arp.minRtt,
			"initialOverloadThresholdReq", arp.overloadThresholdReq,
		)
		arp.state = DRAIN
		return
	}
	arp.logger.Error("Cannot transition to DRAIN", "currState", StateAsString(arp.state))
	os.Exit(1)
}

// resetCounters resets counters
// req counter is reset to whatever the inflight was at this moment
func (arp *AdaptoRTOWCQProvider) resetCounters() {
	// reset counters
	arp.carry = arp.inflight()
	arp.req = 0
	arp.res = 0
	arp.failed = 0
	arp.genericErr = 0
}

// currentRes computes the estimate of instantaneous goodput per interval.
// the reference time for the sliding window is the moment this method is called.
func (arp *AdaptoRTOWCQProvider) currentRes() int64 {
	lastSuccRes := arp.prevSuccRess.GetLast()
	previousResEstimate := float64(lastSuccRes) * float64(arp.interval-time.Since(arp.intervalStart)) / float64(arp.interval)
	arp.logger.Debug("current rate computed",
		"id", arp.id,
		"res", arp.res,
		"lastSuccRes", lastSuccRes,
		"previousResEstimate", previousResEstimate,
		"sinceIntervalStart", time.Since(arp.intervalStart),
		"durationRatio", float64(time.Since(arp.intervalStart))/float64(arp.interval),
	)
	return int64(math.Round(previousResEstimate)) + arp.succeeded()
}

// currentReq computes the estimate of instantaneous offered throughput per interval.
// the reference time for the sliding window is the moment this method is called.
func (arp *AdaptoRTOWCQProvider) currentReq() int64 {
	lastReq := arp.prevReqs.GetLast()
	previousResEstimate := float64(lastReq) * float64(arp.interval-time.Since(arp.intervalStart)) / float64(arp.interval)
	arp.logger.Debug("current rate computed",
		"id", arp.id,
		"res", arp.res,
		"lastSuccRes", lastReq,
		"previousResEstimate", previousResEstimate,
		"sinceIntervalStart", time.Since(arp.intervalStart),
		"durationRatio", float64(time.Since(arp.intervalStart))/float64(arp.interval),
	)
	return int64(math.Round(previousResEstimate)) + arp.req
}

// chokeTimeout handles timeout update when transitioning to overload
// should lock rto updates
func (arp *AdaptoRTOWCQProvider) chokeTimeout() {
	// no need to compute srtt
	// use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
	// srtt = int64(arp.minRtt) * ALPHA_SCALING

	// use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
	rttvar := (int64(arp.minRtt) >> 1) * BETA_SCALING

	// compute rto with formula for first rtt observed.
	rto := arp.minRtt + time.Duration(arp.kMargin*rttvar) // because rtt = srtt / 8
	arp.timeout = min(max(rto, arp.min), arp.sloLatency)
}

// MEMO: shouldn't kMargin consider the difference between srtt and slo latency?
// kMargin should not be too big s.t. timeout <- srtt * kMargin * 4 * rttvar
func (arp *AdaptoRTOWCQProvider) updateKMarginShortTerm(fr float64) {
	if fr >= arp.sloFailureRateAdjusted {
		arp.kMargin++
		arp.logger.Info("incrementing kMargin (short-term)",
			"id", arp.id,
			"fr", fr,
			"sloAdjusted", arp.sloFailureRateAdjusted,
			"kMargin", arp.kMargin,
		)
	}
}
func (arp *AdaptoRTOWCQProvider) updateKMarginLongTerm() {
	// use non-adjusted slo for long-term adjusments
	if arp.sfr >= arp.sloFailureRate {
		arp.kMargin++
		arp.logger.Info("incrementing kMargin (long-term)",
			"id", arp.id,
			"sfr", arp.sfr,
			"sloAdjusted", arp.sloFailureRateAdjusted,
			"kMargin", arp.kMargin,
		)
	}
}

// instead of updating kmargin, directly double the timeout
// this should be called after the new timeout for the interval is computed
func (arp *AdaptoRTOWCQProvider) doubleTimeout(fr float64, timeout time.Duration) time.Duration {
	if fr >= arp.sloFailureRateAdjusted {
		timeout *= 2
		arp.logger.Info("doubling timeout",
			"id", arp.id,
			"fr", fr,
			"sloAdjusted", arp.sloFailureRateAdjusted,
			"timeout", timeout,
		)
	}
	return min(timeout, arp.sloLatency)
}

// calcInflight calculates current inflight requests.
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOWCQProvider) inflight() int64 {
	return arp.req - arp.res
}

// if failed from the last period is zero, returns true no matter the sample counts
// this allows skipping
func (arp *AdaptoRTOWCQProvider) hasEnoughSamples() bool {
	return arp.failed == 0 || arp.res-arp.carry >= int64(math.Round(arp.minSamplesRequired))
}

func (arp *AdaptoRTOWCQProvider) computeFailure() float64 {
	// account for the carry. use the previous failure rate to ESTIMATE the failed from for the carry
	resAdjusted := arp.res - arp.carry
	failedAdjusted := float64(arp.failed)
	if failedAdjusted != 0 {
		failedAdjusted -= max(arp.lastFr*float64(arp.carry), 0)
	}

	fr := failedAdjusted / float64(resAdjusted) // failure rate for current interval
	arp.lastFr = fr                             // update previous failure rate
	if arp.sfr == 0 {
		// first observation of fr
		arp.sfr = fr
	} else {
		// compute smoothing weight by 1 min / interval, so it resembles something close to 1 min smoothing
		// update sfr only using fr from NORMAL state
		if arp.state == CRUISE {
			arp.sfr = arp.sfr + (fr-arp.sfr)/arp.sfrWeight
		}
	}
	arp.logger.Info("failure rate computed",
		"id", arp.id,
		"fr", fr,
		"sfr", arp.sfr,
	)
	return fr
}

// succeeded calculates the successful responses
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOWCQProvider) succeeded() int64 {
	return arp.res - arp.failed - arp.genericErr
}

// NewTimeout returns the current timeout value
// pseudo client queue / rate limiting suspends timeout creation during overload
func (arp *AdaptoRTOWCQProvider) NewTimeout(ctx context.Context) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	arp.req++ // increment req counter
	rttCh = arp.rttCh

	switch arp.state {
	case WCQ_STARTUP:
		// startup intervals, where timeout, srtt, and rttvar are updated,
		// but sloLatency is used to minimize the ACTUAL failure rate.
		// the internal failure rate tracked by arp.failed should be updated using the updated timeout value
		return arp.sloLatency, arp.rttCh, nil

	case WCQ_CRUISE:
		// timeout is returned as is
		return arp.timeout, arp.rttCh, nil

	case WCQ_DRAIN:
		// timeout is returned as is, assuming that it is already choked
		return arp.timeout, rttCh, nil
	}

	return arp.timeout, rttCh, nil
}

// ComputeNewRTO computes new rto based on new rtt
// new timeout value is returned and can be used to set timeouts or overload detection
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOWCQProvider) ComputeNewRTO(rtt time.Duration) time.Duration {
	// boundary check
	if rtt < 0 {
		rtt *= -1
	}
	if arp.srtt == 0 {
		// first observation of rtt
		arp.srtt = int64(rtt) * ALPHA_SCALING                   // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
		arp.rttvar = (int64(rtt) >> 1) * BETA_SCALING           // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
		rto := rtt + time.Duration(DEFAULT_K_MARGIN*arp.rttvar) // because rtt = srtt / 8
		timeout := min(max(rto, arp.min), arp.sloLatency)
		arp.logger.Debug("new RTO computed",
			"id", arp.id,
			"rto", timeout.String(),
			"rtt", rtt.String(),
		)
		return timeout
	}

	rto, srtt, rttvar := jacobsonCalc(int64(rtt), arp.srtt, arp.rttvar, arp.kMargin)

	// do not update these values when overload is detected
	arp.srtt = srtt
	arp.rttvar = rttvar
	timeout := min(max(time.Duration(rto), arp.min), arp.sloLatency)
	arp.logger.Debug("new RTO computed",
		"id", arp.id,
		"rto", timeout.String(),
		"rtt", rtt.String(),
	)
	return timeout
}

// StartWithSLO starts the provider by spawning a goroutine that waits for new rtt or timeout event and updates the timeout value accordingly. timeout calculations are also adjusted to meet the SLO
func (arp *AdaptoRTOWCQProvider) StartWithSLO() {
	// ticker for computing failure rate
	ticker := time.NewTicker(arp.interval)
	for {
		select {
		case rtt := <-arp.rttCh:
			arp.mu.Lock()
			arp.OnRtt(rtt)
			arp.mu.Unlock()
		case <-ticker.C:
			arp.mu.Lock()
			arp.OnInterval()
			arp.mu.Unlock()
			continue
		}
	}
}

// OnRtt handles new rtt event
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOWCQProvider) OnRtt(signal RttSignal) {
	arp.res++ // increment res counter
	if signal.Type == GenericError {
		// increment failed counter and return
		arp.genericErr++
		arp.logger.Debug("Generic Error",
			"id", arp.id,
			"rto", signal.Duration,
		)
		return
	}
	if signal.Type == Successful {
		arp.minRtt = min(signal.Duration, arp.minRtt) // update minimum rtt
		// startup intervals, where timeout, srtt, and rttvar are updated,
		// but sloLatency is used to minimize the ACTUAL failure rate.
		// the internal failure rate tracked by arp.failed should be updated using the updated timeout value
		if arp.state == STARTUP && arp.timeout < signal.Duration {
			arp.failed++
		}
	}
	if signal.Type == TimeoutError {
		// increment failed counter
		arp.failed++
		arp.logger.Debug("Timeout Error",
			"id", arp.id,
			"rto", signal.Duration,
		)
	}
	switch arp.state {
	case WCQ_STARTUP:
		arp.ComputeNewRTO(signal.Duration)
		return
	case WCQ_CRUISE:
		arp.ComputeNewRTO(signal.Duration)
	case WCQ_DRAIN:
		arp.ComputeNewRTO(signal.Duration)
		return
	}
}

// OnInterval calculates failure rate and adjusts margin
// MUST be called with mu.Lock already acquired
func (arp *AdaptoRTOWCQProvider) OnInterval() {
	// record next interval start
	defer func() {
		arp.intervalStart = time.Now()
		arp.onIntervalHandler(arp)
	}()
	// record current res
	arp.prevReqs.Add(arp.res)

	switch arp.state {
	case WCQ_STARTUP:
		arp.prevSuccRess.Add(arp.succeeded())
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"resAdjusted", arp.res-arp.carry,
				"minSamplesRequired", arp.minSamplesRequired,
			)
			return
		}
		defer arp.resetCounters() // reset counters each interval
		fr := arp.computeFailure()
		arp.updateKMarginShortTerm(fr)
		arp.timeout = arp.ComputeNewRTO(time.Duration(arp.srtt >> LOG2_ALPHA))
		if arp.timeout == arp.sloLatency {
			arp.transitionToDrain()
		}
		if fr < arp.sloFailureRateAdjusted {
			// startup intervals, where timeout, srtt, and rttvar are updated,
			// but sloLatency is used to minimize the ACTUAL failure rate.
			// the internal failure rate tracked by arp.failed should be updated using the updated timeout value
			arp.startupIntervalsRemaining--
			if arp.startupIntervalsRemaining == 0 {
				arp.transitionToCruise()
			}
		}

		return
	case WCQ_CRUISE:
		arp.prevSuccRess.Add(arp.succeeded())
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"resAdjusted", arp.res-arp.carry,
				"minSamplesRequired", arp.minSamplesRequired,
			)
			return
		}
		defer arp.resetCounters() // reset counters each interval
		fr := arp.computeFailure()
		timeout := arp.ComputeNewRTO(time.Duration(arp.srtt >> LOG2_ALPHA))
		// use which ever the bigger. the prvious timout or new timeout before try doubling
		if fr >= arp.sloFailureRateAdjusted {
			arp.timeout = arp.doubleTimeout(fr, max(arp.timeout, timeout))
		} else {
			arp.timeout = max(arp.timeout, timeout)
		}
		if arp.timeout == arp.sloLatency {
			arp.transitionToDrain()
		}

		// apply long-term update to kmargin to adjust from the initial value decided in STARTUP
		arp.term++
		if arp.term == int64(arp.sfrWeight) {
			arp.updateKMarginLongTerm()
			arp.term = 0
		}

		return
	case WCQ_DRAIN:
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"resAdjusted", arp.res-arp.carry,
				"minSamplesRequired", arp.minSamplesRequired,
			)
			return
		}
		defer arp.resetCounters() // reset counters each interval

		// a simple threshold check
		if arp.currentReq() < arp.overloadThresholdReq {
			// undeclare overload
			// no need to reset variables as they are all reset at the beginning of interval
			// reset threshold and send rate interval
			arp.transitionToCruise()
			return
		}
		return
	}
}

var AdaptoRTOWCQProviders map[string]*AdaptoRTOWCQProvider

func init() {
	// initialize global provider map
	AdaptoRTOWCQProviders = make(map[string]*AdaptoRTOWCQProvider)
}

// getTimeoutWCQ retrieves timeout value using provider with given id in config.
// if no provider with matching id is found, creates a new provider
func getTimeoutWCQ(ctx context.Context, config Config) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
	provider, ok := AdaptoRTOWCQProviders[config.Id]
	if !ok {
		err := config.Validate()
		if err != nil {
			return timeout, rttCh, err
		}
		provider = NewAdaptoRTOWCQProvider(config)
		go provider.StartWithSLO()
		AdaptoRTOWCQProviders[config.Id] = provider
	}
	timeout, rttCh, err = provider.NewTimeout(ctx)

	return timeout, rttCh, err
}

// CapacityEstimate returns current overload estimate
func (arp *AdaptoRTOWCQProvider) CapacityEstimate() int64 {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	return arp.overloadThresholdReq
}

// State returns current state
func (arp *AdaptoRTOWCQProvider) State() string {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	return StateAsString(arp.state)
}
