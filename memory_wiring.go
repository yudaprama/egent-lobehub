package main

import (
	"egent-lobehub/memory"

	"github.com/cloudwego/eino/components/tool"
)

// memoryTools returns the four user-memory tools backed by the given manager.
// In a multi-process deployment this should be backed by a persistent store
// (e.g. Postgres) instead of the default in-memory store.
func memoryTools(mgr *memory.Manager) []tool.BaseTool {
	return mgr.AllTools()
}
