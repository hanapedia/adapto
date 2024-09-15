# adapto
adapto implements adaptive timeout for context.Context in Go. 
The timeout value is updated based on the pre-defined threshold on number context that timed out during an interval.

## Design
- no mutex. only atomic transaction
- ability to share the threshold check pool freely
