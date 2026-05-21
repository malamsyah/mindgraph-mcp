package memory

import (
	"reflect"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty slice", []string{}, nil},
		{"lowercase + trim", []string{"  Foo ", "bar"}, []string{"foo", "bar"}},
		{"dedupe", []string{"x", "X", "x "}, []string{"x"}},
		{"drop empty", []string{"", "  ", "y"}, []string{"y"}},
		{"preserve first-occurrence order", []string{"b", "a", "B"}, []string{"b", "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeTags(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NormalizeTags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
