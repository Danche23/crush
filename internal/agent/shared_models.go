package agent

import (
	"context"

	"github.com/charmbracelet/crush/internal/csync"
)

// sharedModels is the large and small model every agent of one kind sends
// requests through. Agents hold pointers into it rather than copies, so
// rebuilding it after a credential refresh reaches all of them at once,
// sub-agents included, and fantasy's ModelProvider hands the retry the new
// credential.
type sharedModels struct {
	large, small csync.Value[Model]
}

// rebuildModels rebuilds both model sets from the live config. Both are built
// before either is published, so a failure leaves the old pair in place.
func (c *coordinator) rebuildModels(ctx context.Context) error {
	large, small, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return err
	}
	subLarge, subSmall, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return err
	}
	c.models.large.Set(large)
	c.models.small.Set(small)
	c.subAgentModels.large.Set(subLarge)
	c.subAgentModels.small.Set(subSmall)
	return nil
}

// modelsFor returns the model set an agent of the given kind shares.
func (c *coordinator) modelsFor(isSubAgent bool) *sharedModels {
	if isSubAgent {
		return &c.subAgentModels
	}
	return &c.models
}

func orNoModel(model *csync.Value[Model]) *csync.Value[Model] {
	if model == nil {
		return csync.NewValue(Model{})
	}
	return model
}
