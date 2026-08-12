package cache

import (
	"reflect"
	"testing"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func TestMergeAndMissing(t *testing.T) {
	ranges := []model.ByteRange{{Start: 0, End: 100}, {Start: 200, End: 250}}
	got := Merge(ranges, model.ByteRange{Start: 90, End: 220})
	want := []model.ByteRange{{Start: 0, End: 250}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge: got %#v want %#v", got, want)
	}
	missing := Missing(300, got)
	if !reflect.DeepEqual(missing, []model.ByteRange{{Start: 250, End: 300}}) {
		t.Fatalf("missing: %#v", missing)
	}
}

func TestMergeNormalizesRanges(t *testing.T) {
	input := []model.ByteRange{
		{Start: 10, End: 20},
		{Start: -10, End: 5},
		{Start: 20, End: 30},
		{Start: 40, End: 40},
		{Start: 50, End: 45},
	}
	want := []model.ByteRange{{Start: 0, End: 5}, {Start: 10, End: 30}}
	if got := Merge(input, model.ByteRange{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	if input[1].Start != -10 {
		t.Fatal("Merge mutated its input")
	}
}

func TestMissingClampsToTotal(t *testing.T) {
	ranges := []model.ByteRange{{Start: -10, End: 5}, {Start: 10, End: 20}, {Start: 25, End: 100}}
	want := []model.ByteRange{{Start: 5, End: 10}, {Start: 20, End: 25}}
	if got := Missing(30, ranges); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	if got := Missing(0, ranges); got != nil {
		t.Fatalf("zero total: got %#v want nil", got)
	}
}

func TestParseSingleRange(t *testing.T) {
	for _, test := range []struct {
		header string
		want   model.ByteRange
	}{
		{"bytes=10-19", model.ByteRange{Start: 10, End: 20}},
		{"bytes=90-", model.ByteRange{Start: 90, End: 100}},
		{"bytes=-10", model.ByteRange{Start: 90, End: 100}},
		{"bytes=-200", model.ByteRange{Start: 0, End: 100}},
		{"bytes=90-200", model.ByteRange{Start: 90, End: 100}},
		{"bytes= 10-19 ", model.ByteRange{Start: 10, End: 20}},
	} {
		got, err := ParseSingleRange(test.header, 100)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: got %#v, %v", test.header, got, err)
		}
	}
	for _, header := range []string{
		"", "items=0-1", "Bytes=0-1", "bytes=0-1,4-5", "bytes=100-",
		"bytes=-0", "bytes=-", "bytes=20-10", "bytes=+1-2", "bytes=1--2",
		"bytes=1-2-3", "bytes=9223372036854775807-", " bytes=0-1",
	} {
		if _, err := ParseSingleRange(header, 100); err == nil {
			t.Fatalf("expected rejection for %q", header)
		}
	}
	if _, err := ParseSingleRange("bytes=0-0", 0); err == nil {
		t.Fatal("expected rejection for an empty representation")
	}
}
