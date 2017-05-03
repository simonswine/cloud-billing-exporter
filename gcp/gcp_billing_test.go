package gcp

import (
	"reflect"
	"testing"
	"time"
)

type fakeClock struct {
	Time time.Time
}

func (f fakeClock) Now() time.Time {
	return f.Time
}

func Test_FilterLastTwoMonths(t *testing.T) {
	inputs := []string{
		"2017-01-02",
		"2016-12-02",
		"2016-07-02",
		"2017-05-03",
	}

	outputs := [][]string{
		[]string{
			"prefix-2017-01-",
			"prefix-2016-12-",
		},
		[]string{
			"prefix-2016-12-",
			"prefix-2016-11-",
		},
		[]string{
			"prefix-2016-07-",
			"prefix-2016-06-",
		},
		[]string{
			"prefix-2017-05-",
			"prefix-2017-04-",
		},
	}

	fakeTime := &fakeClock{}
	g := GCPBilling{
		time:         fakeTime,
		ReportPrefix: "prefix",
	}

	for i, _ := range inputs {

		now, err := time.Parse(DateFormat, inputs[i])
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		fakeTime.Time = now

		filters := g.filterLastTwoMonths()
		if len(filters) != len(outputs[i]) {
			t.Errorf("unexpected number of return values: act: %d, exp: %d", len(filters), len(outputs[i]))
		}

		if !reflect.DeepEqual(filters, outputs[i]) {
			t.Errorf("unexpected return value: act: %+v, exp: %+v", filters, outputs[i])
		}
	}

}
