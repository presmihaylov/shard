package sandbox

// SnapshotRequest names the stopped sandbox a snapshot copies. It is the JSON body of POST /v0/snapshots.
type SnapshotRequest struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name,omitempty"`
}
