package adapter

import "fmt"

// ExecutionClass maps a pipeline node's resource tier to a Kueue LocalQueue name.
// Corresponds to caleb's executionClass concept (pipeline_v12.jsonc).
type ExecutionClass string

const (
	ExecutionClassStandard ExecutionClass = "standard" // → poc-standard-lq
	ExecutionClassHighmem  ExecutionClass = "highmem"  // → poc-highmem-lq
)

// QueueLabel returns the Kueue label map for the given ExecutionClass.
// Use as RunSpec.Labels to route a job to the correct LocalQueue.
func (ec ExecutionClass) QueueLabel() map[string]string {
	return map[string]string{
		"kueue.x-k8s.io/queue-name": ec.localQueue(),
	}
}

func (ec ExecutionClass) localQueue() string {
	switch ec {
	case ExecutionClassStandard:
		return "poc-standard-lq"
	case ExecutionClassHighmem:
		return "poc-highmem-lq"
	default:
		panic(fmt.Sprintf("unknown ExecutionClass: %q", ec))
	}
}
