package deliver

import (
	"encoding/json"
	"fmt"
)

// digestIsEmpty reads the `empty` flag out of the structured digest.
//
// The flag is read rather than inferred from an item count: ADR-0002 makes
// "empty" an explicit statement the aggregator writes, and re-deriving it here
// would let the two disagree about what happened.
func digestIsEmpty(structured []byte) (bool, error) {
	var probe struct {
		Empty *bool `json:"empty"`
	}
	if err := json.Unmarshal(structured, &probe); err != nil {
		return false, fmt.Errorf("structured digest is not valid JSON: %w", err)
	}
	if probe.Empty == nil {
		return false, fmt.Errorf("structured digest has no `empty` field: it is what tells a quiet day apart from a broken one")
	}
	return *probe.Empty, nil
}
