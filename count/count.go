package count

import (
	"sync"
)

// Counts holds the numbers of context with timeout set.
// adapto clears the internal Counts when new context is created by adapto
// and when context created by adapto exceeds deadline.
type Counts struct {
	total            uint32
	deadlineExceeded uint32
	mu               sync.Mutex
}

func NewCounts() Counts {
	return Counts{
		total:            0,
		deadlineExceeded: 0,
	}
}

func (c *Counts) OnNew() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
}

func (c *Counts) OnDeadlineExceeded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlineExceeded++
}

func (c *Counts) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total = 0
	c.deadlineExceeded = 0
}

func (c *Counts) Total() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *Counts) DeadlineExceeded() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadlineExceeded
}

func (c *Counts) Ratio() float32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total == 0 {
		return 0
	}
	return float32(c.deadlineExceeded) / float32(c.total)
}
