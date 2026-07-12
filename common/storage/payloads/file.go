package payloads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"common/logging"

	"github.com/productscience/inference/x/inference/types"
)

type storedPayload struct {
	PromptPayload   []byte `json:"prompt_payload"`
	ResponsePayload []byte `json:"response_payload"`
}

// FileStorage stores payloads as JSON files under
// {baseDir}/{epochId}/{escrowId}/{inferenceId}.json .
type FileStorage struct {
	baseDir string
}

func NewFileStorage(baseDir string) *FileStorage {
	return &FileStorage{baseDir: baseDir}
}

func (f *FileStorage) Store(ctx context.Context, escrowId string, inferenceId, epochId uint64, promptPayload, responsePayload []byte) error {
	_ = ctx
	logging.Debug("Storing payload (file)", types.PayloadStorage,
		"escrowId", escrowId, "inferenceId", inferenceId, "epochId", epochId, "baseDir", f.baseDir)

	dir := filepath.Join(f.baseDir, strconv.FormatUint(epochId, 10), escrowId)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("payloads: mkdir: %w", err)
	}

	data, err := json.Marshal(storedPayload{
		PromptPayload:   promptPayload,
		ResponsePayload: responsePayload,
	})
	if err != nil {
		return fmt.Errorf("payloads: marshal: %w", err)
	}

	targetPath := filepath.Join(dir, strconv.FormatUint(inferenceId, 10)+".json")
	tempPath := targetPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o644); err != nil {
		return fmt.Errorf("payloads: write temp: %w", err)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("payloads: rename: %w", err)
	}
	return nil
}

func (f *FileStorage) Retrieve(ctx context.Context, escrowId string, inferenceId, epochId uint64) ([]byte, []byte, error) {
	_ = ctx
	filePath := filepath.Join(f.baseDir, strconv.FormatUint(epochId, 10), escrowId, strconv.FormatUint(inferenceId, 10)+".json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("payloads: read file: %w", err)
	}

	var payload storedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, fmt.Errorf("payloads: unmarshal: %w", err)
	}
	return payload.PromptPayload, payload.ResponsePayload, nil
}

func (f *FileStorage) DropEpoch(ctx context.Context, epochId uint64) error {
	_ = ctx
	epochDir := filepath.Join(f.baseDir, strconv.FormatUint(epochId, 10))
	if err := os.RemoveAll(epochDir); err != nil {
		return fmt.Errorf("payloads: remove epoch dir: %w", err)
	}
	return nil
}

var _ Storage = (*FileStorage)(nil)
