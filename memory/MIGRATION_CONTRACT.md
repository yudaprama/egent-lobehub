# User Memory Migration Contract

Status: Phase 0 baseline

Related: [../../USER_MEMORY_MIGRATION_PLAN.md](../../USER_MEMORY_MIGRATION_PLAN.md)

## Scope

This contract captures the current API surface for the LobeHub user-memory system before the TS backend is removed. It is the parity target for the Go + pREST replacement.

## Data domains

### Structured memory palace tables

- `user_memories`
- `user_memories_activities`
- `user_memories_contexts`
- `user_memories_experiences`
- `user_memories_identities`
- `user_memories_preferences`
- `user_memory_persona_documents`
- `user_memory_persona_document_histories`

### Runtime recall cache

- `egent-lobehub/memory/` (`MuninnStore` only; panic on startup if MuninnDB unreachable)
- Purpose: lightweight key/value recall injected into `AiAgentService` system prompt
- Not the source of truth for the structured `/memory` UI

---

## Router contract: `lambda/userMemory.ts`

Auth model:
- Read procedures: `wsCompatProcedure.use(serverDatabase)`
- Write procedures: read middleware + `withScopedPermission('message:create')`

Context models attached:
- `UserMemoryModel`
- `UserMemoryActivityModel`
- `UserMemoryContextModel`
- `UserMemoryExperienceModel`
- `UserMemoryIdentityModel`
- `UserMemoryPreferenceModel`
- `UserPersonaModel`
- `AsyncTaskModel`
- `TopicModel`

### Procedures

| Procedure | Kind | Input | Output | Current source |
|---|---|---|---|---|
| `createIdentity` | mutation | `CreateUserMemoryIdentitySchema` | identity create result from `userMemoryModel.addIdentityEntry` | TS model |
| `deleteActivity` | mutation | `{ id: string }` | delete result | `activityModel.delete` |
| `deleteAll` | mutation | none | `{ success: true }` | `userMemoryModel.deleteAll` + `personaModel.deletePersona` |
| `deleteContext` | mutation | `{ id: string }` | delete result | `contextModel.delete` |
| `deleteExperience` | mutation | `{ id: string }` | delete result | `experienceModel.delete` |
| `deleteIdentity` | mutation | `{ id: string }` | delete result | `userMemoryModel.removeIdentityEntry` |
| `deletePreference` | mutation | `{ id: string }` | delete result | `preferenceModel.delete` |
| `getActivities` | query | none | `searchActivities({})` result | `userMemoryModel.searchActivities` |
| `getContexts` | query | none | `searchContexts({})` result | `userMemoryModel.searchContexts` |
| `getExperiences` | query | none | `searchExperiences({})` result | `userMemoryModel.searchExperiences` |
| `getIdentities` | query | none | identity list | `userMemoryModel.getAllIdentities` |
| `getMemoryExtractionTask` | query | `{ taskId?: uuid }?` | `{ id, status, metadata, error } \| null` | `AsyncTaskModel` |
| `getPersona` | query | none | `{ content, summary } \| null` | latest persona doc |
| `getPreferences` | query | none | `searchPreferences({})` result | `userMemoryModel.searchPreferences` |
| `requestMemoryFromChatTopic` | mutation | `{ fromDate?: Date, toDate?: Date }` | `{ deduped, id, metadata, status }` | creates async task + triggers QStash workflow |
| `updateActivity` | mutation | `{ id: string, data: { narrative?, notes?, status? } }` | update result | `activityModel.update` |
| `updateContext` | mutation | `{ id: string, data: { currentStatus?, description?, title? } }` | update result | `contextModel.update` |
| `updateExperience` | mutation | `{ id: string, data: { action?, keyLearning?, situation? } }` | update result | `experienceModel.update` |
| `updateIdentity` | mutation | `{ id: string, data: UpdateUserMemoryIdentitySchema }` | update result | `userMemoryModel.updateIdentityEntry` |
| `updatePreference` | mutation | `{ id: string, data: { conclusionDirectives?, suggestions? } }` | update result | `preferenceModel.update` |

### Behavioral notes

- `requestMemoryFromChatTopic` dedupes if an active `AsyncTaskType.UserMemoryExtractionWithChatTopic` already exists.
- `getMemoryExtractionTask` force-marks tasks as `Error` on timeout using `5 minutes * totalTopics`.
- `getPersona` returns only `{ content: persona, summary: tagline }`, not the full row.

---

## Router contract: `lambda/userMemories.ts`

Auth model:
- Read procedures: `authedProcedure.use(serverDatabase)`
- Write procedures: read middleware + `withScopedPermission('message:create')`

Extra context:
- Reads `user_settings.memory.effort`
- Embedding provider/model from `getServerDefaultFilesConfig().embeddingModel || DEFAULT_USER_MEMORY_EMBEDDING_MODEL_ITEM`
- Runtime via `initModelRuntimeFromDB`

### Query procedures

| Procedure | Kind | Input | Output | Current source |
|---|---|---|---|---|
| `getMemoryDetail` | query | `{ id: string, layer: LayersEnum }` | memory detail row or `null` | `memoryModel.getMemoryDetail` |
| `queryActivities` | query | `{ order?, page?, pageSize?, q?, sort?, status?, tags?, types? }?` | paged activity list | `activityModel.queryList` |
| `queryExperiences` | query | `{ order?, page?, pageSize?, q?, sort?, tags?, types? }?` | paged experience list | `experienceModel.queryList` |
| `queryIdentities` | query | `{ order?, page?, pageSize?, q?, relationships?, sort?, tags?, types? }?` | paged identity list | `identityModel.queryList` |
| `queryIdentitiesForInjection` | query | `{ limit?: 1..100 }?` | identity list | `identityModel.queryForInjection` |
| `queryIdentityRoles` | query | `{ page?, size? }?` | `{ roles, tags }` on success; fallback same shape | `memoryModel.queryIdentityRoles` |
| `queryMemories` | query | `{ categories?, layer?, order?, page?, pageSize?, q?, sort?, status?, tags?, types? }?` | paged parent-memory list | `memoryModel.queryMemories` |
| `queryTags` | query | `{ layers?, page?, size? }?` | aggregated tags result | `memoryModel.queryTags` |
| `queryTaxonomyOptions` | query | `queryTaxonomyOptionsSchema?` | taxonomy lists | `memoryModel.queryTaxonomyOptions` |
| `retrieveMemoryForTopic` | query | `{ topicId: string }` | `SearchMemoryResult` | topic user-message concat → embeddings → `searchUserMemories` |
| `searchMemory` | query | `searchMemorySchema` | `SearchMemoryResult` | `searchUserMemories` |
| `toolSearchMemory` | query | `searchMemorySchema` | `SearchMemoryResult` | same as `searchMemory` |

### Write / tool procedures

| Procedure | Kind | Input | Output | Notes |
|---|---|---|---|---|
| `reEmbedMemories` | mutation | `{ concurrency?, endDate?, limit?, only?, startDate? }?` | per-table re-embed stats | Recomputes all pgvector columns |
| `toolAddActivityMemory` | mutation | `ActivityMemoryItemSchema` | `{ success, message, memoryId?, activityId? }` | creates parent + child + vectors |
| `toolAddContextMemory` | mutation | `ContextMemoryItemSchema` | `{ success, message, memoryId?, contextId? }` | creates parent + child + vectors |
| `toolAddExperienceMemory` | mutation | `ExperienceMemoryItemSchema` | `{ success, message, memoryId?, experienceId? }` | creates parent + child + vectors |
| `toolAddIdentityMemory` | mutation | `AddIdentityActionSchema` | `{ success, message, memoryId?, identityId? }` | creates parent + child + vectors |
| `toolAddPreferenceMemory` | mutation | `PreferenceMemoryItemSchema` | `{ success, message, memoryId?, preferenceId? }` | creates parent + child + vectors |
| `toolRemoveIdentityMemory` | mutation | `RemoveIdentityActionSchema` | `{ success, message, identityId?, reason? }` | soft contract: returns success=false if missing |
| `toolUpdateIdentityMemory` | mutation | `UpdateIdentityActionSchema` | `{ success, message, identityId? }` | supports merge strategy + selective re-embed |

### Search / embedding semantics

- `searchUserMemories` normalizes queries via `normalizeSearchMemoryParams`.
- Embeddings are 1024-d from `embedUserMemoryTexts`.
- `retrieveMemoryForTopic` builds a query from concatenated user messages (first 7000 chars) using `UserMemoryTopicRepository.getUserMessagesQueryForTopic`.
- `DEFAULT_SEARCH_USER_MEMORY_TOP_K` + `MEMORY_SEARCH_TOP_K_LIMITS` gate recall counts by effort (`low`, `medium`, `high`).

---

## Frontend service contract: `src/services/userMemory/`

### `memoryCRUDService`

Thin wrappers over `lambdaClient.userMemory.*`:
- `deleteAll()`
- `createIdentity(data)`
- `deleteIdentity(id)`
- `getIdentities()`
- `updateIdentity(id, data)`
- `deleteContext(id)` / `getContexts()` / `updateContext(id, data)`
- `deleteActivity(id)` / `getActivities()` / `updateActivity(id, data)`
- `deleteExperience(id)` / `getExperiences()` / `updateExperience(id, data)`
- `deletePreference(id)` / `getPreferences()` / `updatePreference(id, data)`

### `userMemoryService`

Mix of pREST direct CRUD and tRPC fallbacks:
- direct pREST insert path for `addActivityMemory`, `addContextMemory`, `addExperienceMemory`, `addIdentityMemory`, `addPreferenceMemory`
- direct pREST list/detail path for `getMemoryDetail`, `queryExperiences`, `queryActivities`, `queryIdentities`, `retrieveMemory`, `retrieveMemoryForTopic`, `searchMemory`, `queryTags`, `queryIdentityRoles`, `queryIdentitiesForInjection`, `queryMemories`
- tRPC fallback path for `getPersona`, `queryTaxonomyOptions`

This mix means the migration target must preserve both:
1. pREST result shapes already consumed by the frontend
2. tRPC result shapes still used where pREST coverage is incomplete

---

## Gaps to close before TS removal

1. ~~`prest.toml` scopes 7 tables but **not** `user_memory_persona_document_histories`.~~ All 8 tables already scoped (`prest.toml:452–494`).
2. Tier-2 SQL templates exist only for `userMemoriesByLayer` today; all other rich queries still live in TS.
3. Extraction + persona update are still QStash / TS workflow based.
4. The structured palace tables are still the UI source of truth; MuninnDB only backs simple runtime recall.

## pREST scoping audit

`grep -n 'user_memory' prest.toml` returns 8 hits, one per table. Coverage complete for Tier-1 flat CRUD.

---

## Phase 0 deliverables

- [x] Contract document committed
- [x] pREST scope decision for `user_memory_persona_document_histories` (no change needed)
- [ ] Snapshot fixture strategy defined (deferred to Phase 1 — reuse fixtures from contract tests)
- [ ] Read-template backlog extracted from this contract (do in Phase 1 kickoff)
