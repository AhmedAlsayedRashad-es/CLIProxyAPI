package clientusage

import (
	"context"
	"strings"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// PluginName is the name the plugin registers under on the default usage manager.
const PluginName = "client-key-usage"

func init() {
	coreusage.RegisterNamedPlugin(PluginName, &usagePlugin{store: defaultStore})
}

type usagePlugin struct {
	store *Store
}

// HandleUsage adds one usage record to the store. It follows the
// usage-statistics-enabled switch, like the usage queue.
func (p *usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.store == nil || !redisqueue.UsageStatisticsEnabled() {
		return
	}
	p.store.Add(EventFromRecord(ctx, record))
}

// EventFromRecord reduces a usage record to the fields the store counts.
func EventFromRecord(ctx context.Context, record coreusage.Record) Event {
	apiKey := strings.TrimSpace(record.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(coreusage.APIKeyFromContext(ctx))
	}
	model := strings.TrimSpace(record.Alias)
	if model == "" {
		model = strings.TrimSpace(record.Model)
	}
	failed := record.Failed
	if !failed {
		if status := internallogging.GetResponseStatus(ctx); status >= 400 {
			failed = true
		}
	}
	return Event{
		APIKey: apiKey,
		Model:  model,
		Failed: failed,
		At:     record.RequestedAt,
		Detail: coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType),
	}
}
