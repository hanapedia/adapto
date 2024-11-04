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

## Configuration parameters
- max: maximum timeout value allowed
- min: minimum timeout value allowed
- margin (K in the equation): multiplication factor applied for maximum deviation allowed. defaults to 4.
- backoff: multiplication factor applied to RTO when timeout occurs. defaults to 2.

Not included in the first iteration
- alpha: left for future adjustment
- beta: left for future adjustment


