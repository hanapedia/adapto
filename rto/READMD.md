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
- Capacity (static for now) in Requests per second
    - N: Number of requests to calculate the new failure rate
    - T: Fallback time interval to calculate the new failure rate. could be given by the users. sane default is some fraction of metrics scrape interval by external telemetry
    - capacity = N / T
    - these could be set dynamically based on capacity estimation (in the future work)
    - e.g. Capacity = 150rps, T = 5s (1/3 prometheus scrape interval), N = 750
- **SLO target failure rate**
- Safe range defined by Fraction of SLO target* could be defined by the users as their preference

## Dynamic Margin Determination
When the service is not under overload, this version of RTO will try to find the *margin* that will produce acceptable failure rate.
- RTO = srtt + margin * K * rttvar, (where K = 4, as defined by Jacobson)
- margin should be integer for simplicity. (for now) (could be left for future optimization)

## When latency needs to be prioritized
Given an SLO such as *X*% of carried requests must complete within *Y*ms. And it is more important than failure rate. e.g. services such as trading and real time communication apps where completing the task in time is more valuable than completing the task.

This can be implemented using 2 timeouts, soft and hard
- after *Y*ms and response has not been returned, it will check if there is enough room in the error budget, in other words, it checks if allowing this request to wait longer still complies with *X* of requests being complete within *Y*ms.
    - if it is ok, do not cancel the request and wait until hard timeout
    - if it is not ok, cancel the request 

## When failure rate needs to be prioritized 

## Configuration parameters
```go
type Config struct {
	Id       string
	Max      time.Duration // max timeout value allowed
	Min      time.Duration // min timeout value allowed
	SLO      float64       // target failure rate SLO
	Capacity int64         // capacity given as requests per second
	Interval time.Duration // interval for failure rate calculations
	KMargin  int64         // starting kMargin for with SLO and static kMargin for without SLO

	Logger logger.Logger // optional logger
}
```

