// Package adapters loads domain adapters from versioned manifest files.
//
// A manifest is JSON with an "adapter" name plus the adapter's own
// configuration; the named factory (registered by the adapter package
// compiled into the binary) parses the rest. The full SDK contract —
// registries, ontologies, check sets, hooks — extends this in stage 4;
// today the surface is the extraction adapter.
package adapters

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Factory builds an adapter from its manifest body (everything after the
// "adapter" key has been consumed).
type Factory func(raw json.RawMessage) (any, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes an adapter loadable by name. Call from adapter package
// init functions.
func Register(name string, factory Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[name] = factory
}

// LoadManifest reads a manifest file and dispatches to the named factory.
// The concrete type is the adapter's own; the CLI type-asserts to the
// interface it needs (extract.Adapter today).
func LoadManifest(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("adapters: read manifest: %w", err)
	}
	var head struct {
		Adapter string `json:"adapter"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("adapters: parse manifest: %w", err)
	}
	if head.Adapter == "" {
		return nil, fmt.Errorf("adapters: manifest %s has no \"adapter\" field", path)
	}
	mu.RLock()
	factory, ok := factories[head.Adapter]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("adapters: unknown adapter %q (registered adapters are compiled in)", head.Adapter)
	}
	return factory(json.RawMessage(raw))
}
