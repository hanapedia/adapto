package adapto

import (
	"errors"
	"time"

	"github.com/hanapedia/adapto/count"
	"go.uber.org/atomic"
)

type Config struct {
	Id             string
	Interval       time.Duration
	InitialTimeout time.Duration
	Threshold      float32
	IncBy          float32
	DecBy          time.Duration
	MinimumCount   uint32
	Min            time.Duration
	Max            time.Duration
}

func (c Config) Validate() error {
	if c.Threshold >= 1 || c.Threshold < 0 {
		return errors.New("Cannot set threshold greater than 1.")
	}
	if c.IncBy <= 1 {
		return errors.New("Cannot set IncBy less than or equal to 1.")
	}
	if c.Min <= 0 {
		return errors.New("Cannot set Min less than or equal to 0.")
	}
	return nil
}

var AdaptoProviders map[string]*AdaptoProvider

func init() {
	AdaptoProviders = make(map[string]*AdaptoProvider)
}

func GetTimeout(config Config) (timeoutDuration time.Duration, didDeadlineExceed chan<- bool, err error) {
	err = config.Validate()
	if err != nil {
		return timeoutDuration, didDeadlineExceed, err
	}
	provider, ok := AdaptoProviders[config.Id]
	if !ok {
		max := config.Max
		if max == 0 {
			max = config.InitialTimeout
		}
		provider = &AdaptoProvider{
			counts:          count.NewCounts(),
			currentDuration: *atomic.NewDuration(config.InitialTimeout),
			expiry:          *atomic.NewTime(time.Now().Add(config.Interval)),
			id:              config.Id,
			interval:        config.Interval,
			threshold:       config.Threshold,
			incBy:           config.IncBy,
			decBy:           config.DecBy,
			minimumCount:    config.MinimumCount,
			min:             config.Min,
			max:             max,
		}
		AdaptoProviders[config.Id] = provider
	}
	timeoutDuration, didDeadlineExceed = provider.NewTimeout()

	return timeoutDuration, didDeadlineExceed, nil
}

type AdaptoProvider struct {
	// atomic fields
	counts          count.Counts
	currentDuration atomic.Duration
	expiry          atomic.Time

	// configuration fields
	id           string
	interval     time.Duration
	threshold    float32
	incBy        float32
	decBy        time.Duration
	minimumCount uint32
	min          time.Duration
	max          time.Duration
}

func (ap *AdaptoProvider) NewTimeout() (timeoutDuration time.Duration, didDeadlineExceed chan<- bool) {
	// reset counters if interval is expired
	now := time.Now()
	if ap.interval != 0 && ap.expiry.Load().Before(now) {
		ap.counts.Clear()
		ap.expiry.Store(now.Add(ap.interval))
	}

	timeoutDuration = ap.currentDuration.Load()
	ap.counts.OnNew()

	deadlineCh := make(chan bool)
	go func() {
		// timer for cleaning self up when there is no signal from deadline channel
		// defaults to generated timeoutDuration * 2
		timer := time.NewTimer(timeoutDuration * 2)
		select {
		case isDeadlineExceeded := <-deadlineCh:
			timer.Stop()
			if isDeadlineExceeded {
				ap.counts.OnDeadlineExceeded()
				ap.inc(timeoutDuration)
			} else {
				ap.dec(timeoutDuration)
			}
		case <-timer.C:
			timer.Stop()
			ap.counts.OnDeadlineExceeded()
			ap.inc(timeoutDuration)
		}
	}()
	return timeoutDuration, deadlineCh
}

// inc conditionally, multiplicatively increments atomic duration used for new timeouts
// if current value is greater than or equal to new value generated using timedout duration, the current value is not changed
// if current value is updated, interval is renewed with fresh counts
func (ap *AdaptoProvider) inc(previousDuration time.Duration) {
	if ap.counts.Total() < ap.minimumCount {
		return
	}
	if !(ap.threshold == 0 || ap.counts.Ratio() >= ap.threshold) {
		return
	}
	newDuration := time.Duration(float32(previousDuration) * ap.incBy)
	if newDuration > ap.max {
		newDuration = ap.max
	}
	// skip update if newDuration is not greater than current timedout duration
	currentDuration := ap.currentDuration.Load()
	if newDuration <= currentDuration {
		return
	}
	ap.currentDuration.Store(newDuration)

	// renew interval
	ap.counts.Clear()
	if ap.interval == 0 {
		return
	}
	now := time.Now()
	ap.expiry.Store(now.Add(ap.interval))
}

// dec conditionally, additively decrements atomic duration used for new timeouts
// if current value is smaller than or equal to new value generated using timedout duration, the current value is not changed
func (ap *AdaptoProvider) dec(previousDuration time.Duration) {
	if ap.counts.Total() < ap.minimumCount {
		return
	}
	if !(ap.threshold == 0 || ap.counts.Ratio() < ap.threshold) {
		return
	}
	newDuration := previousDuration - ap.decBy
	if newDuration < ap.min {
		newDuration = ap.min
	}
	// skip update if newDuration is not smaller than current timedout duration
	currentDuration := ap.currentDuration.Load()
	if newDuration >= currentDuration {
		return
	}
	ap.currentDuration.Store(newDuration)

	// renew interval
	ap.counts.Clear()
	if ap.interval == 0 {
		return
	}
	now := time.Now()
	ap.expiry.Store(now.Add(ap.interval))
}
