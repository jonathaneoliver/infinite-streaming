package plays

// Backend is the minimal ClickHouse handle the domain functions need.
// The forwarder's main package builds one from its `config` struct
// and passes it on every call.
//
// Kept tiny on purpose — anything beyond what's needed to address
// CH (URL, db, events table, auth) belongs in the call signature, not
// here.
type Backend struct {
	ClickHouseURL string
	Database      string
	// EventsTable is the per-snapshot table name. The schema rename in
	// #472 made the canonical name `session_events`; the field stays
	// configurable so old deploys / tests can override.
	EventsTable string
	User        string
	Password    string
}
