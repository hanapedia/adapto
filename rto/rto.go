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

const (
	// Configurable parameters
	DEFAULT_K_MARGIN                  int64                   = 1
	DEFAULT_INTERVAL                  time.Duration           = 5 * time.Second
	DEFAULT_OVERLOAD_DETECTION_TIMING OverloadDetectionTiming = MaxTimeoutGenerated

	// Constant parameters. Alpha and Beta for Retransmission Timeout
	// Defined by  V. Jacobson, “Congestion avoidance and control,” SIGCOMM Comput.Commun.
	ALPHA_SCALING int64 = 8
	LOG2_ALPHA    int64 = 3
	BETA_SCALING  int64 = 4
	LOG2_BETA     int64 = 2

	// Tunable parameters
	// The values set for these constant leaves room for tuning
	SLO_SAFETY_MARGIN                            float64 = 0.5 // safety margin of 0.5 or division by 2
	MIN_FAILED_SAMPLES                           float64 = 1   // minimum failed samples reqruired to compute failure rate
	LOG2_PACING_GAIN                             int64   = 5   // used as pacing gain factor. 1+ 1 >> LOG2_PACING_GAIN
	DEFAULT_OVERLOAD_DRAIN_INTERVALS             uint64  = 2   // intervals to choke rto after overload.
	DEFAULT_STARTUP_INTERVALS                    uint64  = 4   // intervals to wait for the kMargin to stabilize
	DEFAULT_CAPACITY_INCREMENT_COOLDOWN          uint64  = 6   // intervals to wait for incrementing capacity
	FAILURE_RECOVERY_CAPACITY_INCREMENT_COOLDOWN uint64  = 1   // intervals to wait for incrementing capacity
)

type AdaptoRTOProviderInterface interface {
	NewTimeout(ctx context.Context) (timeout time.Duration, rttCh chan<- RttSignal, err error)
	CapacityEstimate() int64
	State() string
}

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

// state transition will be
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
const (
	CRUISE RTOProviderState = iota
	STARTUP
	DRAIN
	OVERLOAD
	FAILURE
)

func StateAsString(state RTOProviderState) string {
	switch state {
	case CRUISE:
		return "CRUISE"
	case STARTUP:
		return "STARTUP"
	case DRAIN:
		return "DRAIN"
	case OVERLOAD:
		return "OVERLOAD"
	case FAILURE:
		return "FAILURE"
	}
	return "UNSUPPORTED"
}

func StateAsIOTA(state string) RTOProviderState {
	switch state {
	case "CRUISE":
		return CRUISE
	case "STARTUP":
		return STARTUP
	case "DRAIN":
		return DRAIN
	case "OVERLOAD":
		return OVERLOAD
	case "FAILURE":
		return FAILURE
	}
	return -1
}

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

func DefaultOnIntervalHandler(AdaptoRTOProviderInterface) {}

type Config struct {
	Id                      string
	SLOLatency              time.Duration                    // max timeout value allowed
	Min                     time.Duration                    // min timeout value allowed
	SLOFailureRate          float64                          // target failure rate SLO
	Interval                time.Duration                    // interval for failure rate calculations
	KMargin                 int64                            // starting kMargin for with SLO and static kMargin for without SLO
	OverloadDetectionTiming OverloadDetectionTiming          // timing when to check for overload
	OverloadDrainIntervals  uint64                           // number of intervals to drain overloading requests
	OnIntervalHandler       func(AdaptoRTOProviderInterface) // handler to be called at the end of every interval
	DisableClientQueuing    bool                             //flag for feature gating client queuing

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
		c.Logger.Info("OverloadDetectionTiming is Deprecated. overload is detected per interval",
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
	req        int64 // number of requests sent
	res        int64 // number of responses received
	failed     int64 // number of requests failed. Only timeout failure are counted
	genericErr int64 // number of generic errs. these should not count towards internal failure rate
	/* carry         int64     // carry over from previous interval */
	dropped       int64     // number of request dropped due to sending rate control
	intervalStart time.Time // timestamp of the beginning of the current interval

	// states for startup
	startupIntervalsRemaining uint64 // counter for the number of startup intervals remaining, decremented when failure rate meets the target.

	// states for overload control
	overloadThresholdReq               int64                   // number of requests allowed in an interval during overload
	queueLength                        int64                   // number of requests SCHEDULED & suspended, waiting for their turn
	sendRateInterval                   time.Duration           // pacing interval for controlling sending rate at overloadThresholdReq / interval
	overloadDrainIntervalsRemaining    uint64                  // counter for the number of drain intervals remaining
	consecutivePacingGains             uint64                  // counter for the number of consecutive pacing gains
	lastCapacityIncrement              int64                   // record of the last increment to capacity estimate
	capacityIncrementCoolDown          uint64                  // starting value for counter for the number of capacity increment cooldowns
	capacityIncrementCoolDownRemaining uint64                  // counter for the number of capacity increment cooldowns
	prevSuccRess                       *ring.RingBuffer[int64] // ring buffer for recording previous successful res samples
	prevReqs                           *ring.RingBuffer[int64] // ring buffer for recording previous reqs samples
	nextTransimssion                   time.Time               // timestap for next transmission when sendRate is enforced

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
	overloadDrainIntervals  uint64
	// handler function to be called at the end of every interval
	// WARNING: this is called after the counter has been reset
	onIntervalHandler func(AdaptoRTOProviderInterface)
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
		state:   STARTUP,
		timeout: config.SLOLatency,

		kMargin: kMargin,

		minRtt: config.SLOLatency,

		startupIntervalsRemaining: DEFAULT_STARTUP_INTERVALS,

		intervalStart: time.Now(),
		// ring buffer with default size of 2 + ceil(max / inteval)
		prevSuccRess:     ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.SLOLatency)/float64(config.Interval)))),
		prevReqs:         ring.NewRingBuffer[int64](2 + int(math.Ceil(float64(config.SLOLatency)/float64(config.Interval)))),
		nextTransimssion: time.Now(),

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

// TransitionState conditionally updates the main state to CRUISE
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
func (arp *AdaptoRTOProvider) transitionToCruise() {
	if arp.state == STARTUP {
		if arp.startupIntervalsRemaining == 0 {
			arp.logger.Info(fmt.Sprintf("transitioning from %s to CRUISE", StateAsString(arp.state)),
				"id", arp.id,
			)
			arp.state = CRUISE
		}
		return
	}
	if arp.state == OVERLOAD {
		arp.logger.Info(fmt.Sprintf("transitioning from %s to CRUISE", StateAsString(arp.state)),
			"id", arp.id,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"dropped", arp.dropped,
			"req", arp.req,
		)
		arp.overloadThresholdReq = 0
		arp.sendRateInterval = 0
		arp.state = CRUISE
		return
	}
	arp.logger.Error("Cannot transition to CRUISE", "currState", StateAsString(arp.state))
	os.Exit(1)
}

// TransitionState conditionally updates the main state to DRAIN
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
func (arp *AdaptoRTOProvider) transitionToDrain() {
	if arp.state == CRUISE || arp.state == STARTUP {
		arp.chokeTimeout()
		// increment so that first interval check is not skipped even if dropped is 0
		arp.overloadDrainIntervalsRemaining = arp.overloadDrainIntervals

		// define threshold using the res count instead of req
		// take max with 1 to ensure that at least 1 request is sent even during failure
		arp.overloadThresholdReq = max(arp.currentReq(), 1)
		arp.sendRateInterval = arp.interval / time.Duration(arp.overloadThresholdReq)
		arp.logger.Info(fmt.Sprintf("transitioning from %s to DRAIN", StateAsString(arp.state)),
			"id", arp.id,
			"chokedRTO", arp.timeout,
			"minRtt", arp.minRtt,
			"initialOverloadThresholdReq", arp.overloadThresholdReq,
			"initialSendRateInterval", arp.sendRateInterval,
			"overloadDetectionTiming", arp.overloadDetectionTiming,
		)
		arp.state = DRAIN
		return
	}
	if arp.state == OVERLOAD {
		arp.chokeTimeout()
		arp.overloadDrainIntervalsRemaining = arp.overloadDrainIntervals
		// pacing is not updated here as it will be updated after drain by overload
		arp.logger.Info(fmt.Sprintf("transitioning from %s to DRAIN", StateAsString(arp.state)),
			"id", arp.id,
			"chokedRTO", arp.timeout,
			"minRtt", arp.minRtt,
			"initialOverloadThresholdReq", arp.overloadThresholdReq,
			"initialSendRateInterval", arp.sendRateInterval,
			"overloadDetectionTiming", arp.overloadDetectionTiming,
		)
		arp.state = DRAIN
		return
	}
	arp.logger.Error("Cannot transition to DRAIN", "currState", StateAsString(arp.state))
	os.Exit(1)
}

// TransitionState conditionally updates the main state to OVERLOAD
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
func (arp *AdaptoRTOProvider) transitionToOverload() {
	if arp.state == DRAIN {
		arp.queueLength = 0
		arp.consecutivePacingGains = 0
		arp.lastCapacityIncrement = 0
		arp.capacityIncrementCoolDown = DEFAULT_CAPACITY_INCREMENT_COOLDOWN
		arp.capacityIncrementCoolDownRemaining = arp.capacityIncrementCoolDown
		arp.overloadThresholdReq = max(arp.succeeded(), 1)
		arp.sendRateInterval = arp.interval / time.Duration(arp.overloadThresholdReq)
		arp.logger.Info(fmt.Sprintf("transitioning from %s to OVERLOAD", StateAsString(arp.state)),
			"id", arp.id,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"sendRateInterval", arp.sendRateInterval,
		)
		arp.state = OVERLOAD
		return
	}
	if arp.state == FAILURE {
		arp.queueLength = 0
		arp.consecutivePacingGains = 0
		arp.lastCapacityIncrement = 0
		arp.capacityIncrementCoolDown = FAILURE_RECOVERY_CAPACITY_INCREMENT_COOLDOWN
		arp.capacityIncrementCoolDownRemaining = arp.capacityIncrementCoolDown
		// use the last recorded successful request rate as reference
		arp.overloadThresholdReq = max(arp.prevSuccRess.GetLast(), 1)
		arp.sendRateInterval = arp.interval / time.Duration(arp.overloadThresholdReq)
		arp.logger.Info(fmt.Sprintf("transitioning from %s to OVERLOAD", StateAsString(arp.state)),
			"id", arp.id,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"sendRateInterval", arp.sendRateInterval,
		)
		arp.state = OVERLOAD
	}
	arp.logger.Error("Cannot transition to OVERLOAD", "currState", StateAsString(arp.state))
	os.Exit(1)
}

// TransitionState conditionally updates the main state to FAILURE
// STARTUP -> CRUISE -> DRAIN <-> OVERLOAD <-> FAILURE
// OVERLOAD -> CRUISE
func (arp *AdaptoRTOProvider) transitionToFailure() {
	if arp.state == OVERLOAD {
		arp.logger.Info(fmt.Sprintf("transitioning from %s to FAILURE", StateAsString(arp.state)),
			"id", arp.id,
		)
		arp.state = FAILURE
		return
	}
	arp.logger.Error("Cannot transition to FAILURE", "currState", StateAsString(arp.state))
	os.Exit(1)
}

// resetCounters resets counters
// req counter is reset to whatever the inflight was at this moment
func (arp *AdaptoRTOProvider) resetCounters() {
	// reset counters
	arp.req = 0
	arp.res = 0
	arp.failed = 0
	arp.genericErr = 0
	arp.dropped = 0
}

// currentRes computes the estimate of instantaneous goodput per interval.
// the reference time for the sliding window is the moment this method is called.
func (arp *AdaptoRTOProvider) currentRes() int64 {
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
func (arp *AdaptoRTOProvider) currentReq() int64 {
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
func (arp *AdaptoRTOProvider) chokeTimeout() {
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
func (arp *AdaptoRTOProvider) updateKMarginShortTerm(fr float64) {
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
func (arp *AdaptoRTOProvider) updateKMarginLongTerm() {
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
func (arp *AdaptoRTOProvider) doubleTimeout(fr float64, timeout time.Duration) time.Duration {
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

func (arp *AdaptoRTOProvider) updateCapacityEstimateOrDoubleTimeout(fr float64) {
	// stil in overload
	if fr >= arp.sloFailureRateAdjusted {
		// pacing increased too much
		if arp.lastCapacityIncrement > 0 {
			arp.consecutivePacingGains = 0
			arp.overloadThresholdReq -= arp.lastCapacityIncrement
			arp.sendRateInterval = arp.interval / time.Duration(
				arp.overloadThresholdReq,
			)
			arp.lastCapacityIncrement = 0
			// reset cooldown
			arp.capacityIncrementCoolDownRemaining = arp.capacityIncrementCoolDown
			arp.logger.Info("still in overload, sending too fast, shrinking pacing",
				"id", arp.id,
				"sendRateInterval", arp.sendRateInterval,
				"overloadThresholdReq", arp.overloadThresholdReq,
				"dropped", arp.dropped,
				"req", arp.req,
				"lastCapacityIncrement", arp.lastCapacityIncrement,
			)
			return
		}

		// handle capacity decrease
		arp.timeout = arp.doubleTimeout(fr, arp.timeout)
		arp.lastCapacityIncrement = 0
		arp.consecutivePacingGains = 0 // reset consecutive gains
		// reset cooldown
		arp.capacityIncrementCoolDownRemaining = arp.capacityIncrementCoolDown
		arp.logger.Info("still in overload, possible capacity decrease, doubling timeout",
			"id", arp.id,
			"sendRateInterval", arp.sendRateInterval,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"dropped", arp.dropped,
			"req", arp.req,
			"timeout", arp.timeout,
		)
		return
	}

	if arp.capacityIncrementCoolDown > 0 {
		arp.capacityIncrementCoolDown--
		arp.logger.Info("still in overload, capacity recently been increased, cooling down.",
			"id", arp.id,
			"sendRateInterval", arp.sendRateInterval,
			"overloadThresholdReq", arp.overloadThresholdReq,
			"dropped", arp.dropped,
			"req", arp.req,
			"capacityIncrement", arp.lastCapacityIncrement,
			"capacityIncrementCoolDown", arp.capacityIncrementCoolDown,
		)
		return
	}

	// gain pacing by x1.125 x 2^consecutivePacingGains
	// this helps faster recovery of pacing from circuit broken state
	arp.lastCapacityIncrement = max(max(arp.overloadThresholdReq>>LOG2_PACING_GAIN, 1)<<arp.consecutivePacingGains, 1)
	arp.overloadThresholdReq += arp.lastCapacityIncrement
	arp.sendRateInterval = arp.interval / time.Duration(
		arp.overloadThresholdReq,
	)
	arp.consecutivePacingGains++
	arp.logger.Info("still in overload, growing pacing to seek for additional capacity",
		"id", arp.id,
		"sendRateInterval", arp.sendRateInterval,
		"overloadThresholdReq", arp.overloadThresholdReq,
		"dropped", arp.dropped,
		"req", arp.req,
		"capacityIncrement", arp.lastCapacityIncrement,
	)
	return
}

// calcInflight calculates current inflight requests.
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) inflight() int64 {
	// make sure to subtract dropped
	return arp.req - arp.res - arp.dropped
}

// if failed from the last period is zero, returns true no matter the sample counts
// this allows skipping
func (arp *AdaptoRTOProvider) hasEnoughSamples() bool {
	arp.logger.Info("checking for number of samples",
		"id", arp.id,
		"minSamplesRequired", arp.minSamplesRequired,
		"res", arp.res,
		"req", arp.req,
		"failed", arp.failed,
		"dropped", arp.dropped,
	)
	return arp.failed == 0 || arp.res >= int64(math.Round(arp.minSamplesRequired))
}

func (arp *AdaptoRTOProvider) computeFailure() float64 {
	fr := float64(arp.failed) / float64(arp.res) // failure rate for current interval
	arp.lastFr = fr                              // update previous failure rate
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
func (arp *AdaptoRTOProvider) succeeded() int64 {
	return arp.res - arp.failed - arp.genericErr
}

// NewTimeout returns the current timeout value
// pseudo client queue / rate limiting suspends timeout creation during overload
func (arp *AdaptoRTOProvider) NewTimeout(ctx context.Context) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	arp.req++ // increment req counter
	rttCh = arp.rttCh

	switch arp.state {
	case STARTUP:
		// startup intervals, where timeout, srtt, and rttvar are updated,
		// but sloLatency is used to minimize the ACTUAL failure rate.
		// the internal failure rate tracked by arp.failed should be updated using the updated timeout value
		return arp.sloLatency, arp.rttCh, nil

	case CRUISE:
		// timeout is returned as is
		return arp.timeout, arp.rttCh, nil

	case DRAIN:
		// timeout is returned as is, assuming that it is already choked
		return arp.timeout, rttCh, nil

	case OVERLOAD:
		// handle cases where sendRateInterval exceeds the sloLatency
		maxSuspend := arp.sloLatency - arp.timeout
		if arp.sendRateInterval > maxSuspend {
			now := time.Now()
			// if nextTransimssion is some time in the past, permit transmission. And set nextTransimssion timestamp.
			if arp.nextTransimssion.Sub(now) < maxSuspend {
				arp.nextTransimssion = now.Add(arp.sendRateInterval)
				return arp.timeout, rttCh, nil
			}
			arp.logger.Debug("new timeout dropped (sendRate > slo - currTimeout))",
				"id", arp.id,
				"dropped", arp.dropped,
			)
			return time.Duration(0), rttCh, RequestRateLimitExceeded
		}

		suspend := arp.sendRateInterval * time.Duration(arp.queueLength)
		adjustedTimeout := arp.sloLatency - suspend
		if adjustedTimeout < arp.timeout {
			arp.dropped++
			arp.logger.Debug("new timeout dropped (timeout > slo - suspend)",
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
	case FAILURE:
		// check for failure state and if a health check request had been sent for this interval
		if arp.req == 1 {
			arp.logger.Info("new timeout for the first request in the interval during FAILURE",
				"id", arp.id,
			)
			return arp.sloLatency, rttCh, nil
		}
		arp.dropped++
		arp.logger.Debug("new timeout dropped",
			"id", arp.id,
			"dropped", arp.dropped,
		)
		return time.Duration(0), rttCh, RequestRateLimitExceeded
	}

	return arp.timeout, rttCh, nil
}

// ComputeNewRTO computes new rto based on new rtt
// new timeout value is returned and can be used to set timeouts or overload detection
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) ComputeNewRTO(rtt time.Duration) time.Duration {
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
func (arp *AdaptoRTOProvider) StartWithSLO() {
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
func (arp *AdaptoRTOProvider) OnRtt(signal RttSignal) {
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
	case STARTUP:
		arp.ComputeNewRTO(signal.Duration)
		return
	case CRUISE:
		arp.ComputeNewRTO(signal.Duration)
	case DRAIN:
		arp.ComputeNewRTO(signal.Duration)
		return
	case OVERLOAD:
		arp.ComputeNewRTO(signal.Duration)
		return
	case FAILURE:
		arp.ComputeNewRTO(signal.Duration)
	}
}

// OnInterval calculates failure rate and adjusts margin
// MUST be called with mu.Lock already acquired
func (arp *AdaptoRTOProvider) OnInterval() {
	// record next interval start
	defer func() {
		arp.intervalStart = time.Now()
		arp.onIntervalHandler(arp)
	}()
	// record current res
	arp.prevReqs.Add(arp.res)

	switch arp.state {
	case STARTUP:
		arp.prevSuccRess.Add(arp.succeeded())
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"minSamplesRequired", arp.minSamplesRequired,
				"res", arp.res,
				"state", StateAsString(arp.state),
			)
			return
		}
		defer arp.resetCounters() // reset counters each interval
		fr := arp.computeFailure()
		arp.updateKMarginShortTerm(fr)
		arp.timeout = arp.ComputeNewRTO(time.Duration(arp.srtt >> LOG2_ALPHA))
		if arp.timeout == arp.sloLatency {
			arp.transitionToDrain()
			return
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
	case CRUISE:
		arp.prevSuccRess.Add(arp.succeeded())
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"minSamplesRequired", arp.minSamplesRequired,
				"res", arp.res,
				"state", StateAsString(arp.state),
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
	case DRAIN:
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"minSamplesRequired", arp.minSamplesRequired,
				"res", arp.res,
				"state", StateAsString(arp.state),
			)
			return
		}
		defer arp.resetCounters() // reset counters each interval

		// decrement interval counter if failure is suspected
		if arp.succeeded() == 0 {
			arp.overloadDrainIntervalsRemaining--
			if arp.overloadDrainIntervalsRemaining == 0 {
				arp.transitionToOverload()
			}
			return
		}

		current := arp.ComputeNewRTO(time.Duration(arp.srtt >> LOG2_ALPHA))
		// drain until srtt and rtt var has dropped so that produced timeout is (sloLatency - chokedTimeout) / 8 over chokedTimeout
		if current > (arp.sloLatency-arp.timeout)>>time.Duration(LOG2_ALPHA)+arp.timeout {
			// do not decrement intervals remaining until timeout is tabilized
			return
		}
		arp.overloadDrainIntervalsRemaining--
		if arp.overloadDrainIntervalsRemaining == 0 {
			arp.transitionToOverload()
		}
		return
	case OVERLOAD:
		// bypass samples count check
		if arp.overloadThresholdReq == 1 && arp.succeeded() == 0 {
			// if request per interval is reduced to 1, yet none are succeeding, declare failure
			arp.transitionToFailure()
			arp.resetCounters() // reset counters each interval
			return
		}
		if !arp.hasEnoughSamples() {
			arp.logger.Info("not enough samples",
				"id", arp.id,
				"minSamplesRequired", arp.minSamplesRequired,
				"res", arp.res,
				"state", StateAsString(arp.state),
			)
			return
		}
		defer arp.resetCounters() // reset counters each interval
		fr := arp.computeFailure()
		if arp.dropped == 0 && fr < arp.sloFailureRateAdjusted {
			// undeclare overload
			// no need to reset variables as they are all reset at the beginning of interval
			// reset threshold and send rate interval
			arp.transitionToCruise()
			return
		}
		// should update sending rate if drop ratio is too high and there is room in failure rate
		// if failure rate is too high, adjust timeout.
		arp.timeout = arp.ComputeNewRTO(time.Duration(arp.srtt >> LOG2_ALPHA))
		arp.updateCapacityEstimateOrDoubleTimeout(fr) // can call no matter the fr as fr is checked in the method
		if arp.timeout == arp.sloLatency {
			arp.transitionToDrain()
			return
		}
		return
	case FAILURE:
		defer arp.resetCounters()
		// undeclare failure if health request succeeds
		if arp.succeeded() > 0 {
			arp.transitionToOverload()
		}
		return
	}
}

var AdaptoRTOProviders map[string]*AdaptoRTOProvider

func init() {
	// initialize global provider map
	AdaptoRTOProviders = make(map[string]*AdaptoRTOProvider)
}

// GetTimeout retrieves timeout value using provider with given id in config.
// if no provider with matching id is found, creates a new provider
func GetTimeout(ctx context.Context, config Config) (timeout time.Duration, rttCh chan<- RttSignal, err error) {
	if config.DisableClientQueuing {
		return getTimeoutWCQ(ctx, config)
	}
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

func GetState(config Config) (string, error) {
	provider, ok := AdaptoRTOProviders[config.Id]
	if !ok {
		return "", fmt.Errorf("No provider with id=%s found", config.Id)
	}
	return provider.State(), nil
}

// CapacityEstimate returns current overload estimate
// If not called in onIntervalHandler, must acquire lock first
func (arp *AdaptoRTOProvider) CapacityEstimate() int64 {
	return arp.overloadThresholdReq
}

// State returns current state
// If not called in onIntervalHandler, must acquire lock first
func (arp *AdaptoRTOProvider) State() string {
	return StateAsString(arp.state)
}
