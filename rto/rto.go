// rto implements adaptive timeout algorithm used in TCP retransmission timeout.
package rto

import (
	"fmt"
	"sync"
	"time"

	"github.com/hanapedia/adapto/logger"
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
)

type RttSignal = time.Duration

const (
	DeadlineExceeded RttSignal = iota
	// add new signals if needed
)

type Config struct {
	Id       string
	Max      time.Duration // max timeout value allowed
	Min      time.Duration // min timeout value allowed
	SLO      float64       // target failure rate SLO
	Capacity int64         // capacity given as requests per second
	Interval time.Duration // interval for failure rate calculations

	Logger logger.Logger // optional logger
}

func (c *Config) Validate() error {
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
		fmt.Println("SLO is not provided, using the normal RTO implementation")
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
	return &AdaptoRTOProvider{
		logger:  l,
		timeout: config.Max,

		srtt:    0,
		rttvar:  0,
		kMargin: DEFAULT_K_MARGIN,
		backoff: DEFAULT_BACKOFF,

		minRtt: config.Max,

		req:    0,
		res:    0,
		failed: 0,

		requestLimt: config.Capacity * config.Interval.Milliseconds() / 1000,

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

// failureRate calculates failure rate for the current interval
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) failureRate() float64 {
	if arp.res == 0 {
		return 0
	}
	return float64(arp.failed) / float64(arp.res)
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
	fr := arp.failureRate()
	arp.resetCounters() // reset counters after failure rate calculation
	if fr >= arp.slo {
		// fallback to timeout with mimimum RTT
		// TODO: whether to reset srtt and rttvar remains as question
		arp.srtt = int64(arp.minRtt) * ALPHA_SCALING              // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
		arp.rttvar = (int64(arp.minRtt) >> 1) * BETA_SCALING      // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
		rto := arp.minRtt + time.Duration(arp.kMargin*arp.rttvar) // because rtt = srtt / 8
		arp.timeout = min(max(rto, arp.min), arp.max)
		arp.timeoutLock = true // lock timeout update until overload is gone
	} else {
		// TODO: consider adding safetry margin
		// shift to conservative timeout increment
		arp.backoff = CONSERVATIVE_BACKOFF
	}
	arp.logger.Info("on request limit reached", "fr", fr, "slo", arp.slo, "kMargin", arp.kMargin, "backoff", arp.backoff, "rto", arp.timeout, "srtt", arp.srtt, "rttvar", arp.rttvar, "minRtt", arp.minRtt)
}

// onInterval calculates failure rate and adjusts margin
// this should be the only way margin is mutated
// should unlock rto updates
func (arp *AdaptoRTOProvider) onInterval() {
	arp.timeoutLock = false // unlock timeout update
	arp.backoff = DEFAULT_BACKOFF
	fr := arp.failureRate()
	arp.resetCounters() // reset counters after failure rate calculation
	if fr >= arp.slo {
		arp.kMargin++
	} else if arp.kMargin > 1 {
		arp.kMargin-- // TODO: think about the rational of this. could lead to oscillation
	}
	arp.logger.Info("on interval reached", "fr", fr, "slo", arp.slo, "kMargin", arp.kMargin)
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

func (arp *AdaptoRTOProvider) NewTimeout() (timeout time.Duration, rttCh chan<- RttSignal) {
	arp.mu.Lock()
	defer arp.mu.Unlock()
	arp.onRequest() // increment req
	return arp.timeout, arp.rttCh
}

// ComputeNewRTO computes new rto based on new rtt
// MUST be called in thread safe manner as it does not lock mu
func (arp *AdaptoRTOProvider) ComputeNewRTO(rtt time.Duration) {
	if rtt == DeadlineExceeded {
		// TODO: doubling just the rto value might only have effect on the next rto
		// because as soon as a request succeeds, the rto value is computed with srtt and rttvar prior to timeout.
		arp.onDeadlineExceeded() // increment failed
		if !arp.timeoutLock {
			arp.timeout = min(arp.max, arp.timeout*time.Duration(arp.backoff))
		}
		arp.logger.Info("new RTO computed", "rto", arp.timeout.String(), "rtt", "DeadlineExceeded")
		return
	}

	if arp.srtt == 0 {
		// first observation of rtt
		arp.srtt = int64(rtt) * ALPHA_SCALING              // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
		arp.rttvar = (int64(rtt) >> 1) * BETA_SCALING      // use the scaled rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
		rto := rtt + time.Duration(arp.kMargin*arp.rttvar) // because rtt = srtt / 8
		if !arp.timeoutLock {
			arp.timeout = min(max(rto, arp.min), arp.max)
		}
		arp.logger.Info("new RTO computed", "rto", arp.timeout.String(), "rtt", rtt.String())
		return
	}

	rto, srtt, rttvar := jacobsonCalc(int64(rtt), arp.srtt, arp.rttvar, arp.kMargin)
	if !arp.timeoutLock {
		arp.timeout = min(max(time.Duration(rto), arp.min), arp.max)
	}
	arp.srtt = srtt
	arp.rttvar = rttvar
	arp.logger.Info("new RTO computed", "rto", arp.timeout.String(), "rtt", rtt.String())
}

// Start starts the provider by spawning a goroutine that waits for new rtt or timeout event and updates the timeout value accordingly.
func (arp *AdaptoRTOProvider) Start() {
	// ticker for computing failure rate
	for rtt := range arp.rttCh {
		arp.mu.Lock()
		arp.onRtt() // increment res
		if rtt != DeadlineExceeded {
			arp.minRtt = min(rtt, arp.minRtt)
		}
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
			if rtt != DeadlineExceeded {
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
