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
- if failure rate is **over the acceptable SLO target**, the RTO will be reduced to best case RTO where RTO is computed with *minimum RTT*. in order to conserve the latency target and hopefully remediate overload.
- if failrue rate is **over some fraction of SLO target**, the RTO will be incremented conservatively.
- if failure rate is **in the safe range**, the RTO will be doubled as normal. (and possibly request is retried(WIP))

#### Parameters
- Number of requests to calculate the new failure rate
    - this could be set dynamically based on capacity estimation (in the future work)
- Fallback time interval to calculate the new failure rate
- **SLO target failure rate**
- Safe range defined by Fraction of SLO target*


## Configuration parameters
- max: maximum timeout value allowed
- min: minimum timeout value allowed
- margin (K in the equation): multiplication factor applied for maximum deviation allowed. defaults to 4.
- backoff: multiplication factor applied to RTO when timeout occurs. defaults to 2.

Not included in the first iteration
- alpha: left for future adjustment
- beta: left for future adjustment


