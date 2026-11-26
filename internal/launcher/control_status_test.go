package launcher

import (
	"reflect"
	"testing"

	"github.com/xynolog/sdk/go/api"
)

func TestWorkloadStatusUsesHealthyServicesAndAvailableReadings(t *testing.T) {
	completed, failed := 10, 3
	service := func(role string, healthy bool) api.ServiceStatus {
		return api.ServiceStatus{Role: role, Healthy: healthy}
	}

	tests := []struct {
		name     string
		services []api.ServiceStatus
		readout  string
		want     api.WorkloadStatus
	}{
		{
			name:     "gateway and dispatcher",
			services: []api.ServiceStatus{service("gateway", true), service("dispatcher", true)},
			readout:  sampleReadout,
			want: api.WorkloadStatus{
				Ledger: &api.WorkloadStatusLedger{Claims: 7, Capacity: 4096},
				Queue:  &api.WorkloadStatusQueue{Pending: 3, Running: 2, Completed: &completed, Failed: &failed},
			},
		},
		{
			name:     "gateway only",
			services: []api.ServiceStatus{service("gateway", true), service("dispatcher", false)},
			readout:  sampleReadout,
			want:     api.WorkloadStatus{Ledger: &api.WorkloadStatusLedger{Claims: 7, Capacity: 4096}},
		},
		{
			name:     "dispatcher without outcome counters",
			services: []api.ServiceStatus{service("dispatcher", true)},
			readout: `xynolog_job_queue_size{job="xynolog-dispatcher",job_status="pending",xynolog_kb="` + kbA + `"} 2
xynolog_job_queue_size{job="xynolog-dispatcher",job_status="running",xynolog_kb="` + kbA + `"} 1`,
			want: api.WorkloadStatus{Queue: &api.WorkloadStatusQueue{Pending: 2, Running: 1}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readout := &lazyBody{body: test.readout, fetched: true}
			got := workloadStatus(stackView{}, kbA, test.services, readout)
			if !reflect.DeepEqual(got, &test.want) {
				t.Errorf("workload status = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestWorkloadStatusOmitsUnavailableReadings(t *testing.T) {
	service := func(role string, healthy bool) api.ServiceStatus {
		return api.ServiceStatus{Role: role, Healthy: healthy}
	}

	t.Run("no healthy reporting service does not read collector", func(t *testing.T) {
		readout := &lazyBody{body: sampleReadout}
		got := workloadStatus(stackView{}, kbA, []api.ServiceStatus{service("gateway", false), service("dispatcher", false)}, readout)
		if got != nil {
			t.Fatalf("workload status = %#v, want nil", got)
		}
		if readout.fetched {
			t.Fatal("read collector despite neither service being healthy")
		}
	})

	t.Run("status adapter handles no services", func(t *testing.T) {
		if got := workloadStatus(stackView{}, kbA, nil, &lazyBody{}); got != nil {
			t.Fatalf("workload status = %#v, want nil", got)
		}
	})

	t.Run("healthy services without readings", func(t *testing.T) {
		got := workloadStatus(stackView{}, kbA, []api.ServiceStatus{service("gateway", true), service("dispatcher", true)}, &lazyBody{fetched: true})
		if got != nil {
			t.Fatalf("workload status = %#v, want nil", got)
		}
	})
}
