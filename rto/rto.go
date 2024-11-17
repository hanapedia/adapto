// rto implements adaptive timeout algorithm used in TCP retransmission timeout.
package rto

import (
	"fmt"
	"sync"
	"time"

	"github.com/hanapedia/adapto/logger"
)

const (
	DEFAULT_BACKOFF        int64         = 2
	CONSERVATIVE_BACKOFF   int64         = 1
	DEFAULT_K_MARGIN       int64         = 1
	DEFAULT_INTERVAL       time.Duration = 5 * time.Second
	DEFAULT_SLO            float64       = 0.1
	ALPHA_SCALING          int64         = 8
	LOG2_ALPHA             int64         = 3
	BETA_SCALING           int64         = 4
	LOG2_BETA              int64         = 2
	LOG2_SLO_SAFETY_MARGIN int64         = 1    // safety margin of 0.5 or division by 2
	FR_SCALING             int64         = 10e5 // keep the fr to .000001 precision
)

// DONE(v1.0.14): consider the raional of using negative duration for timedout requests
// so that the duration that just timed out can be transferred and used
// in that case, there is no need to define DeadlineExceeded.
// this will be breaking change for the users

// RttSignal is alias for time.Duration that is used for typing the channel used to report rtt.
type RttSignal = time.Duration

// DeadlineExceeded checks if rtt signal is for requests that DeadlineExceeded
func DeadlineExceeded(rtt RttSignal) bool {
	return rtt < 0
}

type Config struct {
	Id       string
	Max      time.Duration // max timeout value allowed
	Min      time.Duration // min timeout value allowed
	SLO      float64       // target failure rate SLO
	Capacity int64         // capacity given as requests per second
	Interval time.Duration // interval for failure rate calculations
	KMargin  int64         // starting kMargin for with SLO and static kMargin for without SLO

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
	if c.SLO == 0 {
		c.Logger.Info("SLO is not provided, using the normal RTO implementation")
	}
	if c.SLO != 0 && c.Capacity == 0 {
		return fmt.Errorf("Capacity is required if SLO is provided")
	}
	if c.SLO != 0 && c.Interval == 0 {
		return fmt.Errorf("Interval is required if SLO is provided")
	}
	return nil
}

type AdaptoRTOProvider struct {
	// logger uses logger.DefaultLogger if not set
	logger logger.Logger
	// fields with synchronized access
	timeout time.Duration

	// timeoutLock flag indicating whether timeout can be updated
	timeoutLock bool

	// values used in timeout calculations and adjusted dynamically
	srtt    int64
	rttvar  int64
	kMargin int64 // extra margin multiplied to the origin K=4
	backoff int64 // backoff multiplier
	fr      int64 // cumulative failure rate computed as moving average. scaled by FR_SCALING

	// keep track of minimum RTT to fallback to
	minRtt time.Duration

	// counters for failure rate and inflight
	// inflight should be computed by req - recv
	// failure rate should be computed by failed / recv
	req    int64 // number of requests sent
	res    int64 // number of responses received
	failed int64 // number of requests failed

	// mutex for synchronizing access to timeout calculation fields
	mu sync.Mutex

	// channel to receive the recorded rtt
	// in the event of timeout, sender should send `DeadlineExceeded` signal
	rttCh chan RttSignal

	requestLimt int64 // request limit computed by capacity / interval
	sloAdjusted int64 // slo with safety margin. also scaled to integer

	// configuration fields
	id       string
	min      time.Duration
	max      time.Duration
	slo      float64       // slo failure rate
	interval time.Duration // interval for computing failure rate.
	capacity int64         // capacity in requests per second
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
		timeout: config.Max,

		srtt:    0,
		rttvar:  0,
		kMargin: kMargin,
		backoff: DEFAULT_BACKOFF,

		minRtt: config.Max,

		req:    0,
		res:    0,
		failed: 0,

		requestLimt: config.Capacity * config.Interval.Milliseconds() / 1000,
		sloAdjusted: int64(config.SLO*float64(FR_SCALING)) >> LOG2_SLO_SAFETY_MARGIN,

		rttCh:    make(chan RttSignal),
		id:       config.Id,
		min:      config.Min,
		max:      config.Max,
		slo:      config.SLO,
		interval: config.Interval,
		capacity: config.Capacity,
	}
}

// onRequest increments only req
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) onRequest() {
	arp.req++
}

// onRtt increments only res
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) onRtt() {
	arp.res++
}

// onDeadlineExceeded increments only failed
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) onDeadlineExceeded() {
	arp.failed++
}

// failureRate calculates failure rate for the current interval.
// returns fr scaled by FR_SCALING
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) calcFailureRate() int64 {
	if arp.res == 0 {
		return 0
	}
	fr := arp.failed * FR_SCALING / arp.res
	/* arp.logger.Info("computing fr", "failed", arp.failed, "fr", fr, "arp.fr", arp.fr) */
	// if previous arp.fr is too high, it takes time for fr to be adjusted
	// so new fr should be weighted more. for simplicity, weight the new fr equal to all previous fr
	// so essentially it is taking the mean between previous mean and new value
	arp.fr = (arp.fr+fr)>>1
	return arp.fr
}

// resetCounters resets counters
// req counter is reset to whatever the inflight was at this moment
func (arp *AdaptoRTOProvider) resetCounters() {
	// reset counters
	arp.req = arp.inflight()
	arp.res = 0
	arp.failed = 0
}

// onRequestLimit calculates failureRate
// should lock rto updates
func (arp *AdaptoRTOProvider) onRequestLimit() {
	// DONE(v1.0.11): currently, this does not activate soon enough because there is a gap between req and res.
	// instead of looking at just the current fr, it should also look at current inflight, which could fail
	// with higher chance than current failure rate, dur to overload.
	// a conservative approach is to consider all inflight as failed and add them to failed
	// so compute the failure rate by (failed + inflight) / req instead of failed / res
	//
	// DONE(v1.0.12): OR could ignore the failure rate entirely and lock the timeout at reaching capacity load.
	/* fr := arp.adjustedFailureRate() */
	/* fr := arp.failureRate() */

	// TODO:(low priority) consider the rational of resetting the counters
	arp.resetCounters() // reset counters after failure rate calculation
	// fallback to timeout with mimimum RTT
	// DONE(v1.0.13): whether to reset srtt and rttvar remains as question -> do not reset
	// if not reset, the timeout calculation can use srtt and rttvar from right before the overload
	// this is probably better
	// no need to compute srtt
	/* srtt = int64(arp.minRtt) * ALPHA_SCALING              // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8 */
	rttvar := (int64(arp.minRtt) >> 1) * BETA_SCALING // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
	// compute rto if it was the first rtt observed
	rto := arp.minRtt + time.Duration(arp.kMargin*rttvar) // because rtt = srtt / 8
	arp.timeout = min(max(rto, arp.min), arp.max)
	arp.timeoutLock = true // lock timeout update until overload is gone
	arp.logger.Info("timeout locked", "rto", rto)
}

// onInterval calculates failure rate and adjusts margin
// this should be the only way margin is mutated
// should unlock rto updates
// DONE(v1.0.19): if the interval was to be set at 1 second,
// it could be better to look at long term average failure rate because there won't be much samples
// so we could use the similar algorithm as RTO
func (arp *AdaptoRTOProvider) onInterval() {
	arp.timeoutLock = false // unlock timeout update
	arp.backoff = DEFAULT_BACKOFF
	fr := arp.calcFailureRate()
	arp.resetCounters() // reset counters after failure rate calculation
	// use adjusted SLO
	if fr > arp.sloAdjusted {
		arp.kMargin++
		arp.logger.Info("timeout unlocked, incrementing kMargin", "fr", fr, "sloAdjusted", arp.sloAdjusted, "kMargin", arp.kMargin)
		return
	}

	// DONE: refer to the difference between the current fr and slo, and current kMargin
	// to decide if it is rational to decrement
	// (kMargin - 1) / kMargin < slo - fr / fr
	// 1 / 2  < 0.01 - 0.009 / 0.009
	// 5 / 6 < 1/9
	// DONE(v1.0.13): decrementing kMargin causes unstable failure rate even when load is low
	// conisder removing this entirely
	// decrementing this value could be handled after some significant interval to catch long term changes in latency variance
	// -> disabled for now
	// -> TODO: could reference long term latency distribution to decrement kMargin periodically
	// -> e.g. if p(1-slo) latency is well below the timeout being set, decrement kMargin
	/* if arp.kMargin > 1 { */
	/* 	arp.kMargin-- */
	/* 	arp.logger.Info("timeout unlocked, decrementing kMargin", "fr", fr, "kMargin", arp.kMargin) */
	/* 	return */
	/* } */
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
		if config.SLO != 0 {
			go provider.StartWithSLO()
		} else {
			go provider.Start()
		}
		AdaptoRTOProviders[config.Id] = provider
	}
	timeout, rttCh = provider.NewTimeout()

	return timeout, rttCh, nil
}

// NewTimeout returns the current timeout value
// TODO2: currently, timeouts are adjusted for every response and every capacity * interval requests.
// this means that the timeout cannot quickly adjuste to the sudden increase in requests until they return.
// consider adjusting the timeout value based on the current inflight
// could possibly use X = inflight / requestLimt
// higher the X, it is close to being overloaded
func (arp *AdaptoRTOProvider) NewTimeout() (timeout time.Duration, rttCh chan<- RttSignal) {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	arp.onRequest() // increment req
	arp.logger.Info("New timeout created.", "inflight", arp.inflight(), "timeout", arp.timeout, "fr", arp.calcFailureRate())
	return arp.timeout, arp.rttCh
}

// ComputeNewRTO computes new rto based on new rtt
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) ComputeNewRTO(rtt time.Duration) {
	if DeadlineExceeded(rtt) {
		// DONE(v1.0.14): doubling just the rto value might only have effect on the next rto
		// because as soon as a request succeeds, the rto value is computed with srtt and rttvar prior to timeout.
		// could possibly adjust rto using the timeout value for this request -> implemented
		arp.onDeadlineExceeded() // increment failed
		rtt *= -1
		arp.logger.Debug("DeadlineExceeded", "rto", arp.timeout.String(), "rtt", rtt)
	}

	if arp.srtt == 0 {
		// first observation of rtt
		arp.srtt = int64(rtt) * ALPHA_SCALING              // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
		arp.rttvar = (int64(rtt) >> 1) * BETA_SCALING      // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
		rto := rtt + time.Duration(arp.kMargin*arp.rttvar) // because rtt = srtt / 8
		if !arp.timeoutLock {
			arp.timeout = min(max(rto, arp.min), arp.max)
		}
		arp.logger.Debug("new RTO computed", "rto", arp.timeout.String(), "rtt", rtt.String())
		return
	}

	rto, srtt, rttvar := jacobsonCalc(int64(rtt), arp.srtt, arp.rttvar, arp.kMargin)
	if !arp.timeoutLock {
		arp.timeout = min(max(time.Duration(rto), arp.min), arp.max)
	}
	arp.srtt = srtt
	arp.rttvar = rttvar
	arp.logger.Debug("new RTO computed", "rto", arp.timeout.String(), "rtt", rtt.String())
}

// Start starts the provider by spawning a goroutine that waits for new rtt or timeout event and updates the timeout value accordingly.
func (arp *AdaptoRTOProvider) Start() {
	// ticker for computing failure rate
	for rtt := range arp.rttCh {
		arp.mu.Lock()
		arp.onRtt() // increment res
		arp.ComputeNewRTO(rtt)
		arp.mu.Unlock()
	}
}

// StartWithSLO starts the provider by spawning a goroutine that waits for new rtt or timeout event and updates the timeout value accordingly. timeout calculations are also adjusted to meet the SLO
func (arp *AdaptoRTOProvider) StartWithSLO() {
	// ticker for computing failure rate
	ticker := time.NewTicker(arp.interval)
	for {
		select {
		case rtt := <-arp.rttCh:
			arp.mu.Lock()
			arp.onRtt() // increment res
			// check if request limit is reached
			if arp.res >= arp.requestLimt {
				arp.onRequestLimit()
				ticker.Reset(arp.interval) // reset ticker as failure rate was computed by request limit
				arp.mu.Unlock()
				continue
			}
			if !DeadlineExceeded(rtt) {
				arp.minRtt = min(rtt, arp.minRtt)
			}
			arp.ComputeNewRTO(rtt)
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
