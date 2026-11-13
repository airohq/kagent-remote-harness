// Package harness is the kagent side of the remote harness: a BYO harness
// that sends each turn to a runner and turns the runner's events into
// kagent's.
package harness

import (
	"context"
	"encoding/json"
	"fmt"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/bootstrap"
	kagentconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	contract "github.com/kagent-dev/kagent/go/api/bootstrap"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
)

// LoadBundle reads the agent config and card from kagent's bootstrap bundle,
// fetching it into dir on first start. The bundle holds the system prompt,
// the model from the Agent's ModelConfig, and the kagent controller that
// stores the A2A tasks.
func LoadBundle(ctx context.Context, getenv func(string) string, dir string) (*configv1.ADKConfig, a2atype.AgentCard, error) {
	var card a2atype.AgentCard
	bundle, err := bootstrap.Load(ctx, getenv(contract.Environment), dir, getenv("SSL_CERT_FILE"))
	if err != nil {
		return nil, card, fmt.Errorf("load the bootstrap bundle: %w", err)
	}
	var cfg configv1.ADKConfig
	if err := kagentconfig.Unmarshal(bundle.ConfigJson, &cfg); err != nil {
		return nil, card, fmt.Errorf("decode the agent config: %w", err)
	}
	if err := json.Unmarshal(bundle.AgentCardJson, &card); err != nil || card.Name == "" {
		return nil, card, fmt.Errorf("the bootstrap bundle needs an agent card with a name: %v", err)
	}
	return &cfg, card, nil
}
