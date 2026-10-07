package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestScaleRollingUpdateValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    intstr.IntOrString
		replicas int32
		roundUp  bool
		want     int
	}{
		{name: "int maxSurge", value: intstr.FromInt32(1), replicas: 2, roundUp: true, want: 1},
		{name: "int maxUnavailable", value: intstr.FromInt32(0), replicas: 2, roundUp: false, want: 0},
		{name: "25% maxSurge on 1 replica rounds up", value: intstr.FromString("25%"), replicas: 1, roundUp: true, want: 1},
		{name: "25% maxUnavailable on 1 replica rounds down", value: intstr.FromString("25%"), replicas: 1, roundUp: false, want: 0},
		{name: "25% maxUnavailable on 3 replicas rounds down", value: intstr.FromString("25%"), replicas: 3, roundUp: false, want: 0},
		{name: "25% maxUnavailable on 4 replicas", value: intstr.FromString("25%"), replicas: 4, roundUp: false, want: 1},
		{name: "0% maxSurge", value: intstr.FromString("0%"), replicas: 4, roundUp: true, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scaleRollingUpdateValue(&tt.value, tt.replicas, tt.roundUp)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScaleRollingUpdateValueInvalid(t *testing.T) {
	t.Parallel()

	value := intstr.FromString("abc")

	if _, err := scaleRollingUpdateValue(&value, 2, true); err == nil {
		t.Fatal("expected an error for a non-percentage string")
	}
}
