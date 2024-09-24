adapto
=========

<!-- [![GoDoc](https://godoc.org/github.com/hanapedia/adapto?status.svg)](https://godoc.org/github.com/hanapedia/adapto) -->

adapto implements the adaptive timeout in Go.

With adapto, timeout values are dynamically adjusted similar to how additive-increase/multiplicative-decrease (AIMD) algorithm is used in TCP for congestion control.

Installation
------------

```
go get github.com/hanapedia/adapto
```

Usage
-----
```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/hanapedia/adapto"
)

func main() {
	config := adapto.Config{
		Id:             "example",              // Unique ID for the provider
		Interval:       5 * time.Second,        // Interval for resetting counts
		InitialTimeout: 2 * time.Second,        // Starting timeout duration
		Threshold:      0.5,                    // Ratio threshold for triggering adjustments
		IncBy:          1.5,                    // Multiplicative increase factor for timeouts
		DecBy:          200 * time.Millisecond, // Additive decrease amount for timeouts
		MinimumCount:   0,                      // Minimum number of generated timeouts to check the threshold
		Min:            time.Millisecond,       // Minimum timeout duration allowed
		Max:            time.Millisecond,       // Maximum timeout duration allowed. uses InitialTimeout if not set.
	}

    // GetTimeout returns the following
    // - adjusted timeout duration: time.Duration
    // - send only boolean channel to notify whether deadline was exceeded: chan<- bool
    // - err if there is error in config: error
    timeoutDuration, didDeadlineExceed, err := adapto.GetTimeout(config)
    // returns error when threshold is set greater or equal to 1 or negative
    // returns error when IncBy is smaller than or equal to 1
    // returns error when Min is smaller than or equal to 1
    if err != nil {
        panic(err)
    }

    ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
    defer cancel()

    fmt.Printf("Iteration %d: Timeout duration: %v\n", i+1, timeoutDuration)

    // execute some code with context
    go SomeFunc(ctx)
    go func() {
        select {
        case <-ctx.Done():
            // Check if the context's deadline was exceeded
            if ctx.Err() == context.DeadlineExceeded {
                didDeadlineExceed <- true
            } else {
                didDeadlineExceed <- false
            }
        }
    }()
}
```
