package rto

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestInitialRTTCalculation(t *testing.T) {
	config := Config{
		Id:     "test1",
		Max:    5 * time.Second,
		Min:    500 * time.Millisecond,
		Margin: DEFAULT_MARGIN,
		Backoff: DEFAULT_BACKOFF,
	}

	timeout, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")
	assert.Equal(t, config.Max, timeout, "Initial timeout should match config max")

	provider := AdaptoRTOProviders[config.Id]
	go provider.Start()

	// Send the first RTT measurement
	firstRTT := int64(100 * time.Millisecond)
	rttCh <- time.Duration(firstRTT)

	// Verify initial SRTT, RTTVAR, and Timeout
	expectedSrtt := firstRTT * 8
	expectedRttvar := firstRTT * 2
	expectedTimeout := time.Duration(expectedSrtt + config.Margin*expectedRttvar)
	assert.Equal(t, expectedSrtt, provider.srtt.Load(), "Initial SRTT should be scaled correctly")
	assert.Equal(t, expectedRttvar, provider.rttvar.Load(), "Initial RTTVAR should be scaled correctly")
	assert.Equal(t, expectedTimeout, provider.timeout.Load(), "Initial timeout should be calculated correctly")
}

func TestRegularRTTUpdates(t *testing.T) {
	config := Config{
		Id:     "test2",
		Max:    5 * time.Second,
		Min:    500 * time.Millisecond,
		Margin: DEFAULT_MARGIN,
		Backoff: DEFAULT_BACKOFF,
	}

	timeout, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")
	assert.Equal(t, config.Max, timeout, "Initial timeout should match config max")

	provider := AdaptoRTOProviders[config.Id]
	go provider.Start()

	// Simulate multiple RTT updates
	rttValues := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 150 * time.Millisecond}
	for _, rtt := range rttValues {
		rttCh <- rtt
	}

	// Allow time for processing and check updated values
	time.Sleep(100 * time.Millisecond)

	// Checking the final timeout and the smoothed RTT and deviation are non-zero
	assert.Greater(t, provider.srtt.Load(), int64(0), "SRTT should be greater than zero after updates")
	assert.Greater(t, provider.rttvar.Load(), int64(0), "RTTVAR should be greater than zero after updates")
	assert.GreaterOrEqual(t, provider.timeout.Load().Nanoseconds(), config.Min.Nanoseconds(), "Timeout should be greater than min after updates")
}

func TestTimeoutBackoff(t *testing.T) {
	config := Config{
		Id:     "test3",
		Max:    5 * time.Second,
		Min:    500 * time.Millisecond,
		Margin: DEFAULT_MARGIN,
		Backoff: DEFAULT_BACKOFF,
	}

	_, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")

	provider := AdaptoRTOProviders[config.Id]
	go provider.Start()

	// Send an RTT measurement
	rttCh <- 100 * time.Millisecond
	time.Sleep(50 * time.Millisecond)
	expectedBackoffTimeout := min(config.Max, provider.timeout.Load()*time.Duration(config.Backoff))

	// Send a DeadlineExceeded signal to test backoff
	rttCh <- DeadlineExceeded

	time.Sleep(50 * time.Millisecond)
	// Check if timeout was backed off correctly
	assert.Equal(t, expectedBackoffTimeout, provider.timeout.Load(), "Timeout should be backed off correctly on deadline exceeded")
}

func TestMinMaxConstraints(t *testing.T) {
	config := Config{
		Id:     "test4",
		Max:    5 * time.Second,
		Min:    500 * time.Millisecond,
		Margin: DEFAULT_MARGIN,
		Backoff: DEFAULT_BACKOFF,
	}

	_, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")

	provider := AdaptoRTOProviders[config.Id]
	go provider.Start()

	// Send low RTT to check min constraint
	rttCh <- 10 * time.Millisecond
	time.Sleep(50 * time.Millisecond)
	assert.GreaterOrEqual(t, provider.timeout.Load(), config.Min, "Timeout should not go below minimum")

	// Send high RTT to check max constraint
	rttCh <- 10 * time.Second
	time.Sleep(50 * time.Millisecond)
	assert.LessOrEqual(t, provider.timeout.Load(), config.Max, "Timeout should not exceed maximum")
}
