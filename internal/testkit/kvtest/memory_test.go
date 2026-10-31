package kvtest

import "testing"

func TestMemoryPassesTheContract(t *testing.T) {
	Contract(t, func(*testing.T) Store { return new(Memory) })
}
