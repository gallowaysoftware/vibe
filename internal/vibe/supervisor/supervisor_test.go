package supervisor

import (
	"reflect"
	"testing"
)

func TestRingBuffer_OverflowKeepsRecent(t *testing.T) {
	rb := newRingBuffer(3)
	rb.Add("a")
	rb.Add("b")
	if got := rb.Lines(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
	rb.Add("c")
	rb.Add("d")
	if got := rb.Lines(); !reflect.DeepEqual(got, []string{"b", "c", "d"}) {
		t.Errorf("got %v", got)
	}
	rb.Reset()
	if got := rb.Lines(); len(got) != 0 {
		t.Errorf("after reset got %v", got)
	}
}
