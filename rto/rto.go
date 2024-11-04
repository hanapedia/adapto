// rto implements adaptive timeout algorithm used in TCP retransmission timeout.
package rto

import (
	"time"

	"github.com/hanapedia/adapto/count"
	"go.uber.org/atomic"
)

const (
	DEFAULT_BACKOFF = 2
	DEFAULT_MARGIN  = 1
)

type RttSignal = time.Duration

const (
	DeadlineExceeded RttSignal = iota
	// add new signals if needed
)

type Config struct {
	Id      string
	Max     time.Duration // max timeout value allowed
	Min     time.Duration // min timeout value allowed
	Margin  int64         // extra margin multplied to the origin K=4
	Backoff int64         // backoff multiplied to the timeout when timeout
}

type AdaptoRTOProvider struct {
	// atomic fields
	counts  count.Counts
	timeout atomic.Duration

	// values used in timeout calculations
	srtt   atomic.Int64
	rttvar atomic.Int64

	// channel to recevie the recorded rtt
	// in the event of timeout, sender should send `DeadlineExceeded` signal
	rttCh chan RttSignal

	// configuration fields
	id      string
	min     time.Duration
	max     time.Duration
	margin  int64 // extra margin multplied to the origin K=4
	backoff int64
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
		margin := config.Margin
		if margin == 0 {
			margin = DEFAULT_MARGIN
		}
		backoff := config.Backoff
		if backoff == 0 {
			backoff = DEFAULT_BACKOFF
		}
		provider = &AdaptoRTOProvider{
			counts:  count.NewCounts(),
			timeout: *atomic.NewDuration(config.Max),
			srtt:    *atomic.NewInt64(0),
			rttvar:  *atomic.NewInt64(0),

			rttCh:   make(chan RttSignal),
			id:      config.Id,
			min:     config.Min,
			max:     config.Max,
			margin:  margin,
			backoff: backoff,
		}
		go provider.Start()
		AdaptoRTOProviders[config.Id] = provider
	}
	timeout, rttCh = provider.NewTimeout()

	return timeout, rttCh, nil
}

func (arp *AdaptoRTOProvider) NewTimeout() (timeout time.Duration, rttCh chan<- RttSignal) {
	return arp.timeout.Load(), arp.rttCh
}

// Start starts the provider by spawning a go routine that waits for new rtt or timeout event and updates the timeout value accordingly
func (arp *AdaptoRTOProvider) Start() {
	for rtt := range arp.rttCh {
		prevRto := arp.timeout.Load()
		prevSrtt := arp.srtt.Load()
		prevRttvar := arp.rttvar.Load()
		if rtt == DeadlineExceeded {
			arp.timeout.Store(min(arp.max, prevRto*time.Duration(arp.backoff)))
			continue
		}
		if prevSrtt == 0 {
			// first observation of rtt
			srtt := int64(rtt) * 8   // use the scaled srtt for Jacobson. R * 8 since alpha = 1/8
			rttvar := int64(rtt) * 2 // use the scaeld rttvar for Jacobson. (R / 2) * 4 since beta = 1/4
			rto := rtt + time.Duration(arp.margin*srtt)
			arp.timeout.Store(min(max(rto, arp.min), arp.max))
			arp.srtt.Store(srtt)
			arp.rttvar.Store(rttvar)
			continue
		}

		rto, srtt, rttvar := jacobsonCalc(int64(rtt), prevSrtt, prevRttvar, arp.margin)
		arp.timeout.Store(min(max(time.Duration(rto), arp.min), arp.max))
		arp.srtt.Store(srtt)
		arp.rttvar.Store(rttvar)
	}
}

func readableCalc(R, prevSrtt, prevRttvar int64) (rto, srtt, rttvar int64) {
	err := R - prevSrtt
	srtt = prevSrtt + (err >> 3)
	if err < 0 {
		err = -err
	}
	rttvar = prevRttvar + ((err - prevRttvar) >> 2)
	rto = srtt + 4*rttvar
	return rto, srtt, rttvar
}

func jacobsonCalc(R, prevSrtt, prevRttvar, margin int64) (rto, srtt, rttvar int64) {
	R = R - (prevSrtt >> 3) // R = R - (srtt / 8)
	prevSrtt = prevSrtt + R // srtt = srtt + R - (srtt / 8)
	if R < 0 {
		R = -R
	}
	R = R - (prevRttvar >> 2)   // R = |R - (srtt / 8)| - (rttvar / 4)
	prevRttvar = prevRttvar + R // rttvar = rttvar + |R - (srtt / 8)| - (rttvar / 4)

	// srtt + 4 * rttvar
	// srtt + R - (srtt / 8) + 4 * (rttvar + |R - (srtt / 8)| - (rttvar / 4))
	rto = (prevSrtt >> 3) + margin*prevRttvar
	return rto, prevSrtt, prevRttvar
}
