# RTO Specs

## RTO equations
**before any measurement of RTT**
- RTO should be <- 1 second
**after RTT $R$ has been measured**
- $SRTT \leftarrow R$
- $RTTVAR \leftarrow \frac{R}{2}$
- $RTO \leftarrow SRTT + max(G, K \cdot RTTVAR)$ where $K = 4$
**when subsequent RTT $R'$ is measured**
- $RTTVAR \leftarrow (1 - \beta) \cdot RTTVAR + \beta \cdot |SRTT - R'|$
- $SRTT \leftarrow (1 - \alpha) \cdot SRTT + \alpha \cdot R'$
- $RTTVAR$ must be updated with old $SRTT$
- $\alpha = \frac{1}{8}$, $\beta = \frac{1}{4}$
	- according to "[Congestion Avoidance and Control](https://ee.lbl.gov/papers/congavoid.pdf)" by Jacobson(88'), $\alpha$ is chosen to be close to .1 suggested by RFC793, and for the ease of calculation as integer
- then that $RTO$ must be updated with 
	- $RTO \leftarrow SRTT + max(G, K \cdot RTTVAR)$ where $K = 4$
when RTO is calculated and it is less than 1 second, it should be rounded to 1 second in order to keep TCP conservative and avoid spurious retransmissions
**when Ack does not return before RTO expire**
- $RTO = 2 \cdot RTO$
- place maximum bound to constraint the backoff. (at least 60s)

## Overload control mechanism
When the RTT starts to rise due to server overload and more requests starts to timeout, the traditional RTO will try to minimize the number of timeouts by making the RTO longer. However, as explained by Little's law, lengthening the RTO will only make things worse by creating the feedback loop between the increasing RTT and increasing the number of inflight requests.

For this reason, at some point near the point of server overload, the client should start shortening the RTO to attempt to prevent (or remediate) the feedback loop.

This overload control mechanism will be implemented as following:

### Conservative increase of RTO duration based on current failure rate.
This version of RTO will monitor how many requests have failed in the interval. The interval for this calculation will be derived in two ways.
1. Number of requests sent. e.g. calculate failure rate every 50 requests
2. Predefined time interval. e.g. 1/2 of prometheus scrape interval

The assumption is that the overload happens due to the increase in the load, so periodic calculation of failure rate will not be soon enough. So the failure rate should be refreshed based on the load. When the load is stable, it is okay to be calculated at some interval. 

When updating the RTO value upon encoutering timeout, the previously computed failure rate should be referenced to decide how to update the RTO value. The WIP decision tree for RTO value update is as follows:
- if failure rate is **over the fraction (1/2) acceptable SLO target**, the RTO will be reduced to best case RTO where RTO is computed with *minimum RTT*. in order to conserve the latency target and hopefully remediate overload.

#### Parameters
- **SLO target failure rate**
- max timeout allowed. **this should be the max latency allowed by SLO**
- *failure rate calculation interval*
    - \*this interval should be longer than the max latency allowed.

## Dynamic Margin Determination
When the service is not under overload, this version of RTO will try to find the *margin* that will produce acceptable failure rate.
- RTO = srtt + margin * K * rttvar, (where K = 4, as defined by Jacobson)
- margin should be integer for simplicity. (for now) (could be left for future optimization)

## Dynamic capacity breach detection
When the load approaches the server capacity, the timeout duration should be shortened to reduce the queue length on the server, meaning that the timeout adjustment pattern changes based on whether the load is below or above the capacity.
Implementations up to v1.0.20 used static capacity parameter to set the threshold on requests per second, however, this approach is problamatic for few reasons.
- capacity can be dynamic in microservices due to continuous and independent scaling
- capacity cannot be client defined because a single client is not aware of other clients that access the same server
- even if there were some "true" capacity, knowing capacity in advance would require significant effort in load testing, which defeats the part of the purpose of adaptive timeout

Therefore, the capacity breach must be detected at runtime. An easy way to do this is to observe the past generated timeout and the failure rate. If the adaptive timeout has been elongated to the max duration allowed but the failure rate continues to grow, it could be an indicator that the load is past the capacity.

The detection step is outlined as follows:
- at *normal operating state*:
    - for each response or timeout event, new timeout is computed based on the latency or the timeout value.
        - both smoothed rtt and rto is tracked
    - also, for each *failure rate calculation interval*, kMargin is adjusted.
        - if failure rate requirement is not met, increment
        - else if multiply by (srto + srtt) / (2 * srto) and round down
    - if the max timeout value is given **once**, it enters *high load operating state*
        - when this happens, the *load (rps)* for the current *failure rate calculation interval* should be saved.
        - the rationale of using 1 timeout event of max timeout for detection should be discussed in the future.
- at *high load operating state*:
    - timeout is locked at the value computed by min latency.
    - at each *failure rate calculation interval*, current load should be checked to see if the load has subsided
        - kmargin should be locked

## Configuration parameters
```go
type Config struct {
	Id       string
	Max      time.Duration // max timeout value allowed
	Min      time.Duration // min timeout value allowed
	SLO      float64       // target failure rate SLO
	Interval time.Duration // interval for failure rate calculations
	KMargin  int64         // starting kMargin for with SLO and static kMargin for without SLO

	Logger logger.Logger // optional logger
}
```

