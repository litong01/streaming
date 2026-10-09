package logbuf

import "testing"

func TestBufferKeepsTheNewestLines(t *testing.T) {
	buf := &buffer{max: 2}
	if _, err := buf.Write([]byte("one\ntwo\nthree\n")); err != nil {
		t.Fatal(err)
	}
	got := buf.lines()
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("lines %#v", got)
	}
	if _, err := buf.Write([]byte("four")); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	got = buf.lines()
	if len(got) != 2 || got[0] != "three" || got[1] != "four" {
		t.Fatalf("split write %#v", got)
	}
}
