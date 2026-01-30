package broker

import (
	"os"
	"strings"

	"decentralized-api/apiconfig"
)

// Default enforced model for v0.2.9 upgrade
const defaultEnforcedModelId = "Qwen/Qwen3-4B-Instruct-2507"

var defaultEnforcedModelArgs = []string{
	"--enable-auto-tool-choice",
	"--tool-call-parser",
	"hermes",
	"--max-model-len",
	"15000",
}

// getEnforcedModel returns the enforced model ID and args from env vars or defaults.
// Returns empty string if enforcement is disabled (ENFORCED_MODEL_ID="").
func getEnforcedModel() (string, []string) {
	modelId := os.Getenv("ENFORCED_MODEL_ID")
	if modelId == "" {
		modelId = defaultEnforcedModelId
	}
	if modelId == "none" || modelId == "disabled" {
		return "", nil // enforcement disabled
	}

	argsStr := os.Getenv("ENFORCED_MODEL_ARGS")
	var args []string
	if argsStr != "" {
		args = strings.Fields(argsStr)
	} else {
		args = defaultEnforcedModelArgs
	}
	return modelId, args
}

// EnforceModel sets the enforced model if node doesn't already have it.
// Does nothing if enforcement is disabled or node already has the required model ID.
func EnforceModel(node *apiconfig.InferenceNodeConfig) {
	modelId, args := getEnforcedModel()
	if modelId == "" {
		return // enforcement disabled
	}
	if _, ok := node.Models[modelId]; ok {
		return // node already has required model ID
	}
	node.Models = map[string]apiconfig.ModelConfig{
		modelId: {Args: args},
	}
}
