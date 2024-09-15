package adapto

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/atomic"
)

// TestConfigValidation tests the validation method of the Config struct.
func TestConfigValidation(t *testing.T) {
	tests := []struct {
		config   Config
		hasError bool
	}{
		{Config{Threshold: 1.2, IncBy: 1.5}, true},  // Threshold too high
		{Config{Threshold: 0.8, IncBy: 1}, true},    // IncBy less than or equal to 1
		{Config{Threshold: 0.8, IncBy: 1.5}, false}, // Valid config
	}

	for _, test := range tests {
		err := test.config.Validate()
		if test.hasError {
			assert.Error(t, err, "Expected an error for invalid config")
		} else {
			assert.NoError(t, err, "Expected no error for valid config")
		}
	}
}

// TestGetTimeout tests the GetTimeout function.
func TestGetTimeout(t *testing.T) {
	config := Config{
		Id:             "test1",
		Interval:       5 * time.Second,
		InitialTimeout: 2 * time.Second,
		Threshold:      0.5,
		IncBy:          1.5,
		DecBy:          500 * time.Millisecond,
	}

	timeoutDuration, didDeadlineExceed, err := GetTimeout(config)

	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, config.InitialTimeout, timeoutDuration, "Initial timeout duration does not match")
	assert.NotNil(t, didDeadlineExceed, "didDeadlineExceed channel should not be nil")

	// Verify if the AdaptoProvider is created and stored correctly
	provider, ok := AdaptoProviders[config.Id]
	assert.True(t, ok, "AdaptoProvider should be created and stored")
	assert.Equal(t, config.Id, provider.id, "Provider ID does not match")
	assert.Equal(t, config.InitialTimeout, provider.currentDuration.Load(), "Initial timeout in provider does not match")
}

// TestNewTimeout tests the NewTimeout method of AdaptoProvider.
func TestNewTimeout(t *testing.T) {
	config := Config{
		Id:             "test2",
		Interval:       5 * time.Second,
		InitialTimeout: 1 * time.Second,
		Threshold:      0.5,
		IncBy:          2,
		DecBy:          200 * time.Millisecond,
	}

	provider := AdaptoProvider{
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

	// Simulate creating a new timeout
	timeoutDuration, didDeadlineExceed := provider.NewTimeout()
	assert.Equal(t, config.InitialTimeout, timeoutDuration, "Timeout duration should match initial timeout")

	// Simulate a deadline exceeded signal
	didDeadlineExceed <- true

	// Verify if the counts are updated
	assert.Equal(t, uint32(1), provider.counts.deadlineExceeded.Load(), "Deadline exceeded count should be updated")
	assert.Equal(t, uint32(1), provider.counts.total.Load(), "Total count should be updated")
}

// TestInc tests the inc methods of AdaptoProvider.
func TestInc(t *testing.T) {
	config := Config{
		Id:             "test3",
		Interval:       5 * time.Second,
		InitialTimeout: 1 * time.Second,
		Threshold:      0.3,
		IncBy:          1.5,
		DecBy:          200 * time.Millisecond,
		MinimumCount:   0,
	}

	provider := AdaptoProvider{
		counts: Counts{
			total:            *atomic.NewUint32(10),
			deadlineExceeded: *atomic.NewUint32(4), // Ratio is 0.4, above threshold
		},
		currentDuration: *atomic.NewDuration(config.InitialTimeout),
		expiry:          *atomic.NewTime(time.Now().Add(config.Interval)),
		id:              config.Id,
		interval:        config.Interval,
		threshold:       config.Threshold,
		incBy:           config.IncBy,
		decBy:           config.DecBy,
		minimumCount:    config.MinimumCount,
	}

	// Test inc
	provider.inc()
	expectedTimeout := time.Duration(float32(config.InitialTimeout) * config.IncBy)
	assert.Equal(t, expectedTimeout, provider.currentDuration.Load(), "Timeout duration after inc should match")
}

// TestDec tests the inc methods of AdaptoProvider.
func TestDec(t *testing.T) {
	config := Config{
		Id:             "test3",
		Interval:       5 * time.Second,
		InitialTimeout: 1 * time.Second,
		Threshold:      0.3,
		IncBy:          1.5,
		DecBy:          200 * time.Millisecond,
		MinimumCount:   0,
	}

	provider := AdaptoProvider{
		counts: Counts{
			total:            *atomic.NewUint32(10),
			deadlineExceeded: *atomic.NewUint32(2), // Ratio is 0.2, below threshold
		},
		currentDuration: *atomic.NewDuration(config.InitialTimeout),
		expiry:          *atomic.NewTime(time.Now().Add(config.Interval)),
		id:              config.Id,
		interval:        config.Interval,
		threshold:       config.Threshold,
		incBy:           config.IncBy,
		decBy:           config.DecBy,
		minimumCount:    config.MinimumCount,
	}

	// Test dec
	provider.dec()
	expectedTimeout := config.InitialTimeout - config.DecBy
	assert.Equal(t, expectedTimeout, provider.currentDuration.Load(), "Timeout duration after dec should match")
}

func TestDynamicTimeoutAdjustmentNoTrheshold(t *testing.T) {
	// Create a new provider configuration
	decBy := 1 * time.Second
	var incBy float32 = 1.5
	config := Config{
		Id:             "dynamicTestWithOutThreshold",
		Interval:       30 * time.Second, // 2-second interval for threshold reset
		InitialTimeout: 10 * time.Second, // Start with a 1-second timeout
		Threshold:      0,                // Threshold ratio for adjusting timeout
		IncBy:          float32(incBy),   // Multiplicative increase factor
		DecBy:          decBy,            // Additive decrease amount
		MinimumCount:   0,                //Minimum number of generated timeouts to check the threshold
	}

	// Set up the provider & get initial timeout
	timeoutDuration, didDeadlineExceed, err := GetTimeout(config)
	time.Sleep(50 * time.Millisecond)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, config.InitialTimeout, timeoutDuration, "Initial timeout should match configuration")

	// simulate first success attempt
	didDeadlineExceed <- false
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, config.InitialTimeout-decBy, timeoutDuration, "timeout after first attempt should match")

	// simulate second success attempt
	didDeadlineExceed <- false
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, config.InitialTimeout-(decBy*2), timeoutDuration, "timeout after second attempt should match")

	// simulate first failed attempt
	didDeadlineExceed <- true
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, time.Duration(float32(config.InitialTimeout-(decBy*2))*incBy), timeoutDuration, "timeout after first failed attempt should match")
}

func TestDynamicTimeoutAdjustmentWithTrheshold(t *testing.T) {
	// Create a new provider configuration
	decBy := 1 * time.Second
	var incBy float32 = 1.5
	config := Config{
		Id:             "dynamicTestWithThreshold",
		Interval:       30 * time.Second, // 2-second interval for threshold reset
		InitialTimeout: 10 * time.Second, // Start with a 1-second timeout
		Threshold:      0.5,              // Threshold ratio for adjusting timeout
		IncBy:          float32(incBy),   // Multiplicative increase factor
		DecBy:          decBy,            // Additive decrease amount
		MinimumCount:   1,                //Minimum number of generated timeouts to check the threshold
	}

	// Set up the provider & get initial timeout
	timeoutDuration, didDeadlineExceed, err := GetTimeout(config)
	expected := config.InitialTimeout
	time.Sleep(50 * time.Millisecond)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, expected, timeoutDuration, "Initial timeout should match configuration")

	// simulate first success attempt
	didDeadlineExceed <- false
	expected = expected - decBy
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, expected, timeoutDuration, "timeout after first attempt should match")

	// simulate first failed attempt
	didDeadlineExceed <- true
	expected = time.Duration(float32(expected) * incBy)
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, expected, timeoutDuration, "timeout after first failed attempt should match")

	// simulate second success attempt
	didDeadlineExceed <- false
	expected = expected - decBy
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, expected, timeoutDuration, "timeout after second success attempt should match")

	// simulate third success attempt
	didDeadlineExceed <- false
	expected = expected - decBy
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, expected, timeoutDuration, "timeout after third success attempt should match")

	// simulate second failed attempt
	didDeadlineExceed <- true
	// should not update the timeout because it is smaller than the threshold
	/* expected = expected */
	time.Sleep(50 * time.Millisecond)
	timeoutDuration, didDeadlineExceed, err = GetTimeout(config)
	assert.NoError(t, err, "Unexpected error from GetTimeout")
	assert.Equal(t, expected, timeoutDuration, "timeout after second failed attempt should match")
}
