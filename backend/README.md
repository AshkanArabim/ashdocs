endpoints:
- connect to a document: `/document/id`

# persistence
Based on how automerge-repo's storage system and storage adapters work:
- internal StorageSubsystem handles the logic
- storage adapters are extremely dumb. used only for key-value storage / retrieval
- snapshot strategy: whenever diffs are larger than the snapshot
- periodic save: throttled (min 100ms gap default), triggered by every change to doc
- uses `savesince` for incremental save (I'll use the equivalent func in `automerge-go`, probably `saveIncremental`)
- automerge-repo has NO STORAGE ADAPTERS WITH PROPER DB BACKENDS. all just key-value stores. I'll use **Pebble DB**.
- stored changes tracked in `storedHeads` as "doc -> heads" map
- storage keys are hierarchical: `[<docId>, "snapshot"/"incremental", <hash>]`
  - e.g. querying `[<docId>, "incremental"]` gives instant access to all incremental saves for quick deletion on snapshot creation
