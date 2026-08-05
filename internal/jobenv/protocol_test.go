package jobenv

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTripPreservesOpaqueNonNULValues(t *testing.T) {
	want := []Item{{Name: "PUBLIC", Value: []byte("value")}, {Name: "SECRET", Value: []byte{'a', '\n', 0xff}}}
	raw, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got[1].Name != want[1].Name || !bytes.Equal(got[1].Value, want[1].Value) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

func TestDecodeFailsClosedOnMalformedAuthority(t *testing.T) {
	raw, err := Encode([]Item{{Name: "TOKEN", Value: []byte("secret")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "truncated", raw: raw[:len(raw)-1]},
		{name: "trailing", raw: append(append([]byte{}, raw...), 'x')},
		{name: "bad magic", raw: append([]byte("not-"), raw...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(test.raw)); err == nil {
				t.Fatal("malformed environment was accepted")
			}
		})
	}
	if _, err := Encode([]Item{{Name: "BAD=NAME", Value: nil}}); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("error=%v", err)
	}
}
