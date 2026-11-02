package app

import (
	"slices"
	"testing"
)

func TestEnableSelectsComponentsInStartOrder(t *testing.T) {
	for _, tc := range []struct {
		names, want []string
	}{
		{nil, ComponentNames()},
		{[]string{"gateway", "recovery", "dispatcher", "flusher", "pipeline", "kb-learner"}, ComponentNames()},
		{[]string{"pipeline", "kb-learner", "pipeline"}, []string{"kb-learner", "pipeline"}},
		{[]string{"kb-learner"}, []string{"kb-learner"}},
		{[]string{"dispatcher", "flusher", "pipeline"}, []string{"pipeline", "flusher", "dispatcher"}},
	} {
		got, err := enable(tc.names)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("enable(%q) = %q, %v; want %q", tc.names, got, err, tc.want)
		}
	}
}

func TestEnableReportsUnknownNamesAndEveryMissingConsumer(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"http", "pipeline", "websocket"}, `MELINDA_COMPONENTS: unknown component "http" (available: kb-learner, pipeline, flusher, dispatcher, recovery, gateway)` + "\n" +
			`MELINDA_COMPONENTS: unknown component "websocket" (available: kb-learner, pipeline, flusher, dispatcher, recovery, gateway)`},
		{[]string{"gateway"}, "MELINDA_COMPONENTS: gateway needs dispatcher\nMELINDA_COMPONENTS: dispatcher needs flusher\nMELINDA_COMPONENTS: flusher needs pipeline"},
		{[]string{"recovery", "gateway", "flusher", "pipeline"}, "MELINDA_COMPONENTS: gateway needs dispatcher\nMELINDA_COMPONENTS: recovery needs dispatcher"},
		{[]string{"dispatcher", "pipeline"}, "MELINDA_COMPONENTS: dispatcher needs flusher"},
	} {
		got, err := enable(tc.names)
		if err == nil || err.Error() != tc.want {
			t.Errorf("enable(%q) = %q, %v; want error %q", tc.names, got, err, tc.want)
		}
	}
}
