package meter

import (
	"context"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util/templates"
)

func init() {
	registry.AddCtx("template", NewMeterFromTemplateConfig)
}

func NewMeterFromTemplateConfig(ctx context.Context, other map[string]any) (api.Meter, error) {
	instance, err := templates.RenderInstanceWithContext(ctx, templates.Meter, other)
	if err != nil {
		return nil, err
	}

	// Start refreshable parameters if present (also removes __refreshable_params from instance.Other)
	refreshParams, err := templates.StartRefreshableParams(ctx, instance)
	if err != nil {
		return nil, err
	}

	// Create base meter
	meter, err := NewFromConfig(ctx, instance.Type, instance.Other)
	if err != nil {
		return nil, err
	}

	// If there are refreshable params, wrap the meter with lifecycle management
	if refreshParams != nil {
		return NewRefreshableMeter(ctx, meter, refreshParams)
	}

	return meter, nil
}
