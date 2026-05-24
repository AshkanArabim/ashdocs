# Ashdocs
A basic real-time collaborative text-editor, written in Golang, with a React + TypeScript + Vite frontend.

## Video demo
https://youtu.be/KKk57I-RaHg

## Architecture
<img width="850" height="525" alt="2026-01-11_20-31-04" src="https://github.com/user-attachments/assets/735db826-e1ae-4376-924f-89f20daa3bfd" />

- **Frontend:** React + TS; handles the UI and sends/receives changes over WebSockets.
- **Backend:** (what you see in the sequence diagram above) Implemented in Go, using WebSockets to broadcast document updates between clients.
  - Document editing logic, including conflict resolution, incremental saving, and changelog-based reconstruction are handled by the [Automerge](https://github.com/automerge/automerge-go) library.
- **Storage Layer:** A simple storage subsystem interface in Go abstracts document persistence.
  - Currently, there's only one storage subsystem, and it's based on [Pebble](https://github.com/cockroachdb/pebble) which is a basic key-value store. Feel free to implement your own storage subsystem (e.g. based on Redis) if needed.

Note: The higher level [automerge-repo](https://github.com/automerge/automerge-repo) library, which greatly simplifies a project like this by abstracting persistence, broadcasting, and snapshotting, only exists for JS and Swift at the time of writing. For this project, parts of that library were re-implemented in Golang.

## Key Features

- Real-time collaborative text editing between multiple clients
- CRDT-based conflict-free merging
- Uses Automerge-Go for state management and operational transforms
- Modular backend allowing easy adaptation of different storage backends (see `backend/storage-subsystem-interface.go`)

## Performance

Benchmarked on Apple M4, single backend process. Run with `make bench` (requires backend running).

| Scenario | Max error-free throughput |
|----------|--------------------------|
| Single doc, increasing writers (10 edits/sec each) | **~40 edits/sec** (4 users) |
| Single doc, 25 users, increasing edit rate | **~125 edits/sec** (5 edits/sec/user) |
| Many docs, 1–2 writers each (10 edits/sec each) | **~20 edits/sec** (1 doc) |

## Future goals
- Implement "snapshots" to speed up reconstruction of documents with a very large edit history
- Improve the scalability and persistence features of the backend storage subsystem (for multi-server deployment)
- Expand this project to eventually do a full port of [automerge-repo](https://github.com/automerge/automerge-repo) in Golang.
- Add user authentication and document access control for multi-user environments
- Cursor position streaming, similar to Google Docs
- Docker containerization

## Getting Started

See the client and backend directories for README and setup instructions per component.
