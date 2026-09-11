package neatlogs

// Version is the SDK version, reported as `service.version` on every span.
// Go modules take their version from the git tag, so this constant must be
// bumped in the commit tagged for the root module. Independently published
// contrib modules update their root requirement after that tag exists.
const Version = "0.1.9"
