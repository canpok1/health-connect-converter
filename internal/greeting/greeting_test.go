package greeting

import "testing"

func TestMessage(t *testing.T) {
	got := Message()
	want := "Hello, World!"
	if got != want {
		t.Errorf("Message() = %q, want %q", got, want)
	}
}
