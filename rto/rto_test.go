package rto

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestInitialRTTCalculation(t *testing.T) {
	config := Config{
		Id:  "test1",
		Max: 5 * time.Second,
		Min: 50 * time.Millisecond,
	}

	timeout, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")
	assert.Equal(t, config.Max, timeout, "Initial timeout should match config max")

	provider := AdaptoRTOProviders[config.Id]

	// Send the first RTT measurement
	firstRTT := 100 * time.Millisecond
	rttCh <- firstRTT
	time.Sleep(100 * time.Millisecond)

	// Verify initial SRTT, RTTVAR, and Timeout
	expectedSrtt := int64(firstRTT) * 8
	expectedRttvar := int64(firstRTT) * 2
	expectedTimeout := firstRTT + time.Duration(DEFAULT_K_MARGIN*expectedRttvar)

	provider.mu.Lock()
	assert.Equal(t, expectedSrtt, provider.srtt, "Initial SRTT should be scaled correctly")
	assert.Equal(t, expectedRttvar, provider.rttvar, "Initial RTTVAR should be scaled correctly")
	assert.Equal(t, expectedTimeout, provider.timeout, "Initial timeout should be calculated correctly")
	provider.mu.Unlock()
}

func TestRegularRTTUpdates(t *testing.T) {
	config := Config{
		Id:  "test2",
		Max: 5 * time.Second,
		Min: 50 * time.Millisecond,
	}

	timeout, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")
	assert.Equal(t, config.Max, timeout, "Initial timeout should match config max")

	provider := AdaptoRTOProviders[config.Id]

	// Simulate multiple RTT updates
	rttValues := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 150 * time.Millisecond}
	for _, rtt := range rttValues {
		rttCh <- rtt
	}

	// Allow time for processing and check updated values
	time.Sleep(100 * time.Millisecond)

	// Checking the final timeout and the smoothed RTT and deviation are non-zero
	provider.mu.Lock()
	assert.Greater(t, provider.srtt, int64(0), "SRTT should be greater than zero after updates")
	assert.Greater(t, provider.rttvar, int64(0), "RTTVAR should be greater than zero after updates")
	assert.GreaterOrEqual(t, provider.timeout.Nanoseconds(), config.Min.Nanoseconds(), "Timeout should be greater than min after updates")
	provider.mu.Unlock()
}

func TestTimeoutBackoff(t *testing.T) {
	config := Config{
		Id:  "test3",
		Max: 5 * time.Second,
		Min: 50 * time.Millisecond,
	}

	timeout, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")

	provider := AdaptoRTOProviders[config.Id]

	// Send an RTT measurement
	rttCh <- 100 * time.Millisecond
	time.Sleep(50 * time.Millisecond)
	provider.mu.Lock()
	expectedBackoffTimeout := min(config.Max, provider.timeout*time.Duration(DEFAULT_BACKOFF))
	provider.mu.Unlock()

	// Send a DeadlineExceeded signal to test backoff
	rttCh <- -timeout

	time.Sleep(50 * time.Millisecond)
	// Check if timeout was backed off correctly
	provider.mu.Lock()
	assert.Equal(t, expectedBackoffTimeout, provider.timeout, "Timeout should be backed off correctly on deadline exceeded")
	provider.mu.Unlock()
}

func TestMinMaxConstraints(t *testing.T) {
	config := Config{
		Id:  "test4",
		Max: 5 * time.Second,
		Min: 500 * time.Millisecond,
	}

	_, rttCh, err := GetTimeout(config)
	assert.NoError(t, err, "Error should be nil for GetTimeout")

	provider := AdaptoRTOProviders[config.Id]

	// Send low RTT to check min constraint
	rttCh <- 10 * time.Millisecond
	time.Sleep(50 * time.Millisecond)

	provider.mu.Lock()
	assert.GreaterOrEqual(t, provider.timeout, config.Min, "Timeout should not go below minimum")
	provider.mu.Unlock()

	// Send high RTT to check max constraint
	rttCh <- 10 * time.Second
	time.Sleep(50 * time.Millisecond)
	provider.mu.Lock()
	assert.LessOrEqual(t, provider.timeout, config.Max, "Timeout should not exceed maximum")
	provider.mu.Unlock()
}
