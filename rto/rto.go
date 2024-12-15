// rto implements adaptive timeout algorithm used in TCP retransmission timeout.
package rto

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hanapedia/adapto/logger"
	"github.com/hanapedia/adapto/ring"
)

const (
	// Configurable parameters
	DEFAULT_K_MARGIN                  int64                   = 1
	DEFAULT_INTERVAL                  time.Duration           = 5 * time.Second
	DEFAULT_OVERLOAD_DETECTION_TIMING OverloadDetectionTiming = MaxTimeoutExceeded

	// Constant parameters. Alpha and Beta for Retransmission Timeout
	// Defined by  V. Jacobson, “Congestion avoidance and control,” SIGCOMM Comput.Commun.
	ALPHA_SCALING int64 = 8
	LOG2_ALPHA    int64 = 3
	BETA_SCALING  int64 = 4
	LOG2_BETA     int64 = 2

	// Tunable parameters
	// The values set for these constant leaves room for tuning
	SLO_SAFETY_MARGIN                float64 = 0.5 // safety margin of 0.5 or division by 2
	MIN_FAILED_SAMPLES               float64 = 1   // minimum failed samples reqruired to compute failure rate
	LOG2_PACING_GAIN                 int64   = 5   // used as pacing gain factor. 1+ 1 >> LOG2_PACING_GAIN
	DEFAULT_OVERLOAD_DRAIN_INTERVALS uint64  = 2   // intervals to choke rto after overload. TODO: reduce this
)

// RttSignal is used for reporting rtt.
type RttSignal struct {
	Duration time.Duration
	Type     SignalType
}

type SignalType = int64

const (
	Successful SignalType = iota
	// GenericError should be used to signal non-timeout errors.
	// this only counts the failed but do not record rtt
	GenericError
	TimeoutError
)

// State enum for main state machine
type RTOProviderState = int64

const (
	NORMAL RTOProviderState = iota
	OVERLOAD
)

type OverloadDetectionTiming = string

const (
	MaxTimeoutGenerated OverloadDetectionTiming = "maxTimeoutGenerated"
	MaxTimeoutExceeded  OverloadDetectionTiming = "maxTimeoutExceeded"
)

var RequestRateLimitExceeded error = requestRateLimitExceeded{}

type requestRateLimitExceeded struct{}

func (requestRateLimitExceeded) Error() string { return "request rate limit exceeded" }

type ConfigValidationError struct {
	msg string
}

func (c ConfigValidationError) Error() string {
	return fmt.Sprintf("config validation error: msg=%s", c.msg)
}

func DefaultOnIntervalHandler(*AdaptoRTOProvider) {}

type Config struct {
	Id                      string
	SLOLatency              time.Duration            // max timeout value allowed
	Min                     time.Duration            // min timeout value allowed
	SLOFailureRate          float64                  // target failure rate SLO
	Interval                time.Duration            // interval for failure rate calculations
	KMargin                 int64                    // starting kMargin for with SLO and static kMargin for without SLO
	OverloadDetectionTiming OverloadDetectionTiming  // timing when to check for overload
	OverloadDrainIntervals  uint64                   // number of intervals to drain overloading requests
	OnIntervalHandler       func(*AdaptoRTOProvider) // handler to be called at the end of every interval

	Logger logger.Logger // optional logger
}

func (c *Config) Validate() *ConfigValidationError {
	if c.Logger == nil {
		c.Logger = logger.NewDefaultLogger()
	}
	if c.Id == "" {
		return &ConfigValidationError{msg: "Id is required"}
	}
	if c.SLOLatency == 0 {
		return &ConfigValidationError{msg: "Max is required"}
	}
	if c.Min == 0 {
		return &ConfigValidationError{msg: "Min is required"}
	}
	if c.SLOFailureRate == 0 {
		return &ConfigValidationError{msg: "SLO is required"}
	}
	if c.SLOFailureRate != 0 && c.Interval == 0 {
		c.Logger.Info("SLO is provided but Interval is not, using the default interval",
			"id", c.Id,
			"interval", DEFAULT_INTERVAL,
		)
		c.Interval = DEFAULT_INTERVAL
	}
	if c.OverloadDrainIntervals == 0 {
		c.OverloadDrainIntervals = DEFAULT_OVERLOAD_DRAIN_INTERVALS
	}
	switch c.OverloadDetectionTiming {
	case MaxTimeoutExceeded:
	case MaxTimeoutGenerated:
		break
	default:
		c.Logger.Info("SLO is provided but OverloadDetectionTiming is invalid, using the default timing",
			"id", c.Id,
			"timing", DEFAULT_OVERLOAD_DETECTION_TIMING,
		)
		c.OverloadDetectionTiming = DEFAULT_OVERLOAD_DETECTION_TIMING
	}
	if c.OnIntervalHandler == nil {
		c.OnIntervalHandler = DefaultOnIntervalHandler
	}
	return nil
}

type AdaptoRTOProvider struct {
	// logger uses logger.DefaultLogger if not set
	logger logger.Logger

	// Main state machine
	state RTOProviderState

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
	failed        int64     // number of requests failed
	carry         int64     // carry over from previous interval
	dropped       int64     // number of request dropped due to sending rate control
	intervalStart time.Time // timestamp of the beginning of the current interval

	// states for overload control
	overloadThresholdReq            int64                   // number of requests allowed in an interval during overload
	queueLength                     int64                   // number of requests SCHEDULED & suspended, waiting for their turn
	sendRateInterval                time.Duration           // pacing interval for controlling sending rate at overloadThresholdReq / interval
	overloadDrainIntervalsRemaining uint64                  // counter for the number of drain intervals remaining
	consecutivePacingGains          uint64                  // counter for the number of consecutive pacing gains
	prevSuccRess                    *ring.RingBuffer[int64] // ring buffer for recording previous successful res samples
	prevReqs                        *ring.RingBuffer[int64] // ring buffer for recording previous reqs samples

	// mutex for synchronizing access to timeout calculation fields
	mu sync.Mutex

	// channel to receive the recorded rtt
	// in the event of timeout, sender should send `DeadlineExceeded` signal
	rttCh chan RttSignal

	// fields computed at initialization with config values to reduce computation
	sloFailureRateAdjusted float64 // slo with safety margin. Depends on sloFailureRate.
	minSamplesRequired     float64 // minimum samples required to compute failure rate. Depends on sloFailureRate.
	sfrWeight              float64 // smoothing factor for computing long-term failure rate. Depends on interval.

	// configuration fields
	id             string
	min            time.Duration
	sloLatency     time.Duration
	sloFailureRate float64       // slo failure rate
	interval       time.Duration // interval for computing failure rate.
	// TODO: could consider other timings
	overloadDetectionTiming OverloadDetectionTiming
	overloadDrainIntervals  uint64
	// handler function to be called at the end of every interval
	// WARNING: this is called after the counter has been reset
	onIntervalHandler func(*AdaptoRTOProvider)
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
		timeout: config.SLOLatency,

		kMargin: kMargin,

		minRtt: config.SLOLatency,

		intervalStart: time.Now(),
		// ring buffer with default size of 2 + ceil(max / inteval)
		prevSuccRess: ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.SLOLatency)/float64(config.Interval)))),
		prevReqs:     ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.SLOLatency)/float64(config.Interval)))),

		overloadDetectionTiming: config.OverloadDetectionTiming,

		sloFailureRateAdjusted: config.SLOFailureRate * SLO_SAFETY_MARGIN,
		minSamplesRequired:     MIN_FAILED_SAMPLES / (config.SLOFailureRate * SLO_SAFETY_MARGIN),
		sfrWeight:              float64(time.Minute / config.Interval), // 1 min / interval

		rttCh:                  make(chan RttSignal),
		id:                     config.Id,
		min:                    config.Min,
		sloLatency:             config.SLOLatency,
		sloFailureRate:         config.SLOFailureRate,
		interval:               config.Interval,
		overloadDrainIntervals: config.OverloadDrainIntervals,
		onIntervalHandler:      config.OnIntervalHandler,
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
	arp.dropped = 0
}

// CurrentRes computes the estimate of instantaneous goodput per interval.
// the reference time for the sliding window is the moment this method is called.
func (arp *AdaptoRTOProvider) CurrentRes() int64 {
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

// CurrentReq computes the estimate of instantaneous offered throughput per interval.
// the reference time for the sliding window is the moment this method is called.
func (arp *AdaptoRTOProvider) CurrentReq() int64 {
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

// CapacityEstimate returns current overload estimate
func (arp *AdaptoRTOProvider) CapacityEstimate() int64 {
	return arp.overloadThresholdReq
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
	// TODO: whether to use DEFAULT_K_MARGIN or the adjusted kMargin is up for debate.
	// choke timeout will be called when server is likely to have built queues
	// timoeut is choked for overloadDrainIntervals intervals and then starts adjusting in overload
	/* rto := arp.minRtt + time.Duration(arp.kMargin*rttvar) // because rtt = srtt / 8 */
	rto := arp.minRtt + time.Duration(DEFAULT_K_MARGIN*rttvar) // because rtt = srtt / 8
	arp.timeout = min(max(rto, arp.min), arp.sloLatency)
}

// onRtt handles new rtt event
// increments counter
// if max timeout is breached, declares overload for new rtt
// else computes the new timeout for the rtt
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) onRtt(signal RttSignal) {
	arp.res++ // increment res counter
	if signal.Type == GenericError {
		// increment failed counter and return
		arp.failed++
		arp.logger.Debug("Generic Error",
			"id", arp.id,
			"rto", signal.Duration,
		)
		return
	}
	if signal.Type == Successful {
		arp.minRtt = min(signal.Duration, arp.minRtt) // update minimum rtt
	}
	if signal.Type == TimeoutError {
		// increment failed counter
		arp.failed++
		arp.logger.Debug("Timeout Error",
			"id", arp.id,
			"rto", signal.Duration,
		)
	}

	// skip timeout update when overload
	if arp.state == OVERLOAD {
		// only start updating rto some intervals after overload declaration
		// until then, update only srtt and rttvar with choked timeout
		arp.ComputeNewRTO(signal.Duration, arp.overloadDrainIntervalsRemaining == 0)
		// check that the timeout leaves enough room to fit at least 1 req
		// if not, it is likely to be sending too fast, either because
		// 1. capacity estimate was recently increased
		// 2. capacity of the destination decreased due to other clients
		if arp.timeout > arp.sloLatency-arp.sendRateInterval {

			// rechoke and drain
			arp.ChokeTimeout()
			arp.overloadDrainIntervalsRemaining = arp.overloadDrainIntervals
			// enforce pacing rate
			/* arp.overloadThresholdReq = max(arp.CurrentRes(), 2) */
			/* arp.sendRateInterval = arp.interval / time.Duration(arp.overloadThresholdReq) */
			arp.logger.Info("rto maxed out, draining",
				"id", arp.id,
				"rto", arp.timeout.String(),
				"rtt", signal.Duration.String(),
			)
		}
		return
	}

	// Declare overload if max timeout is breached
	if arp.overloadDetectionTiming == MaxTimeoutExceeded && signal.Duration == arp.sloLatency {
		arp.onOverload(signal.Duration)
		return
	}

	// compute new timeout with rtt
	arp.ComputeNewRTO(signal.Duration, true)

	// Declare overload if max timeout is generated
	if arp.overloadDetectionTiming == MaxTimeoutGenerated && arp.timeout == arp.sloLatency {
		arp.onOverload(signal.Duration)
	}
}

// onOverload handles the state updates when overload is detected
func (arp *AdaptoRTOProvider) onOverload(rtt time.Duration) {
	// declare overload
	arp.ChokeTimeout()
	arp.state = OVERLOAD

	// increment so that first interval check is not skipped even if dropped is 0
	arp.overloadDrainIntervalsRemaining = arp.overloadDrainIntervals

	// define threshold using the res count instead of req
	// take max with 1 to ensure that at least 1 request is sent even during failure
	// TODO: move the initial computation of sending rate interval to when drain interval is over
	arp.overloadThresholdReq = max(arp.CurrentReq(), 2)
	arp.sendRateInterval = arp.interval / time.Duration(arp.overloadThresholdReq)
	arp.logger.Info("overload detected",
		"id", arp.id,
		"triggerRTO", rtt,
		"chokedRTO", arp.timeout,
		"minRtt", arp.minRtt,
		"initialOverloadThresholdReq", arp.overloadThresholdReq,
		"initialSendRateInterval", arp.sendRateInterval,
	)
	return
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
//   - return immediately if in draining stage.
//   - if dropped is 0, reset overloadThresholdReq, and set main state to NORMAL
//   - otherwise check if the failure rate excluding dropped is withing the SLO.
//   - if yes, attempt to grow the threshold, and increase sending rate
//   - if not, shrink the threshold
//
// NOTE: this should be the only way margin is mutated
// NOTE: the state transition NORMAL -> OVERLOAD is not handled here
func (arp *AdaptoRTOProvider) onInterval() {
	// record next interval start
	defer func() {
		arp.intervalStart = time.Now()
		arp.onIntervalHandler(arp)
	}()
	// record current res
	arp.prevReqs.Add(arp.res)

	// account for the carry. use the previous failure rate to ESTIMATE the failed from for the carry
	resAdjusted := arp.res - arp.carry
	failedAdjusted := float64(arp.failed) - arp.lastFr*float64(arp.carry)

	// check if there were enough samples in the interval
	if resAdjusted <= int64(math.Round(arp.minSamplesRequired)) {
		arp.logger.Info("not enough samples",
			"id", arp.id,
			"resAdjusted", resAdjusted,
			"minSamplesRequired", arp.minSamplesRequired,
		)
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
	arp.logger.Info("failure rate computed",
		"id", arp.id,
		"fr", fr,
		"sfr", arp.sfr,
	)

	// handle NORMAL state
	// adjusting kMargin
	if arp.state == NORMAL {
		arp.prevSuccRess.Add(arp.succeeded())
		// if the fr for this interval is below threshold, must increment kMargin
		if fr >= arp.sloFailureRateAdjusted {
			arp.kMargin++
			arp.logger.Info("incrementing kMargin",
				"id", arp.id,
				"fr", fr,
				"sloAdjusted", arp.sloFailureRateAdjusted,
				"kMargin", arp.kMargin,
			)
		} else {
			// if the smoothed fr is well over threshold, try decrementing kMargin
			if arp.sfr < arp.sloFailureRateAdjusted {
				arp.kMargin--
				arp.logger.Info("shrinking kMargin",
					"id", arp.id,
					"sfr", arp.sfr,
					"sloAdjusted", arp.sloFailureRateAdjusted,
					"kMargin", arp.kMargin,
				)
			}
		}
		return
	}

	// handle OVERLOAD state
	// skip interval if still draining
	if arp.overloadDrainIntervalsRemaining > 0 {
		arp.overloadDrainIntervalsRemaining--
		if arp.overloadDrainIntervalsRemaining == 0 {
			arp.overloadThresholdReq = max(arp.succeeded(), 2)
			arp.sendRateInterval = arp.interval / time.Duration(arp.overloadThresholdReq)
			arp.logger.Info("overload draining intervals complete, enforcing pacing",
				"id", arp.id,
				"overloadThresholdReq", arp.overloadThresholdReq,
				"sendRateInterval", arp.sendRateInterval,
			)
		}
		return
	}
	// TODO: should consider if this threshold is rationale
	// adjusting pacing
	if arp.dropped > 0 {
		// stil in overload
		if fr >= arp.sloFailureRateAdjusted {
			// shrink pacing to res
			arp.overloadThresholdReq = max(arp.succeeded(), 2) // make sure to max with 2, not 1
			arp.sendRateInterval = arp.interval / time.Duration(
				arp.overloadThresholdReq,
			)
			arp.consecutivePacingGains = 0 // reset consecutive gains
			arp.logger.Info("still in overload, shrinking pacing",
				"id", arp.id,
				"sendRateInterval", arp.sendRateInterval,
				"overloadThresholdReq", arp.overloadThresholdReq,
				"dropped", arp.dropped,
				"req", arp.req,
			)
			return
		}
		// gain pacing by x1.125 x 2^consecutivePacingGains
		// this helps faster recovery of pacing from circuit broken state
		arp.overloadThresholdReq += max(max(arp.overloadThresholdReq>>LOG2_PACING_GAIN, 1)<<arp.consecutivePacingGains, 2)
		arp.sendRateInterval = arp.interval / time.Duration(
			arp.overloadThresholdReq,
		)
		arp.consecutivePacingGains++
		arp.logger.Info("still in overload, growing pacing",
			"id", arp.id,
			"sendRateInterval", arp.sendRateInterval,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"dropped", arp.dropped,
			"req", arp.req,
		)
		return
	}

	// undeclare overload
	// no need to reset variables as they are all reset at the beginning of interval
	arp.state = NORMAL
	// reset threshold and send rate interval
	arp.overloadThresholdReq = 0
	arp.sendRateInterval = 0
	arp.logger.Info("overload resolved",
		"id", arp.id,
		"overloadThresholdReq", arp.overloadThresholdReq,
		"dropped", arp.dropped,
		"req", arp.req,
	)
}

// calcInflight calculates current inflight requests.
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) inflight() int64 {
	// make sure to subtract dropped
	return arp.req - arp.res - arp.dropped
}

// succeeded calculates the successful responses
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) succeeded() int64 {
	return arp.res - arp.failed
}

var AdaptoRTOProviders map[string]*AdaptoRTOProvider

func init() {
	// initialize global provider map
	AdaptoRTOProviders = make(map[string]*AdaptoRTOProvider)
}

// GetTimeout retrieves timeout value using provider with given id in config.
// if no provider with matching id is found, creates a new provider
func GetTimeout(ctx context.Context, config Config) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
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
	timeout, rttCh, err = provider.NewTimeout(ctx)

	return timeout, rttCh, err
}

// NewTimeout returns the current timeout value
// pseudo client queue / rate limiting suspends timeout creation during overload
func (arp *AdaptoRTOProvider) NewTimeout(ctx context.Context) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	arp.req++ // increment req counter
	rttCh = arp.rttCh

	// if in overload state, compute (max timeout - suspend timeout)
	// if this is lower than arp.timeout (which will also be computed dynamically with ComputeNewRTO), return err
	// suspend timeout should be computed as nextSchedulable - time.Now()
	// nextSchedulable = schedule interval * requests already in line + time.Now()
	// suspend = arp.schedulingInterval * arp.queueLength
	if arp.state == OVERLOAD {
		// TODO: consider waiting for the draining intervals before applying pacing
		// it could improve the accuracy of the initial capacity estimation
		// it could also prevent overly cautious control at relatively milder overload
		if arp.overloadDrainIntervalsRemaining > 0 {
			return arp.timeout, rttCh, nil
		}

		suspend := arp.sendRateInterval * time.Duration(arp.queueLength)
		adjustedTimeout := arp.sloLatency - suspend
		if adjustedTimeout < arp.timeout {
			arp.dropped++
			arp.logger.Debug("new timeout dropped",
				"id", arp.id,
				"queueLength", arp.queueLength,
				"supend", suspend,
				"dropped", arp.dropped,
			)
			return time.Duration(0), rttCh, RequestRateLimitExceeded
		}

		arp.queueLength++

		// return the timeout without suspending
		// no need to subtract queueLength to avoid multiplication by zero
		if suspend == 0 {
			return arp.timeout, rttCh, nil
		}

		arp.mu.Unlock() // unlock while suspended

		suspendTimer := time.NewTimer(suspend)
		select {
		case <-suspendTimer.C:
			arp.mu.Lock() // reaquire lock, this wait could add up to suspend
			arp.queueLength--
			return adjustedTimeout, rttCh, nil
		case <-ctx.Done():
			arp.mu.Lock()
			arp.queueLength--
			return time.Duration(0), rttCh, ctx.Err()
		}

	}

	return arp.timeout, arp.rttCh, nil
}

// ComputeNewRTO computes new rto based on new rtt
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) ComputeNewRTO(rtt time.Duration, updateTimeout bool) {
	// boundary check
	if rtt < 0 {
		rtt *= -1
	}
	if arp.srtt == 0 {
		// first observation of rtt
		arp.srtt = int64(rtt) * ALPHA_SCALING                   // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
		arp.rttvar = (int64(rtt) >> 1) * BETA_SCALING           // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
		rto := rtt + time.Duration(DEFAULT_K_MARGIN*arp.rttvar) // because rtt = srtt / 8
		arp.timeout = min(max(rto, arp.min), arp.sloLatency)
		arp.logger.Debug("new RTO computed",
			"id", arp.id,
			"rto", arp.timeout.String(),
			"rtt", rtt.String(),
		)
		return
	}

	rto, srtt, rttvar := jacobsonCalc(int64(rtt), arp.srtt, arp.rttvar, arp.kMargin)

	// do not update these values when overload is detected
	arp.srtt = srtt
	arp.rttvar = rttvar
	if !updateTimeout {
		return
	}
	arp.timeout = min(max(time.Duration(rto), arp.min), arp.sloLatency)
	arp.logger.Debug("new RTO computed",
		"id", arp.id,
		"rto", arp.timeout.String(),
		"rtt", rtt.String(),
	)
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
