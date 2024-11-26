// rto implements adaptive timeout algorithm used in TCP retransmission timeout.
package rto

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hanapedia/adapto/logger"
	"github.com/hanapedia/adapto/ring"
)

const (
	DEFAULT_BACKOFF      int64         = 2
	CONSERVATIVE_BACKOFF int64         = 1
	DEFAULT_K_MARGIN     int64         = 1
	DEFAULT_INTERVAL     time.Duration = 5 * time.Second
	DEFAULT_SLO          float64       = 0.1
	ALPHA_SCALING        int64         = 8
	LOG2_ALPHA           int64         = 3
	BETA_SCALING         int64         = 4
	LOG2_BETA            int64         = 2
	SLO_SAFETY_MARGIN    float64       = 0.5 // safety margin of 0.5 or division by 2
	MIN_FAILED_SAMPLES   float64       = 2
)

// DONE(v1.0.14): consider the raional of using negative duration for timedout requests
// so that the duration that just timed out can be transferred and used
// in that case, there is no need to define DeadlineExceeded.
// this will be breaking change for the users

// RttSignal is alias for time.Duration that is used for typing the channel used to report rtt.
type RttSignal = time.Duration

// State enum for main state machine
type RTOProviderState = int64

const (
	NORMAL RTOProviderState = iota
	OVERLOAD
)

type Config struct {
	Id             string
	Max            time.Duration // max timeout value allowed
	Min            time.Duration // min timeout value allowed
	SLOFailureRate float64       // target failure rate SLO
	Interval       time.Duration // interval for failure rate calculations
	KMargin        int64         // starting kMargin for with SLO and static kMargin for without SLO

	Logger logger.Logger // optional logger
}

func (c *Config) Validate() error {
	if c.Logger == nil {
		c.Logger = logger.NewDefaultLogger()
	}
	if c.Id == "" {
		return fmt.Errorf("Id is required")
	}
	if c.Max == 0 {
		return fmt.Errorf("Max is required")
	}
	if c.Min == 0 {
		return fmt.Errorf("Min is required")
	}
	if c.SLOFailureRate == 0 {
		return fmt.Errorf("SLO is required")
	}
	if c.SLOFailureRate != 0 && c.Interval == 0 {
		c.Logger.Info("SLO is provided but Interval is not, using the default interval", "interval", DEFAULT_INTERVAL)
		c.Interval = DEFAULT_INTERVAL
	}
	return nil
}

type AdaptoRTOProvider struct {
	// logger uses logger.DefaultLogger if not set
	logger logger.Logger

	// Main state machine
	state RTOProviderState

	// fields with synchronized access
	timeout time.Duration

	// values used in timeout calculations and adjusted dynamically
	srtt    int64 // smoothed rtt
	rttvar  int64
	kMargin int64   // extra margin multiplied to the origin K=4
	backoff int64   // backoff multiplier
	sfr     float64 // smoothed failure rate computed as moving average.
	lastFr  float64 // most recent failure rate

	// keep track of minimum RTT to fallback to
	minRtt time.Duration

	// counters for failure rate and inflight
	// inflight should be computed by req - recv
	// failure rate should be computed by failed / recv
	// these counters are cleared per interval
	req                  int64     // number of requests sent
	res                  int64     // number of responses received
	failed               int64     // number of requests failed
	carry                int64     // carry over from previous interval
	intervalStart        time.Time // timestamp of the beginning of the current interval
	overloadThresholdReq int64     // number of requests sent for the interval that caused state transition from NORMAL to OVERLOAD
	// TODO: this should be a slice to account for the cases where the max is greater than interval
	// when max > interval, the previous interval could already have request that is over the capacity,
	// but because overload can only be declared when response is observed,
	// if max is greater than interval, the request could have be from more than 1 interval ago
	/* lastNormalReq  int64                  // req from most recent normal interval */
	prevNormalReqs *ring.RingBuffer[int64] // ring buffer for recording previous numPrevNormalReqs reqs samples

	// mutex for synchronizing access to timeout calculation fields
	mu sync.Mutex

	// channel to receive the recorded rtt
	// in the event of timeout, sender should send `DeadlineExceeded` signal
	rttCh chan RttSignal

	sloFailureRateAdjusted float64 // slo with safety margin.
	minSamplesRequired     float64 // minimum samples required to compute failure rate
	sfrWeight              float64

	// configuration fields
	id             string
	min            time.Duration
	max            time.Duration
	sloFailureRate float64       // slo failure rate
	interval       time.Duration // interval for computing failure rate.
}

func NewAdaptoRTOProvider(config Config) *AdaptoRTOProvider {
	l := config.Logger
	if l == nil {
		l = logger.NewDefaultLogger()
	}
	kMargin := config.KMargin
	if kMargin == 0 {
		kMargin = DEFAULT_K_MARGIN
	}
	return &AdaptoRTOProvider{
		logger:  l,
		state:   NORMAL,
		timeout: config.Max,

		// use max for starting timeout.
		// timeout will be adjusted top-to-down to avoid volatility at startup
		srtt: int64(config.Max) * ALPHA_SCALING, // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
		rttvar: (int64(config.Max) >> 1) * BETA_SCALING, // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
		kMargin: kMargin,

		minRtt: config.Max,

		req:           0,
		res:           0,
		failed:        0,
		intervalStart: time.Now(),
		// ring buffer with default size of 2 + ceil(max / inteval)
		prevNormalReqs: ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.Max)/float64(config.Interval)))),

		sloFailureRateAdjusted: config.SLOFailureRate * SLO_SAFETY_MARGIN,
		minSamplesRequired:     MIN_FAILED_SAMPLES / (config.SLOFailureRate * SLO_SAFETY_MARGIN),
		sfrWeight:              float64(time.Minute / config.Interval), // 1 min / interval

		rttCh:          make(chan RttSignal),
		id:             config.Id,
		min:            config.Min,
		max:            config.Max,
		sloFailureRate: config.SLOFailureRate,
		interval:       config.Interval,
	}
}

// resetCounters resets counters
// req counter is reset to whatever the inflight was at this moment
func (arp *AdaptoRTOProvider) resetCounters() {
	// reset counters
	arp.carry = arp.inflight()
	arp.req = 0
	arp.res = 0
	arp.failed = 0
}

// CurrentReq extraporates current number of requests for the past interval using sliding window
// this should be used when overload is declared
// ref: https://blog.cloudflare.com/counting-things-a-lot-of-different-things/
// should lock rto updates
func (arp *AdaptoRTOProvider) CurrentReq() int64 {
	lastNormalReq := arp.prevNormalReqs.GetLast()
	previousReqEstimate := float64(lastNormalReq) * float64(arp.interval-time.Since(arp.intervalStart)) / float64(arp.interval)
	arp.logger.Debug("current rate computed",
		"req", arp.req,
		"lastNormalReq", lastNormalReq,
		"previousReqEstimate", previousReqEstimate,
		"sinceIntervalStart", time.Since(arp.intervalStart),
		"durationRatio", float64(time.Since(arp.intervalStart))/float64(arp.interval),
	)
	return int64(math.Round(previousReqEstimate)) + arp.req
}

// ChokeTimeout handles timeout update when transitioning to overload
// should lock rto updates
func (arp *AdaptoRTOProvider) ChokeTimeout() {
	// no need to compute srtt
	// use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
	// srtt = int64(arp.minRtt) * ALPHA_SCALING

	// use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
	rttvar := (int64(arp.minRtt) >> 1) * BETA_SCALING

	// compute rto with formula for first rtt observed.
	// use kMargin = 1
	// TODO: which kMargin should be used here
	rto := arp.minRtt + time.Duration(DEFAULT_K_MARGIN*rttvar) // because rtt = srtt / 8
	arp.timeout = min(max(rto, arp.min), arp.max)
}

// onRtt handles new rtt event
// increments counter
// if max timeout is breached, declares overload for new rtt
// else computes the new timeout for the rtt
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) onRtt(rtt time.Duration) {
	arp.res++ // increment res counter
	if rtt > 0 {
		arp.minRtt = min(rtt, arp.minRtt) // update minimum rtt
	} else {
		// increment failed counter
		arp.failed++
		rtt = -rtt
		arp.logger.Debug("DeadlineExceeded", "rto", rtt)
	}

	if arp.state == OVERLOAD {
		return
	}

	arp.ComputeNewRTO(rtt)
}

// onInterval calculates failure rate and adjusts margin
// failure rate for the interval is computed only if there were enough samples. if not interval exits.
// first failure rate for the current interval is computed, then smoothed mean failure rate
// if the main state machine is in NORMAL state:
//   - if failure rate is higher than the sloFailureRateAdjusted, kMargin is incremented
//   - if smoothed failure rate is lower than the sloFailureRateAdjusted, kMargin is decremented
//
// if the main state machine is in OVERLOAD state:
//   - kMargin is not updated no matter the failure rate
//   - if the req for current interval is smaller than overloadThresholdReq,
//     reset overloadThresholdReq, and set main state to NORMAL
//
// NOTE: this should be the only way margin is mutated
// NOTE: the state transition NORMAL -> OVERLOAD is not handled here
func (arp *AdaptoRTOProvider) onInterval() {
	// record next interval start
	defer func() {
		arp.intervalStart = time.Now()
	}()

	// account for the carry. use the previous failure rate to ESTIMATE the failed from for the carry
	resAdjusted := arp.res - arp.carry
	failedAdjusted := float64(arp.failed) - arp.lastFr*float64(arp.carry)

	// check if there were enough samples in the interval
	if resAdjusted <= int64(math.Round(arp.minSamplesRequired)) {
		arp.logger.Info("not enough samples", "resAdjusted", resAdjusted, "minSamplesRequired", arp.minSamplesRequired)
		return // do not reset counters
	}
	defer arp.resetCounters() // reset counters each interval

	fr := failedAdjusted / float64(resAdjusted) // failure rate for current interval
	arp.lastFr = fr                             // update previous failure rate
	if arp.sfr == 0 {
		// first observation of fr
		arp.sfr = fr
	} else {
		// compute smoothing weight by 1 min / interval, so it resembles something close to 1 min smoothing
		// update sfr only using fr from NORMAL state
		if arp.state == NORMAL {
			arp.sfr = arp.sfr + (fr-arp.sfr)/arp.sfrWeight
		}
	}
	arp.logger.Info("failure rate computed", "fr", fr, "sfr", arp.sfr)

	// handle NORMAL state
	if arp.state == NORMAL {
		arp.prevNormalReqs.Add(arp.req)
		// TODO: how to effectively decrement kMargin
		// if the fr for this interval is below threshold, must increment kMargin
		if fr >= arp.sloFailureRateAdjusted {
			arp.kMargin++
			arp.logger.Info("incrementing kMargin", "fr", fr, "sloAdjusted", arp.sloFailureRateAdjusted, "kMargin", arp.kMargin)
		} else {
			// if the smoothed fr is well over threshold, try decrementing kMargin
			if arp.sfr < arp.sloFailureRateAdjusted {
				arp.kMargin--
				arp.logger.Info("shrinking kMargin", "sfr", arp.sfr, "sloAdjusted", arp.sloFailureRateAdjusted, "kMargin", arp.kMargin)
			}
		}
		return
	}

	// handle OVERLOAD state
	// TODO: should consider if this threshold is rationale
	// TEST: record last req before overload start, and use the average as threshold
	if arp.overloadThresholdReq < arp.req {
		// stil in overload
		arp.logger.Info("still in overload", "overloadThresholdReq", arp.overloadThresholdReq, "req", arp.req)
		return
	}
	// undeclare overload
	arp.logger.Info("overload resolved", "overloadThresholdReq", arp.overloadThresholdReq, "req", arp.req)
	arp.overloadThresholdReq = 0
	arp.state = NORMAL
}

// calcInflight calculates current inflight requests
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) inflight() int64 {
	return arp.req - arp.res
}

var AdaptoRTOProviders map[string]*AdaptoRTOProvider

func init() {
	// initialize global provider map
	AdaptoRTOProviders = make(map[string]*AdaptoRTOProvider)
}

// GetTimeout retrieves timeout value using provider with given id in config.
// if no provider with matching id is found, creates a new provider
func GetTimeout(config Config) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
	provider, ok := AdaptoRTOProviders[config.Id]
	if !ok {
		err := config.Validate()
		if err != nil {
			return timeout, rttCh, err
		}
		provider = NewAdaptoRTOProvider(config)
		go provider.StartWithSLO()
		AdaptoRTOProviders[config.Id] = provider
	}
	timeout, rttCh = provider.NewTimeout()

	return timeout, rttCh, nil
}

// NewTimeout returns the current timeout value
// TODO: currently, timeouts are adjusted for every response and every capacity * interval requests.
// this means that the timeout cannot quickly adjuste to the sudden increase in requests until they return.
// consider adjusting the timeout value based on the current inflight
// could possibly use X = inflight / requestLimt
// higher the X, it is close to being overloaded
func (arp *AdaptoRTOProvider) NewTimeout() (timeout time.Duration, rttCh chan<- RttSignal) {
	arp.mu.Lock()
	defer arp.mu.Unlock()

	arp.req++ // increment req counter
	return arp.timeout, arp.rttCh
}

// ComputeNewRTO computes new rto based on new rtt
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) ComputeNewRTO(rtt time.Duration) {
	// boundary check
	if rtt < 0 {
		rtt *= -1
	}

	rto, srtt, rttvar := jacobsonCalc(int64(rtt), arp.srtt, arp.rttvar, arp.kMargin)
	rtoD := time.Duration(rto)
	// check if max timeout was not breachd
	if rtoD >= arp.max {
		// declare overload
		arp.ChokeTimeout()
		arp.state = OVERLOAD

		// precompute the threshold
		arp.overloadThresholdReq = (arp.CurrentReq() + int64(arp.prevNormalReqs.AverageNonZero())) >> 1
		arp.logger.Info("overload detected", "triggerRTO", rtoD, "chokedRTO", arp.timeout, "minRtt", arp.minRtt, "overloadThresholdReq", arp.overloadThresholdReq)
		return
	}

	// do not update these values when overload is detected
	arp.timeout = max(time.Duration(rto), arp.min) // no need to check for max because of early return
	arp.srtt = srtt
	arp.rttvar = rttvar
	arp.logger.Debug("new RTO computed", "rto", arp.timeout.String(), "rtt", rtt.String())
}

// StartWithSLO starts the provider by spawning a goroutine that waits for new rtt or timeout event and updates the timeout value accordingly. timeout calculations are also adjusted to meet the SLO
func (arp *AdaptoRTOProvider) StartWithSLO() {
	// ticker for computing failure rate
	ticker := time.NewTicker(arp.interval)
	for {
		select {
		case rtt := <-arp.rttCh:
			arp.mu.Lock()
			arp.onRtt(rtt)
			arp.mu.Unlock()
		case <-ticker.C:
			arp.mu.Lock()
			arp.onInterval()
			arp.mu.Unlock()
			continue
		}
	}
}

func jacobsonCalc(R, prevSrtt, prevRttvar, margin int64) (rto, srtt, rttvar int64) {
	err := R - (prevSrtt >> LOG2_ALPHA) // R = R - (srtt / 8)
	srtt = prevSrtt + err               // srtt = srtt + R - (srtt / 8)
	if err < 0 {
		err = -err
	}
	err = err - (prevRttvar >> LOG2_BETA) // R = |R - (srtt / 8)| - (rttvar / 4)
	rttvar = prevRttvar + err             // rttvar = rttvar + |R - (srtt / 8)| - (rttvar / 4)

	// srtt + 4 * rttvar
	// rttvar must be scaled by 1/4, cancelling out 4
	rto = (srtt >> LOG2_ALPHA) + margin*rttvar
	return rto, srtt, rttvar
}
