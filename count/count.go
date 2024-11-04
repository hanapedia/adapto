package count

import (
	"go.uber.org/atomic"
)

// Counts holds the numbers of context with timeout set.
// adapto clears the internal Counts when new context is created by adapto
// and when context created by adapto exceeds deadline.
type Counts struct {
	total            atomic.Uint32
	deadlineExceeded atomic.Uint32
}

func NewCounts() Counts {
	return Counts{
		total:            *atomic.NewUint32(0),
		deadlineExceeded: *atomic.NewUint32(0),
	}
}

func (c *Counts) OnNew() {
	c.total.Add(1)
}

func (c *Counts) OnDeadlineExceeded() {
	c.deadlineExceeded.Add(1)
}

func (c *Counts) Clear() {
	c.total.Store(0)
	c.deadlineExceeded.Store(0)
}

func (c *Counts) Total() uint32 {
	return c.total.Load()
}

func (c *Counts) DeadlineExceeded() uint32 {
	return c.deadlineExceeded.Load()
}

func (c *Counts) Ratio() float32 {
	total := c.total.Load()
	de := c.deadlineExceeded.Load()
	return float32(de) / float32(total)
}
