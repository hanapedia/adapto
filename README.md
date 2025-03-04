adapto
=========

<!-- [![GoDoc](https://godoc.org/github.com/hanapedia/adapto?status.svg)](https://godoc.org/github.com/hanapedia/adapto) -->

adapto implements the adaptive timeout in Go.

With adapto, timeout values are dynamically adjusted to meet the SLO requirement provided by the user.

Installation
------------

```
go get github.com/hanapedia/adapto
```

Usage
---
```go
import (
    "context"
    "fmt"
    "time"

    "github.com/hanapedia/adapto/rto"
)

func main() {
	config := rto.Config{
		Id:             "uniqueIdentifier",
		Min:            time.Duration(0),
		SLOLatency:     time.Second,
		SLOFailureRate: 0.01,
		Interval:       5 * time.Second,
		OnIntervalHandler: func(a rto.AdaptoRTOProviderInterface) {
            // operation to run every interval
            fmt.Println(a.State())
		},
	}
    timeoutDuration, rttCh, err := rto.GetTimeout(ctx, adaptoRTOConfig)
    if err != nil {
        // handle client queueing errors
        if err == rto.RequestRateLimitExceeded || err == context.Canceled || err == context.DeadlineExceeded {
            // report as generic error
            rttCh <- rto.RttSignal{Duration: timeoutDuration, Type: rto.GenericError}
            return secondary.SecondaryPortCallResult{
                Payload: nil,
                Error:   err,
            }
        }
        fmt.Println("failed to create new adaptive RTO timeout. resorting to default call timeout.")
        timeoutDuration = time.Second
    }

    // use the generated timeout duration for context
    ctx, callCancel := context.WithTimeout(context.Background(), timeoutDuration)
    defer callCancel()

    startTime := time.Now() // Record startTime for each opeation to record Response Time

    // Run Time taking operation such as calling remote API with context.
    result := SomeRemoteCall(ctx)

    // Check for cancelation or timeout after `next` returns
    if newCtx.Err() == context.DeadlineExceeded {
        // must report timeout error. Timeout Duration is recorded as the response time.
        rttCh <- rto.RttSignal{Duration: timeoutDuration, Type: rto.TimeoutError}
    } else if result.Error != nil {
        // report generic error
        rttCh <- rto.RttSignal{Duration: time.Since(startTime), Type: rto.GenericError}
    } else if result.Error == nil {
        // report Response Time
        rttCh <- rto.RttSignal{Duration: time.Since(startTime), Type: rto.Successful}
    }

    // Do whatever with the result of the operation.
}
```
