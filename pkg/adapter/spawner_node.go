// Package adapter bridges dag-go's Runnable interface with spawner's DriverK8s.
// SpawnerNode implements dag-go.Runnable: each node carries a RunSpec and executes
// it as a K8s Job via spawner when the DAG calls RunE.
package adapter

import (
	"context"
	"fmt"

	daggo "github.com/seoyhaein/dag-go"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
)

// SpawnerNode implements dag-go.Runnable.
// One SpawnerNode per DAG node; it submits a K8s Job and waits for completion.
type SpawnerNode struct {
	Spec   api.RunSpec
	Driver *imp.DriverK8s
}

var _ daggo.Runnable = (*SpawnerNode)(nil)

// RunE is called by the dag-go engine when this node is scheduled to run.
// It executes Prepare → Start → Wait on the spawner K8s driver.
// Returns nil on success, non-nil error on failure (dag-go propagates failure to children).
func (s *SpawnerNode) RunE(ctx context.Context, _ interface{}) error {
	prepared, err := s.Driver.Prepare(ctx, s.Spec)
	if err != nil {
		return fmt.Errorf("spawner prepare %s: %w", s.Spec.RunID, err)
	}

	handle, err := s.Driver.Start(ctx, prepared)
	if err != nil {
		return fmt.Errorf("spawner start %s: %w", s.Spec.RunID, err)
	}

	event, err := s.Driver.Wait(ctx, handle)
	if err != nil {
		return fmt.Errorf("spawner wait %s: %w", s.Spec.RunID, err)
	}

	if event.State != api.StateSucceeded {
		return fmt.Errorf("job %s finished with state=%s: %s", s.Spec.RunID, event.State, event.Message)
	}
	return nil
}
