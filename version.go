package neatlogs

// Version is the SDK version, reported as `service.version` on every span.
// Go modules take their version from the git tag, so this constant must be
// bumped in the same commit that is tagged (and matched by the
// `github.com/neatlogs/neatlogs-go` require in each contrib module's go.mod).
const Version = "0.1.7"
