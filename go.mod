module github.com/amit-timalsina/pi-agent-go

go 1.25.0

require (
	github.com/amit-timalsina/pi-llm-go v0.2.0
	github.com/invopop/jsonschema v0.14.0
	golang.org/x/sync v0.20.0
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
)

// v0.7.1 shipped with internal product identifiers in CHANGELOG +
// agent.go comments that should not appear in this OSS repo. v0.7.2
// reships the same nil-block fix with generic descriptors; please
// upgrade.
retract v0.7.1
