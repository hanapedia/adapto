package adapto

import (
	"errors"
	"time"

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
}

func (c Config) Validate() error {
	if c.Threshold >= 1 || c.Threshold < 0 {
		return errors.New("Cannot set threshold greater than 1.")
	}
	if c.IncBy <= 1 {
		return errors.New("Cannot set IncBy less than or equal to 1.")
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
		provider = &AdaptoProvider{
			counts: Counts{
				total:            *atomic.NewUint32(0),
				deadlineExceeded: *atomic.NewUint32(0),
			},
			currentDuration: *atomic.NewDuration(config.InitialTimeout),
			expiry:          *atomic.NewTime(time.Now().Add(config.Interval)),
			id:              config.Id,
			interval:        config.Interval,
			threshold:       config.Threshold,
			incBy:           config.IncBy,
			decBy:           config.DecBy,
		}
		AdaptoProviders[config.Id] = provider
	}
	timeoutDuration, didDeadlineExceed = provider.NewTimeout()
	return timeoutDuration, didDeadlineExceed, nil
}

// Counts holds the numbers of context with timeout set.
// adapto clears the internal Counts when new context is created by adapto
// and when context created by adapto exceeds deadline.
type Counts struct {
	total            atomic.Uint32
	deadlineExceeded atomic.Uint32
}

func (c *Counts) onNew() {
	c.total.Add(1)
}

func (c *Counts) onDeadlineExceeded() {
	c.deadlineExceeded.Add(1)
}

func (c *Counts) clear() {
	c.total.Store(0)
	c.deadlineExceeded.Store(0)
}

func (c *Counts) Total() uint32 {
	return c.total.Load()
}

func (c *Counts) DeadlineExceeded() uint32 {
	return c.deadlineExceeded.Load()
}

func (c *Counts) Ratio() float32 {
	total := c.total.Load()
	de := c.deadlineExceeded.Load()
	return float32(de) / float32(total)
}

type AdaptoProvider struct {
	// atomic fields
	counts          Counts
	currentDuration atomic.Duration
	expiry          atomic.Time

	// configuration fields
	id           string
	interval     time.Duration
	threshold    float32
	incBy        float32
	decBy        time.Duration
	minimumCount uint32
}

func (ap *AdaptoProvider) NewTimeout() (timeoutDuration time.Duration, didDeadlineExceed chan<- bool) {
	timeoutDuration = ap.currentDuration.Load()
	deadlineCh := make(chan bool)
	go func() {
		// timer for cleaning self up when there is no signal from deadline channel
		timer := time.NewTimer(timeoutDuration * 10)
		select {
		case isDeadlineExceeded := <-deadlineCh:
			timer.Stop()
			now := time.Now()
			if ap.expiry.Load().Before(now) {
				ap.counts.clear()
				ap.expiry.Store(now.Add(ap.interval))
			}
			if isDeadlineExceeded {
				ap.counts.onDeadlineExceeded()
				ap.inc()
			} else {
				ap.dec()
			}
		case <-timer.C:
			timer.Stop()
			now := time.Now()
			if ap.expiry.Load().Before(now) {
				ap.counts.clear()
				ap.expiry.Store(now.Add(ap.interval))
			}
			ap.counts.onDeadlineExceeded()
			ap.inc()
		}
	}()
	ap.counts.onNew()
	return timeoutDuration, deadlineCh
}

// inc conditionally, multiplicatively increments atomic duration used for new timeouts
func (ap *AdaptoProvider) inc() {
	if ap.threshold == 0 || (ap.counts.Total() > ap.minimumCount && ap.counts.Ratio() >= ap.threshold) {
		curr_duration := ap.currentDuration.Load()
		newDuration := time.Duration(float32(curr_duration) * ap.incBy)
		ap.currentDuration.Store(newDuration)
	}
}

// dec conditionally, additively decrements atomic duration used for new timeouts
func (ap *AdaptoProvider) dec() {
	if ap.threshold == 0 || (ap.counts.Total() > ap.minimumCount && ap.counts.Ratio() < ap.threshold) {
		ap.currentDuration.Sub(ap.decBy)
	}
}
