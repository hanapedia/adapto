// ring implements RingBuffer
package ring

// Numeric is a constraint that allows only numeric types (int, int64, float64, etc.)
type Numeric interface {
	~int | ~int64 | ~float64
}

// RingBuffer is a generic fixed-size circular buffer for numeric types.
type RingBuffer[T Numeric] struct {
	buffer []T
	size   int
	start  int
	count  int
}

// NewRingBuffer creates a new RingBuffer of the given size.
func NewRingBuffer[T Numeric](size int) *RingBuffer[T] {
	if size <= 0 {
		panic("size must be greater than 0")
	}
	return &RingBuffer[T]{
		buffer: make([]T, size),
		size:   size,
	}
}

// Add adds a new element to the buffer.
func (rb *RingBuffer[T]) Add(value T) {
	rb.buffer[(rb.start+rb.count)%rb.size] = value
	if rb.count < rb.size {
		rb.count++
	} else {
		rb.start = (rb.start + 1) % rb.size
	}
}

// GetAll retrieves all elements from the buffer in order.
func (rb *RingBuffer[T]) GetAll() []T {
	result := make([]T, rb.count)
	for i := 0; i < rb.count; i++ {
		result[i] = rb.buffer[(rb.start+i)%rb.size]
	}
	return result
}

// GetLast retrieves the last element from the buffer.
// If the buffer is empty, it returns the zero value of the type.
func (rb *RingBuffer[T]) GetLast() T {
	var zero T // Zero value for type T
	if rb.count == 0 {
		return zero // Return zero value if the buffer is empty
	}

	// Get the index of the last element
	lastIndex := (rb.start + rb.count - 1) % rb.size
	return rb.buffer[lastIndex]
}

// GetSecondLast retrieves the second-to-last element from the buffer.
// If the buffer has fewer than two elements, it returns the zero value of the type.
func (rb *RingBuffer[T]) GetSecondLast() T {
	var zero T // Zero value for type T
	if rb.count < 2 {
		return zero // Not enough elements to retrieve the second-to-last
	}

	// Get the index of the second-to-last element
	secondLastIndex := (rb.start + rb.count - 2) % rb.size
	return rb.buffer[secondLastIndex]
}

// AverageNonZero calculates the average of non-zero elements in the buffer.
func (rb *RingBuffer[T]) AverageNonZero() T {
	sum := T(0)
	nonZeroCount := 0

	for i := 0; i < rb.count; i++ {
		value := rb.buffer[(rb.start+i)%rb.size]
		if value != 0 {
			sum += value
			nonZeroCount++
		}
	}

	if nonZeroCount == 0 {
		return 0 // Avoid division by zero
	}
	return sum / T(nonZeroCount)
}
