package buildinfo

// These values are overridden with -ldflags in release builds. Keeping useful
// development defaults preserves the AGPL source offer in local deployments.
var (
	Revision  = "unknown"
	SourceURL = "https://github.com/MysticRyuujin/enrscout"
)
